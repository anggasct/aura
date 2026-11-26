package effect

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/anggasct/aura/internal/platform/clikit"
	"github.com/anggasct/aura/internal/platform/fault"
)

type CommandEnv struct {
	Logger  *slog.Logger
	Journal *Journal
}

type CommandOpener func(cmd *cobra.Command, fn func(ctx context.Context, env *CommandEnv) error) error

func Commands(open CommandOpener) []*cobra.Command {
	cmd := &cobra.Command{
		Use:   "effects",
		Short: "Inspect and resolve durable external effects",
	}
	cmd.AddCommand(
		newEffectsApproveCmd(open),
		newEffectsListCmd(open),
		newEffectsReconcileCmd(open),
		newEffectsMarkCmd(open),
		newEffectsRetryCmd(open),
		newEffectsPruneCmd(open),
	)
	return []*cobra.Command{cmd}
}

func runWithOpener(cmd *cobra.Command, open CommandOpener, fn func(ctx context.Context, env *CommandEnv) error) error {
	if open == nil {
		return fault.New(ErrorCodeInvalidArgument, "effect command opener must not be nil")
	}
	return open(cmd, func(ctx context.Context, env *CommandEnv) error {
		if env == nil || env.Journal == nil {
			return fault.New(ErrorCodeInvalidArgument, "effect command requires a journal")
		}
		return fn(ctx, env)
	})
}

func newEffectsApproveCmd(open CommandOpener) *cobra.Command {
	var action string
	var reason string
	var expires time.Duration
	cmd := &cobra.Command{
		Use:   "approve <id>",
		Short: "Issue a one-shot owner approval token",
		Args: func(cmd *cobra.Command, args []string) error {
			if err := exactOneIntentArg(cmd, args); err != nil {
				return err
			}
			if !validEffectApprovalAction(action) {
				return clikit.NewUsageError(fmt.Errorf("%s: invalid --action %q", cmd.CommandPath(), action))
			}
			if strings.TrimSpace(reason) == "" {
				return clikit.NewUsageError(errors.New("effects approve requires --reason <text>"))
			}
			if expires <= 0 || expires > 24*time.Hour {
				return clikit.NewUsageError(errors.New("effects approve --expires must be positive and at most 24h"))
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			return runWithOpener(cmd, open, func(ctx context.Context, env *CommandEnv) error {
				approval, err := env.Journal.Approve(ctx, &ApprovalRequest{
					IntentID:  args[0],
					Action:    effectApprovalAction(action),
					Reason:    reason,
					ExpiresIn: expires,
				})
				if err != nil {
					return err
				}
				_, err = fmt.Fprintf(cmd.OutOrStdout(), "approval_token: %s\nexpires_at: %s\n", approval.Token, approval.ExpiresAt.UTC().Format(time.RFC3339Nano))
				return err
			})
		},
	}
	cmd.Flags().StringVar(&action, "action", "", "approval action: mark-succeeded, mark-failed, or retry")
	cmd.Flags().StringVar(&reason, "reason", "", "operator reason for the approval")
	cmd.Flags().DurationVar(&expires, "expires", DefaultApprovalTTL, "approval lifetime, up to 24h")
	return cmd
}

func newEffectsListCmd(open CommandOpener) *cobra.Command {
	var state string
	var limit int
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List safe metadata for effect intents",
		Args: func(cmd *cobra.Command, args []string) error {
			if err := clikit.NoPositionalArgs(cmd, args); err != nil {
				return err
			}
			if limit < 0 {
				return clikit.NewUsageError(errors.New("effects list --limit must not be negative"))
			}
			if !validEffectState(state) {
				return clikit.NewUsageError(fmt.Errorf("effects list: invalid --state %q", state))
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runWithOpener(cmd, open, func(ctx context.Context, env *CommandEnv) error {
				intents, err := env.Journal.ListByState(ctx, State(state), limit)
				if err != nil {
					return err
				}
				if len(intents) == 0 {
					_, err := fmt.Fprintln(cmd.OutOrStdout(), "no effect intents")
					return err
				}
				tbl := clikit.NewTable(cmd.OutOrStdout())
				tbl.Printf("id\tstate\tprovider\toperation\tclassification\tupdated_at\tage\n")
				now := time.Now().UTC()
				for i := range intents {
					intent := &intents[i]
					age := now.Sub(intent.UpdatedAt)
					if age < 0 {
						age = 0
					}
					tbl.Printf("%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
						intent.ID, intent.State, intent.Provider, intent.Operation, intent.Classification,
						intent.UpdatedAt.UTC().Format(time.RFC3339Nano), age.Round(time.Second),
					)
				}
				return tbl.Flush()
			})
		},
	}
	cmd.Flags().StringVar(&state, "state", string(StateUnknown), "state to list")
	cmd.Flags().IntVar(&limit, "limit", 100, "maximum intents to list; zero means unlimited")
	return cmd
}

func newEffectsReconcileCmd(open CommandOpener) *cobra.Command {
	return &cobra.Command{
		Use:   "reconcile <id>",
		Short: "Reconcile an unknown effect through its provider adapter",
		Args:  exactOneIntentArg,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runWithOpener(cmd, open, func(context.Context, *CommandEnv) error {
				return fault.New(ErrorCodeReconciliationUnsupported, "provider reconciliation is unavailable for intent "+args[0])
			})
		},
	}
}

func newEffectsMarkCmd(open CommandOpener) *cobra.Command {
	var succeeded, failed bool
	var reason, token string
	cmd := &cobra.Command{
		Use:   "mark <id>",
		Short: "Resolve an unknown effect with an approval token",
		Args: func(cmd *cobra.Command, args []string) error {
			if err := exactOneIntentArg(cmd, args); err != nil {
				return err
			}
			if succeeded == failed {
				return clikit.NewUsageError(errors.New("effects mark requires exactly one of --succeeded or --failed"))
			}
			if strings.TrimSpace(reason) == "" {
				return clikit.NewUsageError(errors.New("effects mark requires --reason <text>"))
			}
			if token == "" {
				return clikit.NewUsageError(errors.New("effects mark requires --approval-token <token>"))
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			return runWithOpener(cmd, open, func(ctx context.Context, env *CommandEnv) error {
				intent, err := env.Journal.MarkWithApproval(ctx, args[0], succeeded, reason, token)
				if err != nil {
					return err
				}
				_, err = fmt.Fprintf(cmd.OutOrStdout(), "id: %s\nstate: %s\n", intent.ID, intent.State)
				return err
			})
		},
	}
	cmd.Flags().BoolVar(&succeeded, "succeeded", false, "resolve the effect as succeeded")
	cmd.Flags().BoolVar(&failed, "failed", false, "resolve the effect as failed")
	cmd.Flags().StringVar(&reason, "reason", "", "operator reason matching the approval token")
	cmd.Flags().StringVar(&token, "approval-token", "", "one-shot approval token")
	return cmd
}

func newEffectsRetryCmd(open CommandOpener) *cobra.Command {
	var token string
	cmd := &cobra.Command{
		Use:   "retry <id>",
		Short: "Create an approved linked retry intent",
		Args: func(cmd *cobra.Command, args []string) error {
			if err := exactOneIntentArg(cmd, args); err != nil {
				return err
			}
			if token == "" {
				return clikit.NewUsageError(errors.New("effects retry requires --approval-token <token>"))
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			return runWithOpener(cmd, open, func(ctx context.Context, env *CommandEnv) error {
				intent, err := env.Journal.RetryWithApproval(ctx, args[0], token)
				if err != nil {
					return err
				}
				_, err = fmt.Fprintf(cmd.OutOrStdout(), "id: %s\nretry_of: %s\nstate: %s\n", intent.ID, intent.RetryOf, intent.State)
				return err
			})
		},
	}
	cmd.Flags().StringVar(&token, "approval-token", "", "one-shot approval token")
	return cmd
}

func newEffectsPruneCmd(open CommandOpener) *cobra.Command {
	var before string
	cmd := &cobra.Command{
		Use:   "prune",
		Short: "Prune old terminal effect intents",
		Args: func(cmd *cobra.Command, args []string) error {
			if err := clikit.NoPositionalArgs(cmd, args); err != nil {
				return err
			}
			if before != "" {
				if _, err := time.Parse(time.RFC3339Nano, before); err != nil {
					return clikit.NewUsageError(fmt.Errorf("effects prune --before must be RFC3339Nano: %w", err))
				}
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runWithOpener(cmd, open, func(ctx context.Context, env *CommandEnv) error {
				cutoff := time.Now().UTC().Add(-DefaultTerminalRetention)
				if before != "" {
					parsed, err := time.Parse(time.RFC3339Nano, before)
					if err != nil {
						return err
					}
					cutoff = parsed
				}
				report, err := env.Journal.Prune(ctx, cutoff)
				if err != nil {
					return err
				}
				_, err = fmt.Fprintf(cmd.OutOrStdout(), "deleted: %d\npreserved_audits: %d\n", report.Deleted, report.PreservedAudits)
				return err
			})
		},
	}
	cmd.Flags().StringVar(&before, "before", "", "delete terminal intents older than this RFC3339Nano time (default: 30d ago)")
	return cmd
}

func exactOneIntentArg(cmd *cobra.Command, args []string) error {
	if len(args) != 1 {
		return clikit.NewUsageError(fmt.Errorf("%s requires exactly one intent ID", cmd.CommandPath()))
	}
	return nil
}

func effectApprovalAction(raw string) ApprovalAction {
	return ApprovalAction(strings.ReplaceAll(raw, "-", "_"))
}

func validEffectApprovalAction(raw string) bool {
	switch effectApprovalAction(raw) {
	case ApprovalActionMarkSucceeded, ApprovalActionMarkFailed, ApprovalActionRetry:
		return true
	default:
		return false
	}
}

func validEffectState(raw string) bool {
	switch State(raw) {
	case StatePrepared, StateStarted, StateSucceeded, StateUnknown, StateFailed:
		return true
	default:
		return false
	}
}
