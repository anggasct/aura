package cli

import (
	"errors"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/anggasct/aura/internal/child"
	"github.com/anggasct/aura/internal/config"
	"github.com/anggasct/aura/internal/store"
)

func openChildStore(cmd *cobra.Command, gf *globalFlags) (store.ChildStore, func(), error) {
	result, err := config.Load(gf.configPath)
	if err != nil {
		return nil, nil, err
	}
	if result.Config.Children == nil || !result.Config.Children.Enabled {
		return nil, nil, errors.New("subagent runner is not enabled")
	}
	db, err := openStorage(cmd.Context(), result.Config)
	if err != nil {
		return nil, nil, err
	}
	closer := func() { _ = db.Close() }
	return store.NewChildStore(db), closer, nil
}

func writeChildLines(cmd *cobra.Command, lines []string) error {
	out := cmd.OutOrStdout()
	for _, line := range lines {
		if _, err := fmt.Fprintln(out, line); err != nil {
			return err
		}
	}
	return nil
}

func newChildrenListCmd(gf *globalFlags) *cobra.Command {
	var state string
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List child runs",
		RunE: func(cmd *cobra.Command, args []string) error {
			if strings.TrimSpace(state) != "" && !child.ValidState(state) {
				return &usageError{err: errors.New("state must be one of queued, running, succeeded, failed, cancelled, deadline_exceeded, interrupted")}
			}
			children, closer, err := openChildStore(cmd, gf)
			if err != nil {
				return err
			}
			defer closer()
			runs, err := children.List(cmd.Context(), state, 1000)
			if err != nil {
				return err
			}
			if len(runs) == 0 {
				return writeChildLines(cmd, []string{"no children"})
			}
			lines := make([]string, 0, len(runs))
			for i := range runs {
				run := &runs[i]
				lines = append(lines, fmt.Sprintf("%s session=%s state=%s durable=%s deadline=%s parent=%s",
					run.ID, run.ChildSessionID, run.State, run.DurableKey,
					run.Deadline.UTC().Format("2006-01-02T15:04:05Z"), run.ParentInvocation))
			}
			return writeChildLines(cmd, lines)
		},
	}
	cmd.Flags().StringVar(&state, "state", "", "filter by state")
	return cmd
}

func newChildrenShowCmd(gf *globalFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "show <child-id>",
		Short: "Show one child run with lineage",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			children, closer, err := openChildStore(cmd, gf)
			if err != nil {
				return err
			}
			defer closer()
			run, found, err := children.GetRun(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			if !found {
				return &exitCodeError{code: 1, err: errors.New("child_not_found: child is not found")}
			}
			return writeChildLines(cmd, []string{
				"id: " + run.ID,
				"child_session: " + run.ChildSessionID,
				"state: " + run.State,
				"durable_key: " + run.DurableKey,
				"deadline: " + run.Deadline.UTC().Format("2006-01-02T15:04:05Z"),
				"parent_session: " + run.ParentSessionID,
				"parent_turn: " + run.ParentTurnID,
				"parent_invocation: " + run.ParentInvocation,
				"context_digest: " + run.ContextDigest,
				"grants: " + run.GrantsJSON,
				"budget: " + run.BudgetJSON,
				"result_status: " + run.State,
				"result_provenance: child=" + run.ID + " session=" + run.ChildSessionID + " digest=" + run.ContextDigest + " durable=" + run.DurableKey,
			})
		},
	}
}

func newChildrenCancelCmd(gf *globalFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "cancel <child-id>",
		Short: "Cancel a nonterminal child run",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			canceller, closer, err := openChildCanceller(cmd, gf)
			if err != nil {
				return err
			}
			defer closer()
			state, err := canceller.Cancel(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			return writeChildLines(cmd, []string{"cancelled: " + args[0] + " state=" + state})
		},
	}
}
