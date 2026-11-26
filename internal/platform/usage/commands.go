package usage

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"

	"github.com/spf13/cobra"

	"github.com/anggasct/aura/internal/platform/clikit"
	"github.com/anggasct/aura/internal/platform/fault"
)

const (
	usageGroupPersona   = "persona"
	usageUnattributedID = "(unattributed)"
)

type CommandEnv struct {
	Logger *slog.Logger
	Ledger *Ledger
	Prices *PriceRegistry
}

type CommandOpener func(cmd *cobra.Command, pricesPath string, fn func(ctx context.Context, env *CommandEnv) error) error

func Commands(open CommandOpener) []*cobra.Command {
	cmd := &cobra.Command{
		Use:   "usage",
		Short: "Model spend ledger: budget status, entries, prices, and reconciliation",
	}
	cmd.AddCommand(
		newUsageStatusCmd(open),
		newUsageEntriesCmd(open),
		newUsagePricesCmd(open),
		newUsageReconcileCmd(open),
	)
	return []*cobra.Command{cmd}
}

func runWithOpener(cmd *cobra.Command, open CommandOpener, pricesPath string, needLedger, needPrices bool, fn func(ctx context.Context, env *CommandEnv) error) error {
	if open == nil {
		return fault.New(ErrorCodeInvalidArgument, "usage command opener must not be nil")
	}
	return open(cmd, pricesPath, func(ctx context.Context, env *CommandEnv) error {
		if env == nil {
			return fault.New(ErrorCodeInvalidArgument, "usage command environment must not be nil")
		}
		if needLedger && env.Ledger == nil {
			return fault.New(ErrorCodeInvalidArgument, "usage command requires a ledger")
		}
		if needPrices && env.Prices == nil {
			return fault.New(ErrorCodeInvalidArgument, "usage command requires a price registry")
		}
		return fn(ctx, env)
	})
}

func newUsageStatusCmd(open CommandOpener) *cobra.Command {
	var by string
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Report current day/month spend against budget caps",
		Args:  clikit.NoPositionalArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if by != "" && by != usageGroupPersona {
				return clikit.NewUsageError(fmt.Errorf("%s: --by %q is not supported; use %q", cmd.CommandPath(), by, usageGroupPersona))
			}
			return runWithOpener(cmd, open, "", true, false, func(ctx context.Context, env *CommandEnv) error {
				if by == usageGroupPersona {
					return printUsageByPersona(ctx, cmd.OutOrStdout(), env.Ledger)
				}
				st, err := env.Ledger.Status(ctx)
				if err != nil {
					return err
				}
				tbl := clikit.NewTable(cmd.OutOrStdout())
				tbl.Printf("window\tday\tmonth\n")
				tbl.Printf("used (micros)\t%d\t%d\n", st.DayUsedMicros(), st.MonthUsedMicros())
				tbl.Printf("reserved (micros)\t%d\t%d\n", st.DayReservedMicros, st.MonthReservedMicros)
				tbl.Printf("settled (micros)\t%d\t%d\n", st.DaySettledMicros, st.MonthSettledMicros)
				tbl.Printf("cap (micros)\t%d\t%d\n", st.DailyCapMicros, st.MonthlyCapMicros)
				tbl.Printf("remaining (micros)\t%d\t%d\n", st.DayRemainingMicros(), st.MonthRemainingMicros())
				tbl.Printf("period\t%s\t%s\n", st.Day, st.Month)
				if err := tbl.Flush(); err != nil {
					return err
				}
				_, err = fmt.Fprintf(cmd.OutOrStdout(), "active reservations: %d\n", st.ActiveReservations)
				return err
			})
		},
	}
	cmd.Flags().StringVar(&by, "by", "", `group spend; only "persona" is supported`)
	return cmd
}

func printUsageByPersona(ctx context.Context, out io.Writer, ledger *Ledger) error {
	spends, err := ledger.StatusByAgent(ctx)
	if err != nil {
		return err
	}
	tbl := clikit.NewTable(out)
	tbl.Printf("persona\tday used (micros)\tmonth used (micros)\n")
	for i := range spends {
		persona := spends[i].AgentID
		if persona == "" {
			persona = usageUnattributedID
		}
		tbl.Printf("%s\t%d\t%d\n", persona, spends[i].DayUsedMicros(), spends[i].MonthUsedMicros())
	}
	return tbl.Flush()
}

func newUsageEntriesCmd(open CommandOpener) *cobra.Command {
	var limit int
	cmd := &cobra.Command{
		Use:   "entries",
		Short: "List settled usage entries, newest first",
		Args: func(cmd *cobra.Command, args []string) error {
			if err := clikit.NoPositionalArgs(cmd, args); err != nil {
				return err
			}
			if limit < 0 {
				return clikit.NewUsageError(errors.New("usage entries --limit must not be negative"))
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runWithOpener(cmd, open, "", true, false, func(ctx context.Context, env *CommandEnv) error {
				entries, err := env.Ledger.Entries(ctx, limit)
				if err != nil {
					return err
				}
				if len(entries) == 0 {
					_, err := fmt.Fprintln(cmd.OutOrStdout(), "no usage entries")
					return err
				}
				tbl := clikit.NewTable(cmd.OutOrStdout())
				tbl.Printf("recorded_at\treservation_id\tmodel\tinput\toutput\tcost_micros\taccounting\tprice_version\n")
				for i := range entries {
					en := &entries[i]
					tbl.Printf("%s\t%s\t%s\t%d\t%d\t%d\t%s\t%s\n",
						en.RecordedAt.UTC().Format("2006-01-02T15:04:05Z"),
						en.ReservationID, en.ModelDefinitionID, en.InputTokens, en.OutputTokens,
						en.CostMicros, en.Accounting, en.PriceVersion)
				}
				return tbl.Flush()
			})
		},
	}
	cmd.Flags().IntVar(&limit, "limit", 50, "maximum entries to list")
	return cmd
}

func newUsagePricesCmd(open CommandOpener) *cobra.Command {
	var pricesPath string
	cmd := &cobra.Command{
		Use:   "prices",
		Short: "List versioned price records from the operator price file",
		Args:  clikit.NoPositionalArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runWithOpener(cmd, open, pricesPath, false, true, func(_ context.Context, env *CommandEnv) error {
				prices := env.Prices.All()
				if len(prices) == 0 {
					_, err := fmt.Fprintln(cmd.OutOrStdout(), "no price records")
					return err
				}
				tbl := clikit.NewTable(cmd.OutOrStdout())
				tbl.Printf("model\tcurrency\tin\tout\tcache\treasoning\teffective_from\teffective_to\trate\tsource\n")
				for i := range prices {
					p := &prices[i]
					to := "open"
					if !p.EffectiveTo.IsZero() {
						to = p.EffectiveTo.UTC().Format("2006-01-02")
					}
					tbl.Printf("%s\t%s\t%d\t%d\t%d\t%d\t%s\t%s\t%d%%\t%s\n",
						p.ModelDefinitionID, p.Currency,
						p.MicrosPerInputToken, p.MicrosPerOutputToken,
						p.MicrosPerCacheToken, p.MicrosPerReasoningToken,
						p.EffectiveFrom.UTC().Format("2006-01-02"), to,
						p.MaxReservationRate, p.Source)
				}
				return tbl.Flush()
			})
		},
	}
	cmd.Flags().StringVar(&pricesPath, "prices", "", "path to the operator price file (default: prices.yaml beside config)")
	return cmd
}

func newUsageReconcileCmd(open CommandOpener) *cobra.Command {
	return &cobra.Command{
		Use:   "reconcile",
		Short: "Expire stale reservations and release expired ones; never deletes rows",
		Args:  clikit.NoPositionalArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runWithOpener(cmd, open, "", true, false, func(ctx context.Context, env *CommandEnv) error {
				expired, err := env.Ledger.ExpireStale(ctx)
				if err != nil {
					return err
				}
				reconciled, err := env.Ledger.Reconcile(ctx)
				if err != nil {
					return err
				}
				if env.Logger != nil {
					env.Logger.InfoContext(ctx, "usage reconcile complete",
						"component", "usage",
						"operation", "reconcile",
						"expired", expired,
						"reconciled", reconciled,
					)
				}
				_, err = fmt.Fprintf(cmd.OutOrStdout(), "expired: %d\nreconciled: %d\n", expired, reconciled)
				return err
			})
		},
	}
}
