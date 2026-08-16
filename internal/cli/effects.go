package cli

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/anggasct/aura/internal/config"
	"github.com/anggasct/aura/internal/effect"
)

func newEffectsCmd(gf *globalFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "effects",
		Short: "Inspect and resolve durable external effects",
	}
	cmd.AddCommand(
		newEffectsApproveCmd(gf),
		newEffectsListCmd(gf),
		newEffectsReconcileCmd(gf),
		newEffectsMarkCmd(gf),
		newEffectsRetryCmd(gf),
	)
	return cmd
}

func newEffectsApproveCmd(gf *globalFlags) *cobra.Command {
	var action string
	var reason string
	var expires time.Duration
	cmd := &cobra.Command{
		Use:   "approve <id>",
		Short: "Issue a one-shot owner approval token",
		Args: func(cmd *cobra.Command, args []string) error {
			if err := exactOneArg(cmd, args); err != nil {
				return err
			}
			if !validEffectApprovalAction(action) {
				return &usageError{fmt.Errorf("%s: invalid --action %q", cmd.CommandPath(), action)}
			}
			if strings.TrimSpace(reason) == "" {
				return &usageError{errors.New("effects approve requires --reason <text>")}
			}
			if expires <= 0 || expires > 24*time.Hour {
				return &usageError{errors.New("effects approve --expires must be positive and at most 24h")}
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			return withEffects(cmd, gf, func(ctx context.Context, logger *slog.Logger, cfg *config.Config, journal *effect.Journal) error {
				approval, err := journal.Approve(ctx, &effect.ApprovalRequest{
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
	cmd.Flags().DurationVar(&expires, "expires", effect.DefaultApprovalTTL, "approval lifetime, up to 24h")
	return cmd
}

func newEffectsListCmd(gf *globalFlags) *cobra.Command {
	var state string
	var limit int
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List safe metadata for effect intents",
		Args: func(cmd *cobra.Command, args []string) error {
			if err := noPositionalArgs(cmd, args); err != nil {
				return err
			}
			if limit < 0 {
				return &usageError{errors.New("effects list --limit must not be negative")}
			}
			if !validEffectState(state) {
				return &usageError{fmt.Errorf("effects list: invalid --state %q", state)}
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			return withEffects(cmd, gf, func(ctx context.Context, logger *slog.Logger, cfg *config.Config, journal *effect.Journal) error {
				intents, err := journal.ListByState(ctx, effectState(state), limit)
				if err != nil {
					return err
				}
				if len(intents) == 0 {
					_, err := fmt.Fprintln(cmd.OutOrStdout(), "no effect intents")
					return err
				}
				tbl := newTable(cmd.OutOrStdout())
				tbl.printf("id\tstate\tprovider\toperation\tclassification\tupdated_at\tage\n")
				now := time.Now().UTC()
				for i := range intents {
					intent := &intents[i]
					age := now.Sub(intent.UpdatedAt)
					if age < 0 {
						age = 0
					}
					tbl.printf("%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
						intent.ID, intent.State, intent.Provider, intent.Operation, intent.Classification,
						intent.UpdatedAt.UTC().Format(time.RFC3339Nano), age.Round(time.Second),
					)
				}
				return tbl.flush()
			})
		},
	}
	cmd.Flags().StringVar(&state, "state", string(effect.StateUnknown), "state to list")
	cmd.Flags().IntVar(&limit, "limit", 100, "maximum intents to list; zero means unlimited")
	return cmd
}

func newEffectsReconcileCmd(gf *globalFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "reconcile <id>",
		Short: "Reconcile an unknown effect through its provider adapter",
		Args:  exactOneArg,
		RunE: func(cmd *cobra.Command, args []string) error {
			return withEffects(cmd, gf, func(context.Context, *slog.Logger, *config.Config, *effect.Journal) error {
				return &effect.Error{Code: effect.ErrorCodeReconciliationUnsupported, Detail: "provider reconciliation is unavailable for intent " + args[0]}
			})
		},
	}
}

func newEffectsMarkCmd(gf *globalFlags) *cobra.Command {
	var succeeded, failed bool
	var reason, token string
	cmd := &cobra.Command{
		Use:   "mark <id>",
		Short: "Resolve an unknown effect with an approval token",
		Args: func(cmd *cobra.Command, args []string) error {
			if err := exactOneArg(cmd, args); err != nil {
				return err
			}
			if succeeded == failed {
				return &usageError{errors.New("effects mark requires exactly one of --succeeded or --failed")}
			}
			if strings.TrimSpace(reason) == "" {
				return &usageError{errors.New("effects mark requires --reason <text>")}
			}
			if token == "" {
				return &usageError{errors.New("effects mark requires --approval-token <token>")}
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			return withEffects(cmd, gf, func(ctx context.Context, logger *slog.Logger, cfg *config.Config, journal *effect.Journal) error {
				intent, err := journal.MarkWithApproval(ctx, args[0], succeeded, reason, token)
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

func newEffectsRetryCmd(gf *globalFlags) *cobra.Command {
	var token string
	cmd := &cobra.Command{
		Use:   "retry <id>",
		Short: "Create an approved linked retry intent",
		Args: func(cmd *cobra.Command, args []string) error {
			if err := exactOneArg(cmd, args); err != nil {
				return err
			}
			if token == "" {
				return &usageError{errors.New("effects retry requires --approval-token <token>")}
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			return withEffects(cmd, gf, func(ctx context.Context, logger *slog.Logger, cfg *config.Config, journal *effect.Journal) error {
				intent, err := journal.RetryWithApproval(ctx, args[0], token)
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

func withEffects(cmd *cobra.Command, gf *globalFlags, fn func(context.Context, *slog.Logger, *config.Config, *effect.Journal) error) error {
	return withStorage(cmd, gf, true, func(ctx context.Context, logger *slog.Logger, cfg *config.Config, db *sql.DB) error {
		journal, err := effect.NewJournal(db, effect.Options{Logger: logger})
		if err != nil {
			return err
		}
		return fn(ctx, logger, cfg, journal)
	})
}

func exactOneArg(cmd *cobra.Command, args []string) error {
	if len(args) != 1 {
		return &usageError{fmt.Errorf("%s requires exactly one intent ID", cmd.CommandPath())}
	}
	return nil
}

func effectApprovalAction(raw string) effect.ApprovalAction {
	return effect.ApprovalAction(strings.ReplaceAll(raw, "-", "_"))
}

func effectState(raw string) effect.State {
	return effect.State(raw)
}

func validEffectApprovalAction(raw string) bool {
	switch effectApprovalAction(raw) {
	case effect.ApprovalActionMarkSucceeded, effect.ApprovalActionMarkFailed, effect.ApprovalActionRetry:
		return true
	default:
		return false
	}
}

func validEffectState(raw string) bool {
	switch effect.State(raw) {
	case effect.StatePrepared, effect.StateStarted, effect.StateSucceeded, effect.StateUnknown, effect.StateFailed:
		return true
	default:
		return false
	}
}
