package child

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/anggasct/aura/internal/platform/command"
)

type CommandDeps struct {
	Store     Store
	Canceller *CancelService
	Now       func() time.Time
}

type Opener func(cmd *cobra.Command, needsDurable bool, fn func(ctx context.Context, deps *CommandDeps) error) error

func Commands(open Opener) []*cobra.Command {
	cmd := &cobra.Command{
		Use:   "children",
		Short: "Inspect and manage durable child agent runs",
	}
	cmd.AddCommand(
		newListCmd(open),
		newShowCmd(open),
		newCancelCmd(open),
	)
	return []*cobra.Command{cmd}
}

func runWithOpener(cmd *cobra.Command, open Opener, needsDurable bool, fn func(ctx context.Context, deps *CommandDeps) error) error {
	if open == nil {
		return errNilArgument("opener")
	}
	return open(cmd, needsDurable, fn)
}

func writeCommandLines(cmd *cobra.Command, lines []string) error {
	out := cmd.OutOrStdout()
	for _, line := range lines {
		if _, err := fmt.Fprintln(out, line); err != nil {
			return err
		}
	}
	return nil
}

func formatCommandInt(v int64) string {
	return strconv.FormatInt(v, 10)
}

func commandNow(deps *CommandDeps) time.Time {
	if deps == nil || deps.Now == nil {
		return time.Now().UTC()
	}
	return deps.Now().UTC()
}

func newListCmd(open Opener) *cobra.Command {
	var state string
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List child runs",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if strings.TrimSpace(state) != "" && !ValidState(state) {
				return command.NewUsageError(errors.New("state must be one of queued, running, succeeded, failed, cancelled, deadline_exceeded, interrupted"))
			}
			return runWithOpener(cmd, open, false, func(ctx context.Context, deps *CommandDeps) error {
				if deps == nil || deps.Store == nil {
					return errNilArgument("store")
				}
				runs, err := deps.Store.List(ctx, state, 1000)
				if err != nil {
					return err
				}
				if len(runs) == 0 {
					return writeCommandLines(cmd, []string{"no children"})
				}
				lines := make([]string, 0, len(runs))
				for i := range runs {
					run := &runs[i]
					lines = append(lines, fmt.Sprintf("%s session=%s state=%s durable=%s deadline=%s parent=%s",
						run.ID, run.ChildSessionID, run.State, run.DurableKey,
						run.Deadline.UTC().Format("2006-01-02T15:04:05Z"), run.ParentInvocation))
				}
				return writeCommandLines(cmd, lines)
			})
		},
	}
	cmd.Flags().StringVar(&state, "state", "", "filter by state")
	return cmd
}

func newShowCmd(open Opener) *cobra.Command {
	return &cobra.Command{
		Use:   "show <child-id>",
		Short: "Show one child run with lineage",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runWithOpener(cmd, open, false, func(ctx context.Context, deps *CommandDeps) error {
				if deps == nil || deps.Store == nil {
					return errNilArgument("store")
				}
				run, found, err := deps.Store.GetRun(ctx, args[0])
				if err != nil {
					return err
				}
				if !found {
					return Errorf(ErrorCodeChildNotFound, "child is not found")
				}
				resultStatus := run.ResultStatus
				if resultStatus == "" {
					resultStatus = run.State
				}
				completedAt := ""
				if !run.CompletedAt.IsZero() {
					completedAt = run.CompletedAt.UTC().Format("2006-01-02T15:04:05Z")
				}
				return writeCommandLines(cmd, []string{
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
					"budget_usage: tokens=" + formatCommandInt(run.TokensUsed) + " cost=" + formatCommandInt(run.CostMicros),
					"result_status: " + resultStatus,
					"result_completed_at: " + completedAt,
					"result_provenance: " + run.ResultProvenance,
					"result_model: " + run.ResultModel,
					"result_trust: " + run.ResultTrust,
				})
			})
		},
	}
}

func newCancelCmd(open Opener) *cobra.Command {
	return &cobra.Command{
		Use:   "cancel <child-id>",
		Short: "Cancel a nonterminal child run",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runWithOpener(cmd, open, true, func(ctx context.Context, deps *CommandDeps) error {
				if deps == nil || deps.Canceller == nil {
					return errNilArgument("canceller")
				}
				state, err := deps.Canceller.Cancel(ctx, args[0], commandNow(deps))
				if err != nil {
					return err
				}
				return writeCommandLines(cmd, []string{"cancelled: " + args[0] + " state=" + state})
			})
		},
	}
}
