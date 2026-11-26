package model

import (
	"context"
	"database/sql"
	"fmt"
	"maps"
	"slices"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/anggasct/aura/internal/platform/fault"
)

type CommandEnv struct {
	Models Config
	Routes map[string]ModelRoute
	DB     *sql.DB
}

type CommandOpener func(cmd *cobra.Command, needDB bool, fn func(ctx context.Context, env *CommandEnv) error) error

func Commands(open CommandOpener) []*cobra.Command {
	cmd := &cobra.Command{
		Use:   "models",
		Short: "Inspect model routes and circuit states",
	}
	cmd.AddCommand(
		newModelsRoutesCmd(open),
		newModelsCircuitsCmd(open),
		newModelsCircuitResetCmd(open),
	)
	return []*cobra.Command{cmd}
}

func runWithOpener(cmd *cobra.Command, open CommandOpener, needDB bool, fn func(ctx context.Context, env *CommandEnv) error) error {
	if open == nil {
		return fault.New(ErrorCodeInvalidArgument, "model command opener must not be nil")
	}
	return open(cmd, needDB, func(ctx context.Context, env *CommandEnv) error {
		if env == nil {
			return fault.New(ErrorCodeInvalidArgument, "model command environment must not be nil")
		}
		if needDB && env.DB == nil {
			return fault.New(ErrorCodeInvalidArgument, "model command requires an open database")
		}
		return fn(ctx, env)
	})
}

func newModelsRoutesCmd(open CommandOpener) *cobra.Command {
	return &cobra.Command{
		Use:   "routes",
		Short: "List configured model routes",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runWithOpener(cmd, open, false, func(_ context.Context, env *CommandEnv) error {
				out := cmd.OutOrStdout()
				tw := tabwriter.NewWriter(out, 0, 8, 2, ' ', 0)
				if _, err := fmt.Fprintln(tw, "ROUTE\tCANDIDATES\tMAX ATTEMPTS\tDELAY BUDGET\tCOST BUDGET"); err != nil {
					return err
				}
				for _, routeName := range slices.Sorted(maps.Keys(env.Routes)) {
					route := env.Routes[routeName]
					attempts := route.MaxProviderAttempts
					if attempts <= 0 {
						attempts = DefaultModelRouteMaxProviderAttempts
					}
					delay := time.Duration(route.RetryDelayBudget)
					if delay <= 0 {
						delay = DefaultModelRouteRetryDelayBudget
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
			})
		},
	}
}

func newModelsCircuitsCmd(open CommandOpener) *cobra.Command {
	return &cobra.Command{
		Use:   "circuits",
		Short: "Inspect model circuit states",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runWithOpener(cmd, open, true, func(ctx context.Context, env *CommandEnv) error {
				cm, _, err := loadCommandCircuits(ctx, env)
				if err != nil {
					return err
				}
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
			})
		},
	}
}

func newModelsCircuitResetCmd(open CommandOpener) *cobra.Command {
	return &cobra.Command{
		Use:   "circuit-reset <definition-id>",
		Short: "Reset circuit breaker state for a model definition",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runWithOpener(cmd, open, true, func(ctx context.Context, env *CommandEnv) error {
				cm, checkpointStore, err := loadCommandCircuits(ctx, env)
				if err != nil {
					return err
				}
				target := args[0]
				if !cm.Reset(ctx, target) {
					_ = checkpointStore.Delete(ctx, target)
					return fmt.Errorf("circuit for model definition %q not found", target)
				}
				_, err = fmt.Fprintf(cmd.OutOrStdout(), "circuit reset for %s\n", target)
				return err
			})
		},
	}
}

func loadCommandCircuits(ctx context.Context, env *CommandEnv) (*CircuitManager, CircuitCheckpointStore, error) {
	checkpointStore := NewSQLiteCircuitCheckpointStore(env.DB)
	cm := NewCircuitManager(time.Now, checkpointStore)
	for name := range env.Models.Definitions {
		def := env.Models.Definitions[name]
		resolved, err := env.Models.ResolveDefinition(&def)
		if err != nil {
			return nil, nil, err
		}
		digest := ComputeConfigDigest(&def, resolved)
		cm.Register(name, resolved.BaseURL, digest, DefaultCircuitPolicy())
	}
	if err := cm.LoadCheckpoints(ctx); err != nil {
		return nil, nil, err
	}
	return cm, checkpointStore, nil
}
