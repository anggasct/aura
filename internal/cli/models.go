package cli

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/anggasct/aura/internal/config"
	"github.com/anggasct/aura/internal/model"
	"github.com/anggasct/aura/internal/store"
)

type storeCircuitCheckpointAdapter struct {
	store store.CircuitCheckpointStore
}

func (a *storeCircuitCheckpointAdapter) Save(ctx context.Context, cp *model.CircuitCheckpoint) error {
	if a.store == nil || cp == nil {
		return nil
	}
	return a.store.Save(ctx, &store.CircuitCheckpoint{
		CircuitKey:          cp.CircuitKey,
		ConfigDigest:        cp.ConfigDigest,
		State:               string(cp.State),
		ConsecutiveFailures: cp.ConsecutiveFailures,
		OpenUntil:           cp.OpenUntil,
		UpdatedAt:           cp.UpdatedAt,
	})
}

func (a *storeCircuitCheckpointAdapter) Load(ctx context.Context) ([]model.CircuitCheckpoint, error) {
	if a.store == nil {
		return nil, nil
	}
	items, err := a.store.Load(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]model.CircuitCheckpoint, len(items))
	for i, it := range items {
		out[i] = model.CircuitCheckpoint{
			CircuitKey:          it.CircuitKey,
			ConfigDigest:        it.ConfigDigest,
			State:               model.CircuitState(it.State),
			ConsecutiveFailures: it.ConsecutiveFailures,
			OpenUntil:           it.OpenUntil,
			UpdatedAt:           it.UpdatedAt,
		}
	}
	return out, nil
}

func (a *storeCircuitCheckpointAdapter) Delete(ctx context.Context, circuitKey string) error {
	if a.store == nil {
		return nil
	}
	return a.store.Delete(ctx, circuitKey)
}

func newModelsCmd(gf *globalFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "models",
		Short: "Inspect model routes and circuit states",
	}
	cmd.AddCommand(
		newModelsRoutesCmd(gf),
		newModelsCircuitsCmd(gf),
		newModelsCircuitResetCmd(gf),
	)
	return cmd
}

func newModelsRoutesCmd(gf *globalFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "routes",
		Short: "List configured model routes",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			result, err := config.Load(gf.configPath)
			if err != nil {
				return err
			}
			cfg := result.Config
			out := cmd.OutOrStdout()
			tw := tabwriter.NewWriter(out, 0, 8, 2, ' ', 0)
			if _, err := fmt.Fprintln(tw, "ROUTE\tCANDIDATES\tMAX ATTEMPTS\tDELAY BUDGET\tCOST BUDGET"); err != nil {
				return err
			}
			for _, routeName := range slices.Sorted(maps.Keys(cfg.ModelRoutes)) {
				route := cfg.ModelRoutes[routeName]
				attempts := route.MaxProviderAttempts
				if attempts <= 0 {
					attempts = 4
				}
				delay := time.Duration(route.RetryDelayBudget)
				if delay <= 0 {
					delay = 20 * time.Second
				}
				cost := "-"
				if route.CostBudgetUSD > 0 {
					cost = fmt.Sprintf("$%.2f", route.CostBudgetUSD)
				}
				if _, err := fmt.Fprintf(tw, "%s\t%s\t%d\t%s\t%s\n",
					routeName,
					strings.Join(route.Candidates, ", "),
					attempts,
					delay,
					cost,
				); err != nil {
					return err
				}
			}
			return tw.Flush()
		},
	}
}

func newModelsCircuitsCmd(gf *globalFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "circuits",
		Short: "Inspect model circuit states",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			result, err := config.Load(gf.configPath)
			if err != nil {
				return err
			}
			cfg := result.Config

			db, err := openStorage(cmd.Context(), cfg)
			if err != nil {
				return err
			}
			defer func() { _ = db.Close() }()

			checkpointStore := store.NewCircuitCheckpointStore(db)
			adapter := &storeCircuitCheckpointAdapter{store: checkpointStore}
			cm := model.NewCircuitManager(time.Now, adapter)

			for name, def := range cfg.Models.Definitions {
				defCopy := def
				digest := model.ComputeConfigDigest(&defCopy)
				cm.Register(name, def.BaseURL, digest, model.DefaultCircuitPolicy())
			}

			_ = cm.LoadCheckpoints(cmd.Context())

			statuses := cm.Inspect()
			out := cmd.OutOrStdout()
			tw := tabwriter.NewWriter(out, 0, 8, 2, ' ', 0)
			if _, err := fmt.Fprintln(tw, "CIRCUIT KEY\tSTATE\tFAILURES\tOPEN UNTIL\tUPDATED AT"); err != nil {
				return err
			}
			for i := range statuses {
				status := &statuses[i]
				openUntil := "-"
				if status.OpenUntil != nil && !status.OpenUntil.IsZero() {
					openUntil = status.OpenUntil.UTC().Format(time.RFC3339)
				}
				updatedAt := "-"
				if !status.UpdatedAt.IsZero() {
					updatedAt = status.UpdatedAt.UTC().Format(time.RFC3339)
				}
				if _, err := fmt.Fprintf(tw, "%s\t%s\t%d\t%s\t%s\n",
					status.Key,
					status.State,
					status.ConsecutiveFailures,
					openUntil,
					updatedAt,
				); err != nil {
					return err
				}
			}
			return tw.Flush()
		},
	}
}

func newModelsCircuitResetCmd(gf *globalFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "circuit-reset <definition-id>",
		Short: "Reset circuit breaker state for a model definition",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			result, err := config.Load(gf.configPath)
			if err != nil {
				return err
			}
			cfg := result.Config

			db, err := openStorage(cmd.Context(), cfg)
			if err != nil {
				return err
			}
			defer func() { _ = db.Close() }()

			checkpointStore := store.NewCircuitCheckpointStore(db)
			adapter := &storeCircuitCheckpointAdapter{store: checkpointStore}
			cm := model.NewCircuitManager(time.Now, adapter)

			for name, def := range cfg.Models.Definitions {
				defCopy := def
				digest := model.ComputeConfigDigest(&defCopy)
				cm.Register(name, def.BaseURL, digest, model.DefaultCircuitPolicy())
			}

			_ = cm.LoadCheckpoints(cmd.Context())

			target := args[0]
			reset := cm.Reset(cmd.Context(), target)
			if !reset {
				_ = checkpointStore.Delete(cmd.Context(), target)
				return fmt.Errorf("circuit for model definition %q not found", target)
			}

			_, err = fmt.Fprintf(cmd.OutOrStdout(), "circuit reset for %s\n", target)
			return err
		},
	}
}
