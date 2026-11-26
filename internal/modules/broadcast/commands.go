package broadcast

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/anggasct/aura/internal/platform/durable"
	"github.com/anggasct/aura/internal/platform/effect"
	"github.com/anggasct/aura/internal/platform/fault"
)

type cliUsageError struct {
	err error
}

func (e *cliUsageError) Error() string { return e.err.Error() }

func (e *cliUsageError) Unwrap() error { return e.err }

func (e *cliUsageError) UsageError() bool { return true }

type EffectReconciler interface {
	GetIntent(ctx context.Context, id string) (*effect.Intent, error)
	Reconcile(ctx context.Context, id string, provider effect.Provider) (*effect.Intent, error)
}

type CommandEnv struct {
	Store           *SQLiteStore
	Runtime         durable.Runtime
	Effects         func() (EffectReconciler, error)
	EffectProviders func() (map[string]effect.Provider, error)
}

type Opener func(cmd *cobra.Command, needDurable bool, fn func(ctx context.Context, env *CommandEnv) error) error

func Commands(open Opener) []*cobra.Command {
	cmd := &cobra.Command{
		Use:   "broadcast",
		Short: "Inspect proactive broadcast notifications",
	}
	cmd.AddCommand(newStatusCmd(open), newReconcileCmd(open))
	return []*cobra.Command{cmd}
}

func requireOpener(open Opener) error {
	if open == nil {
		return fault.New(ErrorCodeInvalidArgument, "broadcast command opener must not be nil")
	}
	return nil
}

func exactOneBroadcastID(cmd *cobra.Command, args []string) error {
	if len(args) != 1 || strings.TrimSpace(args[0]) == "" {
		return &cliUsageError{fmt.Errorf("%s requires exactly one broadcast ID", cmd.CommandPath())}
	}
	return nil
}

func newStatusCmd(open Opener) *cobra.Command {
	var state string
	var showContent bool
	var limit int
	cmd := &cobra.Command{
		Use:   "status",
		Short: "List broadcast notifications without content by default",
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := requireOpener(open); err != nil {
				return err
			}
			if limit <= 0 || limit > 1000 {
				return &cliUsageError{fmt.Errorf("%s: --limit must be between 1 and 1000", cmd.CommandPath())}
			}
			var states []string
			if strings.TrimSpace(state) != "" {
				states = strings.Split(state, ",")
			}
			return open(cmd, false, func(ctx context.Context, env *CommandEnv) error {
				if env == nil || env.Store == nil {
					return fault.New(ErrorCodeInvalidArgument, "broadcast command environment is incomplete")
				}
				items, err := env.Store.ListItems(ctx, states, limit)
				if err != nil {
					return err
				}
				out := cmd.OutOrStdout()
				header := "ID\tPRODUCER\tPRIORITY\tDESTINATION\tSTATE\tNOT_BEFORE\tATTEMPTS\tEFFECT\tAGE\n"
				if showContent {
					header = "ID\tPRODUCER\tPRIORITY\tDESTINATION\tSTATE\tNOT_BEFORE\tATTEMPTS\tEFFECT\tAGE\tCONTENT\n"
				}
				if _, err := fmt.Fprint(out, header); err != nil {
					return err
				}
				now := time.Now().UTC()
				for i := range items {
					item := &items[i]
					row := fmt.Sprintf("%s\t%s\t%s\t%s\t%s\t%s\t%d\t%s\t%s\n",
						item.ID,
						item.Producer,
						item.Priority,
						item.DestinationAlias,
						item.State,
						item.NotBefore.UTC().Format(time.RFC3339),
						item.AttemptCount,
						item.EffectID,
						now.Sub(item.CreatedAt.UTC()).Round(time.Second),
					)
					if showContent {
						row = fmt.Sprintf("%s\t%s\t%s\t%s\t%s\t%s\t%d\t%s\t%s\t%s\n",
							item.ID,
							item.Producer,
							item.Priority,
							item.DestinationAlias,
							item.State,
							item.NotBefore.UTC().Format(time.RFC3339),
							item.AttemptCount,
							item.EffectID,
							now.Sub(item.CreatedAt.UTC()).Round(time.Second),
							item.ContentJSON,
						)
					}
					if _, err := fmt.Fprint(out, row); err != nil {
						return err
					}
				}
				return ctx.Err()
			})
		},
	}
	cmd.Flags().StringVar(&state, "state", "", "comma-separated lifecycle states to show")
	cmd.Flags().BoolVar(&showContent, "show-content", false, "include notification content in the output")
	cmd.Flags().IntVar(&limit, "limit", 100, "maximum rows to show")
	return cmd
}

func newReconcileCmd(open Opener) *cobra.Command {
	return &cobra.Command{
		Use:   "reconcile <id>",
		Short: "Settle an unknown broadcast or restart a stalled run",
		Args:  exactOneBroadcastID,
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := requireOpener(open); err != nil {
				return err
			}
			return open(cmd, true, func(ctx context.Context, env *CommandEnv) error {
				if env == nil || env.Store == nil || env.Runtime == nil {
					return fault.New(ErrorCodeInvalidArgument, "broadcast command environment is incomplete")
				}
				item, found, err := env.Store.Load(ctx, args[0])
				if err != nil {
					return err
				}
				if !found {
					return fmt.Errorf("broadcast %q not found", args[0])
				}
				out := cmd.OutOrStdout()
				switch item.State {
				case StateUnknown:
					return reconcileUnknownBroadcast(ctx, out, env, &item)
				case StateHeld, StateScheduled:
					if item.NotBefore.UTC().After(time.Now().UTC()) {
						_, err := fmt.Fprintf(out, "id: %s\nresult: no-op (not due until %s)\n", item.ID, item.NotBefore.UTC().Format(time.RFC3339))
						return err
					}
					if err := ResumeItemRun(ctx, env.Runtime, item.ID); err != nil {
						return err
					}
					_, err = fmt.Fprintf(out, "id: %s\nresult: run started\n", item.ID)
					return err
				default:
					_, err := fmt.Fprintf(out, "id: %s\nresult: no-op (state %s)\n", item.ID, item.State)
					return err
				}
			})
		},
	}
}

func reconcileUnknownBroadcast(ctx context.Context, out io.Writer, env *CommandEnv, item *FullItem) error {
	if item.EffectID == "" {
		_, err := fmt.Fprintf(out, "id: %s\nresult: unchanged (no linked effect)\n", item.ID)
		return err
	}
	if env.Effects == nil || env.EffectProviders == nil {
		return fault.New(ErrorCodeInvalidArgument, "broadcast effect reconciler is not configured")
	}
	reconciler, err := env.Effects()
	if err != nil {
		return err
	}
	if reconciler == nil {
		return fault.New(ErrorCodeInvalidArgument, "broadcast effect reconciler must not be nil")
	}
	intent, err := reconciler.GetIntent(ctx, item.EffectID)
	if err != nil {
		return err
	}
	providers, err := env.EffectProviders()
	if err != nil {
		return err
	}
	provider, ok := providers[intent.Provider]
	if !ok {
		_, err := fmt.Fprintf(out, "id: %s\nresult: unchanged (provider %q not wired)\n", item.ID, intent.Provider)
		return err
	}
	resolved, err := reconciler.Reconcile(ctx, item.EffectID, provider)
	if err != nil {
		_, reportErr := fmt.Fprintf(out, "id: %s\nresult: unchanged (%s)\n", item.ID, reconcileReason(err))
		if reportErr != nil {
			return reportErr
		}
		return nil
	}
	now := time.Now().UTC()
	switch resolved.State {
	case effect.StateSucceeded:
		if err := env.Store.Settle(ctx, item.ID, StateSucceeded, resolved.ID, now); err != nil {
			return err
		}
		_, err := fmt.Fprintf(out, "id: %s\nresult: settled succeeded\n", item.ID)
		return err
	case effect.StateFailed:
		if err := env.Store.Settle(ctx, item.ID, StateFailed, resolved.ID, now); err != nil {
			return err
		}
		_, err := fmt.Fprintf(out, "id: %s\nresult: settled failed\n", item.ID)
		return err
	default:
		_, err := fmt.Fprintf(out, "id: %s\nresult: unchanged (effect still unknown)\n", item.ID)
		return err
	}
}

func reconcileReason(err error) string {
	if code, ok := fault.CodeOf[effect.ErrorCode](err); ok {
		return string(code)
	}
	return "reconciliation inconclusive"
}
