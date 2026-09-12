package cli

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/anggasct/aura/internal/broadcast"
	"github.com/anggasct/aura/internal/config"
	"github.com/anggasct/aura/internal/effect"
	"github.com/anggasct/aura/internal/store"
	toolsbuiltin "github.com/anggasct/aura/internal/tools/builtin"
)

func newBroadcastCmd(gf *globalFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "broadcast",
		Short: "Inspect proactive broadcast notifications",
	}
	cmd.AddCommand(newBroadcastStatusCmd(gf), newBroadcastReconcileCmd(gf))
	return cmd
}

func newBroadcastStatusCmd(gf *globalFlags) *cobra.Command {
	var state string
	var showContent bool
	var limit int
	cmd := &cobra.Command{
		Use:   "status",
		Short: "List broadcast notifications without content by default",
		RunE: func(cmd *cobra.Command, args []string) error {
			if limit <= 0 || limit > 1000 {
				return &usageError{fmt.Errorf("%s: --limit must be between 1 and 1000", cmd.CommandPath())}
			}
			var states []string
			if strings.TrimSpace(state) != "" {
				states = strings.Split(state, ",")
			}
			return withStorage(cmd, gf, true, func(ctx context.Context, _ *slog.Logger, _ *config.Config, db *sql.DB) error {
				items, err := store.NewBroadcastStore(db).ListItems(ctx, states, limit)
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

func newBroadcastReconcileCmd(gf *globalFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "reconcile <id>",
		Short: "Settle an unknown broadcast or restart a stalled run",
		Args:  exactOneArg,
		RunE: func(cmd *cobra.Command, args []string) error {
			return withStorage(cmd, gf, true, func(ctx context.Context, logger *slog.Logger, cfg *config.Config, db *sql.DB) error {
				adapter := &broadcastItemStore{store: store.NewBroadcastStore(db)}
				item, found, err := adapter.Load(ctx, args[0])
				if err != nil {
					return err
				}
				if !found {
					return fmt.Errorf("broadcast %q not found", args[0])
				}
				out := cmd.OutOrStdout()
				switch item.State {
				case broadcast.StateUnknown:
					return reconcileUnknownBroadcast(ctx, cmd.OutOrStdout(), db, logger, cfg, adapter, &item)
				case broadcast.StateHeld, broadcast.StateScheduled:
					if item.NotBefore.UTC().After(time.Now().UTC()) {
						_, err := fmt.Fprintf(out, "id: %s\nresult: no-op (not due until %s)\n", item.ID, item.NotBefore.UTC().Format(time.RFC3339))
						return err
					}
					runtime, err := durableRuntimeForConfig(cfg, logger)
					if err != nil {
						return err
					}
					if err := broadcast.StartItemRun(ctx, runtime, item.ID); err != nil {
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
	return cmd
}

func reconcileUnknownBroadcast(ctx context.Context, out io.Writer, db *sql.DB, logger *slog.Logger, cfg *config.Config, adapter *broadcastItemStore, item *broadcast.FullItem) error {
	if item.EffectID == "" {
		_, err := fmt.Fprintf(out, "id: %s\nresult: unchanged (no linked effect)\n", item.ID)
		return err
	}
	executor, err := toolsbuiltin.NewChannelEffects(db, logger)
	if err != nil {
		return err
	}
	intent, err := executor.Journal().Get(ctx, item.EffectID)
	if err != nil {
		return err
	}
	providers, err := broadcastEffectProviders(cfg, db, logger)
	if err != nil {
		return err
	}
	provider, ok := providers[intent.Provider]
	if !ok {
		_, err := fmt.Fprintf(out, "id: %s\nresult: unchanged (provider %q not wired)\n", item.ID, intent.Provider)
		return err
	}
	resolved, err := executor.Reconcile(ctx, item.EffectID, provider, nil)
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
		if err := adapter.Settle(ctx, item.ID, broadcast.StateSucceeded, resolved.ID, now); err != nil {
			return err
		}
		_, err := fmt.Fprintf(out, "id: %s\nresult: settled succeeded\n", item.ID)
		return err
	case effect.StateFailed:
		if err := adapter.Settle(ctx, item.ID, broadcast.StateFailed, resolved.ID, now); err != nil {
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
	if code, ok := effect.CodeOf(err); ok {
		return string(code)
	}
	return "reconciliation inconclusive"
}
