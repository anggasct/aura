package workflow

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/anggasct/aura/internal/platform/approval"
	"github.com/anggasct/aura/internal/platform/durable"
	"github.com/anggasct/aura/internal/platform/principal"
)

type CommandMode int

const (
	CommandModeConfig CommandMode = iota
	CommandModeStorage
	CommandModeRuntime
)

type CommandDeps struct {
	DefinitionsDir string
	ValidationDeps func() (*ValidationDeps, error)
	Store          *Store
	Interpreter    *Interpreter
	Config         *Config
	Secrets        []string
	Now            func() time.Time
}

type CommandOpener func(cmd *cobra.Command, mode CommandMode, fn func(ctx context.Context, deps *CommandDeps) error) error

type commandUsageError struct {
	err error
}

func (e *commandUsageError) Error() string { return e.err.Error() }

func (e *commandUsageError) Unwrap() error { return e.err }

func (e *commandUsageError) IsUsageError() bool { return true }

func Commands(open CommandOpener) []*cobra.Command {
	cmd := &cobra.Command{
		Use:   "workflow",
		Short: "Validate and run declarative workflows",
	}
	cmd.AddCommand(
		workflowValidateCmd(open),
		workflowDefinitionsCmd(open),
		workflowStartCmd(open),
		workflowRunsCmd(open),
		workflowInspectCmd(open),
		workflowCancelCmd(open),
		workflowApproveCmd(open),
		workflowSignalCmd(open),
		workflowProposalsCmd(open),
		workflowProposalCmd(open),
	)
	return []*cobra.Command{cmd}
}

func runWithOpener(cmd *cobra.Command, open CommandOpener, mode CommandMode, fn func(ctx context.Context, deps *CommandDeps) error) error {
	if open == nil {
		return codedError(ErrorCodeInvalidArgument, "workflow command opener must not be nil")
	}
	return open(cmd, mode, fn)
}

func commandNow(deps *CommandDeps) time.Time {
	if deps == nil || deps.Now == nil {
		return time.Now()
	}
	return deps.Now()
}

func writeLines(cmd *cobra.Command, lines []string) error {
	out := cmd.OutOrStdout()
	for _, line := range lines {
		if _, err := fmt.Fprintln(out, line); err != nil {
			return err
		}
	}
	return nil
}

func workflowSet(specs []*Spec) func(id string) (*Spec, bool) {
	byID := make(map[string]*Spec, len(specs))
	for _, spec := range specs {
		byID[spec.ID] = spec
	}
	return func(id string) (*Spec, bool) {
		spec, ok := byID[id]
		return spec, ok
	}
}

func yesNo(value bool) string {
	if value {
		return "yes"
	}
	return "no"
}

func workflowValidateCmd(open CommandOpener) *cobra.Command {
	return &cobra.Command{
		Use:   "validate <file>",
		Short: "Validate a workflow definition file",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runWithOpener(cmd, open, CommandModeConfig, func(_ context.Context, deps *CommandDeps) error {
				if deps == nil || deps.ValidationDeps == nil {
					return codedError(ErrorCodeInvalidArgument, "workflow validation dependencies must not be nil")
				}
				spec, err := LoadSpecFile(args[0])
				if err != nil {
					return err
				}
				vdeps, err := deps.ValidationDeps()
				if err != nil {
					return err
				}
				siblings, err := LoadDefinitionsDir(deps.DefinitionsDir)
				if err != nil {
					return err
				}
				if _, err := Compile(spec, vdeps); err != nil {
					return fmt.Errorf("invalid workflow definition: %w", err)
				}
				if err := ValidateComposition(spec, workflowSet(append(siblings, spec))); err != nil {
					return fmt.Errorf("invalid workflow definition: %w", err)
				}
				_, err = fmt.Fprintln(cmd.OutOrStdout(), "valid: "+spec.ID)
				return err
			})
		},
	}
}

func workflowDefinitionsCmd(open CommandOpener) *cobra.Command {
	return &cobra.Command{
		Use:   "definitions",
		Short: "List workflow definitions from the configured directory",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runWithOpener(cmd, open, CommandModeConfig, func(_ context.Context, deps *CommandDeps) error {
				if deps == nil || deps.ValidationDeps == nil {
					return codedError(ErrorCodeInvalidArgument, "workflow validation dependencies must not be nil")
				}
				specs, err := LoadDefinitionsDir(deps.DefinitionsDir)
				if err != nil {
					return err
				}
				vdeps, err := deps.ValidationDeps()
				if err != nil {
					return err
				}
				out := cmd.OutOrStdout()
				for _, spec := range specs {
					if _, err := Compile(spec, vdeps); err != nil {
						return fmt.Errorf("definition %s: %w", spec.ID, err)
					}
					stepIDs := make([]string, 0, len(spec.Steps))
					for index := range spec.Steps {
						stepIDs = append(stepIDs, spec.Steps[index].ID)
					}
					if _, err := fmt.Fprintf(out, "%s\tv%d\t%s\n", spec.ID, spec.Version, strings.Join(stepIDs, ",")); err != nil {
						return err
					}
				}
				return nil
			})
		},
	}
}

func workflowStartCmd(open CommandOpener) *cobra.Command {
	var inputFlag string
	cmd := &cobra.Command{
		Use:   "start <id> [--input json]",
		Short: "Start one workflow run",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runWithOpener(cmd, open, CommandModeRuntime, func(ctx context.Context, deps *CommandDeps) error {
				if deps == nil || deps.Interpreter == nil {
					return codedError(ErrorCodeInvalidArgument, "workflow interpreter must not be nil")
				}
				runInput := &RunInput{}
				if inputFlag != "" {
					if err := json.Unmarshal([]byte(inputFlag), runInput); err != nil {
						return fmt.Errorf("invalid --input json: %w", err)
					}
				}
				summary, err := deps.Interpreter.StartConfigured(ctx, args[0], runInput, principal.OwnerID, principal.ClassOwner, "")
				if err != nil {
					return err
				}
				_, err = fmt.Fprintf(cmd.OutOrStdout(), "run: %s\nstatus: %s\nnote: the run is executed by a running aura server\n", summary.ID, summary.Status)
				return err
			})
		},
	}
	cmd.Flags().StringVar(&inputFlag, "input", "", "run input as JSON (objective, resources, permissions)")
	return cmd
}

func workflowRunsCmd(open CommandOpener) *cobra.Command {
	var statusFilter string
	cmd := &cobra.Command{
		Use:   "runs [--status filter]",
		Short: "List workflow runs",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runWithOpener(cmd, open, CommandModeStorage, func(ctx context.Context, deps *CommandDeps) error {
				if deps == nil || deps.Store == nil {
					return codedError(ErrorCodeInvalidArgument, "workflow store must not be nil")
				}
				runs, err := deps.Store.Runs(ctx, statusFilter)
				if err != nil {
					return err
				}
				out := cmd.OutOrStdout()
				for _, run := range runs {
					if _, err := fmt.Fprintf(out, "%s\t%s\t%s@v%d\t%s\n", run.ID, run.Status, run.DefinitionID, run.DefinitionVersion, EscapeText(run.Goal)); err != nil {
						return err
					}
				}
				return nil
			})
		},
	}
	cmd.Flags().StringVar(&statusFilter, "status", "", "filter by status")
	return cmd
}

func workflowInspectCmd(open CommandOpener) *cobra.Command {
	return &cobra.Command{
		Use:   "inspect <run>",
		Short: "Show one run and its steps",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runWithOpener(cmd, open, CommandModeStorage, func(ctx context.Context, deps *CommandDeps) error {
				if deps == nil || deps.Store == nil {
					return codedError(ErrorCodeInvalidArgument, "workflow store must not be nil")
				}
				run, err := deps.Store.Run(ctx, args[0])
				if err != nil {
					return err
				}
				steps, err := deps.Store.Steps(ctx, run.ID)
				if err != nil {
					return err
				}
				out := cmd.OutOrStdout()
				if _, err := fmt.Fprintf(out, "run: %s\nstatus: %s\ndefinition: %s@v%d\ngoal: %s\n", run.ID, run.Status, run.DefinitionID, run.DefinitionVersion, EscapeText(run.Goal)); err != nil {
					return err
				}
				for _, step := range steps {
					line := fmt.Sprintf("step: %s\t%s\tattempt %d", step.StepID, step.Status, step.Attempt)
					if step.ErrorCode != "" {
						line += "\t" + step.ErrorCode
					}
					if _, err := fmt.Fprintln(out, line); err != nil {
						return err
					}
					if step.OutputArtifactDigest != "" {
						if _, err := fmt.Fprintf(out, "  artifact: %s\n", step.OutputArtifactDigest); err != nil {
							return err
						}
						continue
					}
					if len(step.Output) > 0 {
						if _, err := fmt.Fprintf(out, "  output: %s\n", bytes.TrimSpace(step.Output)); err != nil {
							return err
						}
					}
				}
				correlations, err := deps.Store.ListCorrelationsByRun(ctx, run.ID)
				if err != nil {
					return err
				}
				for _, correlation := range correlations {
					matched := "awaiting"
					if correlation.DedupeKey != "" {
						matched = "matched"
					}
					if _, err := fmt.Fprintf(out, "correlation: %s\t%s\t%s\t%s\t%s\n",
						correlation.Source, correlation.EventType, correlation.ExternalID, correlation.SignalName, matched); err != nil {
						return err
					}
				}
				return nil
			})
		},
	}
}

func workflowCancelCmd(open CommandOpener) *cobra.Command {
	return &cobra.Command{
		Use:   "cancel <run>",
		Short: "Cancel a workflow run cooperatively",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runWithOpener(cmd, open, CommandModeRuntime, func(ctx context.Context, deps *CommandDeps) error {
				if deps == nil || deps.Store == nil || deps.Interpreter == nil {
					return codedError(ErrorCodeInvalidArgument, "workflow cancel requires a store and an interpreter")
				}
				run, err := deps.Store.Run(ctx, args[0])
				if err != nil {
					return err
				}
				if run.Status == RunSucceeded || run.Status == RunFailed || run.Status == RunCancelled {
					_, err = fmt.Fprintf(cmd.OutOrStdout(), "run: %s\nstatus: %s\n", run.ID, run.Status)
					return err
				}
				if err := deps.Interpreter.Cancel(ctx, args[0]); err != nil && !errors.Is(err, durable.ErrUnknownRun) {
					return err
				}
				if err := deps.Store.SetRunStatus(ctx, args[0], RunCancelled); err != nil {
					return err
				}
				_, err = fmt.Fprintf(cmd.OutOrStdout(), "run: %s\nstatus: %s\n", args[0], RunCancelled)
				return err
			})
		},
	}
}

func workflowApproveCmd(open CommandOpener) *cobra.Command {
	var stepID string
	var reject bool
	cmd := &cobra.Command{
		Use:   "approve <run> --step <id> [--reject]",
		Short: "Resolve a suspended approval step",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if stepID == "" {
				return errors.New("step id is required: pass --step <id>")
			}
			return runWithOpener(cmd, open, CommandModeRuntime, func(ctx context.Context, deps *CommandDeps) error {
				if deps == nil || deps.Interpreter == nil {
					return codedError(ErrorCodeInvalidArgument, "workflow interpreter must not be nil")
				}
				decision := "approve"
				if reject {
					decision = "reject"
				}
				payload, err := json.Marshal(map[string]string{"decision": decision})
				if err != nil {
					return fmt.Errorf("encode approval decision: %w", err)
				}
				if err := deps.Interpreter.Signal(ctx, args[0], "approval."+stepID, payload); err != nil {
					return err
				}
				_, err = fmt.Fprintf(cmd.OutOrStdout(), "run: %s\nsignal: %s\ndecision: %s\n", args[0], "approval."+stepID, decision)
				return err
			})
		},
	}
	cmd.Flags().StringVar(&stepID, "step", "", "approval step id to resolve")
	cmd.Flags().BoolVar(&reject, "reject", false, "reject instead of approving")
	return cmd
}

func workflowSignalCmd(open CommandOpener) *cobra.Command {
	var stepID string
	var payloadFlag string
	cmd := &cobra.Command{
		Use:   "signal <run> --step <id> --payload json",
		Short: "Deliver a payload to a suspended wait step",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if stepID == "" {
				return errors.New("step id is required: pass --step <id>")
			}
			if !json.Valid([]byte(payloadFlag)) {
				return errors.New("payload must be valid JSON")
			}
			return runWithOpener(cmd, open, CommandModeRuntime, func(ctx context.Context, deps *CommandDeps) error {
				if deps == nil || deps.Interpreter == nil {
					return codedError(ErrorCodeInvalidArgument, "workflow interpreter must not be nil")
				}
				if err := deps.Interpreter.Signal(ctx, args[0], "wait."+stepID, []byte(payloadFlag)); err != nil {
					return err
				}
				_, err := fmt.Fprintf(cmd.OutOrStdout(), "run: %s\nsignal: %s\n", args[0], "wait."+stepID)
				return err
			})
		},
	}
	cmd.Flags().StringVar(&stepID, "step", "", "wait step id to signal")
	cmd.Flags().StringVar(&payloadFlag, "payload", "", "signal payload as JSON")
	return cmd
}

func workflowProposalsCmd(open CommandOpener) *cobra.Command {
	var status string
	cmd := &cobra.Command{
		Use:   "proposals",
		Short: "List agent-proposed workflows",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			switch status {
			case "", ProposalProposed, ProposalApproved, ProposalRejected, ProposalExpired:
			default:
				return &commandUsageError{err: errors.New("--status must be proposed, approved, rejected or expired")}
			}
			return runWithOpener(cmd, open, CommandModeStorage, func(ctx context.Context, deps *CommandDeps) error {
				if deps == nil || deps.Store == nil {
					return codedError(ErrorCodeInvalidArgument, "workflow store must not be nil")
				}
				proposals, err := deps.Store.Proposals(ctx, status, commandNow(deps))
				if err != nil {
					return err
				}
				if len(proposals) == 0 {
					return writeLines(cmd, []string{"no proposals"})
				}
				lines := []string{"ID\tSTATUS\tPERSONA\tCREATED\tGOAL"}
				for _, proposal := range proposals {
					lines = append(lines, fmt.Sprintf("%s\t%s\t%s\t%s\t%s", proposal.ID, proposal.Status, EscapeText(proposal.AgentID), proposal.CreatedAt.UTC().Format(time.RFC3339), EscapeText(proposal.Spec.Goal)))
				}
				return writeLines(cmd, lines)
			})
		},
	}
	cmd.Flags().StringVar(&status, "status", "", "filter by proposed, approved, rejected or expired")
	return cmd
}

func workflowProposalCmd(open CommandOpener) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "proposal",
		Short: "Show, approve or reject one agent-proposed workflow",
	}
	cmd.AddCommand(
		&cobra.Command{
			Use:   "show <proposal-id>",
			Short: "Show a proposal and its plan",
			Args:  cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				return runWithOpener(cmd, open, CommandModeStorage, func(ctx context.Context, deps *CommandDeps) error {
					if deps == nil || deps.Store == nil {
						return codedError(ErrorCodeInvalidArgument, "workflow store must not be nil")
					}
					proposal, err := deps.Store.Proposal(ctx, args[0], commandNow(deps))
					if err != nil {
						return err
					}
					lines := []string{
						"proposal: " + EscapeText(proposal.ID),
						"status: " + EscapeText(proposal.Status),
						"persona: " + EscapeText(proposal.AgentID),
						"session: " + EscapeText(proposal.SessionID),
						"digest: " + EscapeText(proposal.Digest),
						"tainted: " + yesNo(proposal.Tainted),
					}
					if proposal.Tainted {
						lines = append(lines, approval.UntrustedInputNotice)
					}
					if proposal.RunID != "" {
						lines = append(lines, "run: "+EscapeText(proposal.RunID))
					}
					if proposal.PlanText == "" {
						return writeLines(cmd, append(lines, planNotStoredLine))
					}
					return writeLines(cmd, append(lines, proposal.PlanText))
				})
			},
		},
		&cobra.Command{
			Use:   "approve <proposal-id>",
			Short: "Approve a proposal and start exactly the approved spec",
			Args:  cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				return runWithOpener(cmd, open, CommandModeRuntime, func(ctx context.Context, deps *CommandDeps) error {
					if deps == nil || deps.Store == nil || deps.Interpreter == nil {
						return codedError(ErrorCodeInvalidArgument, "workflow proposal approve requires a store and an interpreter")
					}
					runID, err := ApproveStoredProposal(ctx, deps.Interpreter, deps.Store, args[0], deps.Secrets, commandNow(deps))
					if err != nil {
						return err
					}
					return writeLines(cmd, []string{"approved: " + args[0], "run: " + runID})
				})
			},
		},
		&cobra.Command{
			Use:   "reject <proposal-id>",
			Short: "Reject a proposal; nothing runs",
			Args:  cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				return runWithOpener(cmd, open, CommandModeRuntime, func(ctx context.Context, deps *CommandDeps) error {
					if deps == nil || deps.Store == nil {
						return codedError(ErrorCodeInvalidArgument, "workflow store must not be nil")
					}
					now := commandNow(deps)
					if _, err := deps.Store.Proposal(ctx, args[0], now); err != nil {
						return err
					}
					if err := deps.Store.DecideProposal(ctx, args[0], ProposalRejected, "", now); err != nil {
						return err
					}
					return writeLines(cmd, []string{"rejected: " + args[0]})
				})
			},
		},
	)
	return cmd
}
