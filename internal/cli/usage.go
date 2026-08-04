package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/anggasct/aura/internal/config"
	"github.com/anggasct/aura/internal/logging"
	"github.com/anggasct/aura/internal/usage"
)

const defaultPricesFilename = "prices.yaml"

func newUsageCmd(gf *globalFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "usage",
		Short: "Model spend ledger: budget status, entries, prices, and reconciliation",
	}
	cmd.AddCommand(
		newUsageStatusCmd(gf),
		newUsageEntriesCmd(gf),
		newUsagePricesCmd(gf),
		newUsageReconcileCmd(gf),
	)
	return cmd
}

// withUsage loads config, opens the live database, builds a ledger, and runs
// fn. The price registry is loaded from the operator price file; a missing
// default file is not an error.
func withUsage(cmd *cobra.Command, gf *globalFlags, pricesPath string, fn func(context.Context, *slog.Logger, *config.Config, *usage.Ledger, *usage.PriceRegistry) error) error {
	result, err := config.Load(gf.configPath)
	if err != nil {
		return err
	}
	cfg := result.Config
	level, format := resolveLogging(cmd, cfg)
	logger, err := logging.Setup(level, format, os.Stderr)
	if err != nil {
		return err
	}
	logConfigResult(cmd.Context(), logger, &result)

	db, err := openStorage(cmd.Context(), cfg)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()

	reg := usage.NewPriceRegistry()
	if err := loadPrices(cmd.Context(), logger, result.Path, pricesPath, reg); err != nil {
		return err
	}

	ledger, err := usage.NewLedger(db, usage.LedgerOptions{
		Prices:           reg,
		Currency:         cfg.Usage.Currency,
		DailyCapMicros:   cfg.Usage.DailyBudgetMicros,
		MonthlyCapMicros: cfg.Usage.MonthlyBudgetMicros,
		ReservationTTL:   time.Duration(cfg.Usage.ReservationTTL),
		Logger:           logger,
	})
	if err != nil {
		return err
	}
	return fn(cmd.Context(), logger, cfg, ledger, reg)
}

// loadPrices reads the operator price file into reg. An explicit path wins;
// otherwise prices.yaml beside the config file is used. A missing default
// file is not an error (no prices registered); a missing explicit file is.
func loadPrices(ctx context.Context, logger *slog.Logger, configPath, explicit string, reg *usage.PriceRegistry) error {
	path := explicit
	explicitGiven := explicit != ""
	if path == "" {
		path = filepath.Join(filepath.Dir(configPath), defaultPricesFilename)
	}
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) && !explicitGiven {
			return nil
		}
		return fmt.Errorf("usage: cannot read price file: %w", err)
	}
	if err := usage.LoadPricesFile(path, reg); err != nil {
		return err
	}
	logger.DebugContext(ctx, "loaded price records", "component", "usage", "file", filepath.Base(path), "prices", len(reg.All()))
	return nil
}

// table is a tabwriter that records the first write error so callers print
// many rows and check once.
type table struct {
	w   *tabwriter.Writer
	err error
}

func newTable(out io.Writer) *table {
	return &table{w: tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)}
}

func (t *table) printf(format string, args ...any) {
	if t.err != nil {
		return
	}
	_, t.err = fmt.Fprintf(t.w, format, args...)
}

func (t *table) flush() error {
	if t.err != nil {
		return t.err
	}
	return t.w.Flush()
}

func newUsageStatusCmd(gf *globalFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Report current day/month spend against budget caps",
		RunE: func(cmd *cobra.Command, args []string) error {
			return withUsage(cmd, gf, "", func(ctx context.Context, logger *slog.Logger, cfg *config.Config, ledger *usage.Ledger, _ *usage.PriceRegistry) error {
				st, err := ledger.Status(ctx)
				if err != nil {
					return err
				}
				tbl := newTable(cmd.OutOrStdout())
				tbl.printf("window\tday\tmonth\n")
				tbl.printf("used (micros)\t%d\t%d\n", st.DayUsedMicros(), st.MonthUsedMicros())
				tbl.printf("reserved (micros)\t%d\t%d\n", st.DayReservedMicros, st.MonthReservedMicros)
				tbl.printf("settled (micros)\t%d\t%d\n", st.DaySettledMicros, st.MonthSettledMicros)
				tbl.printf("cap (micros)\t%d\t%d\n", st.DailyCapMicros, st.MonthlyCapMicros)
				tbl.printf("remaining (micros)\t%d\t%d\n", st.DayRemainingMicros(), st.MonthRemainingMicros())
				tbl.printf("period\t%s\t%s\n", st.Day, st.Month)
				if err := tbl.flush(); err != nil {
					return err
				}
				_, err = fmt.Fprintf(cmd.OutOrStdout(), "active reservations: %d\n", st.ActiveReservations)
				return err
			})
		},
	}
}

func newUsageEntriesCmd(gf *globalFlags) *cobra.Command {
	var limit int
	cmd := &cobra.Command{
		Use:   "entries",
		Short: "List settled usage entries, newest first",
		Args: func(cmd *cobra.Command, args []string) error {
			if limit < 0 {
				return &usageError{errors.New("usage entries --limit must not be negative")}
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			return withUsage(cmd, gf, "", func(ctx context.Context, logger *slog.Logger, cfg *config.Config, ledger *usage.Ledger, _ *usage.PriceRegistry) error {
				entries, err := ledger.Entries(ctx, limit)
				if err != nil {
					return err
				}
				if len(entries) == 0 {
					_, err := fmt.Fprintln(cmd.OutOrStdout(), "no usage entries")
					return err
				}
				tbl := newTable(cmd.OutOrStdout())
				tbl.printf("recorded_at\treservation_id\tmodel\tinput\toutput\tcost_micros\taccounting\tprice_version\n")
				for i := range entries {
					en := &entries[i]
					tbl.printf("%s\t%s\t%s\t%d\t%d\t%d\t%s\t%s\n",
						en.RecordedAt.UTC().Format("2006-01-02T15:04:05Z"),
						en.ReservationID, en.ModelDefinitionID, en.InputTokens, en.OutputTokens,
						en.CostMicros, en.Accounting, en.PriceVersion)
				}
				return tbl.flush()
			})
		},
	}
	cmd.Flags().IntVar(&limit, "limit", 50, "maximum entries to list")
	return cmd
}

func newUsagePricesCmd(gf *globalFlags) *cobra.Command {
	var pricesPath string
	cmd := &cobra.Command{
		Use:   "prices",
		Short: "List versioned price records from the operator price file",
		RunE: func(cmd *cobra.Command, args []string) error {
			return withUsage(cmd, gf, pricesPath, func(ctx context.Context, logger *slog.Logger, cfg *config.Config, _ *usage.Ledger, reg *usage.PriceRegistry) error {
				prices := reg.All()
				if len(prices) == 0 {
					_, err := fmt.Fprintln(cmd.OutOrStdout(), "no price records")
					return err
				}
				tbl := newTable(cmd.OutOrStdout())
				tbl.printf("model\tcurrency\tin\tout\tcache\treasoning\teffective_from\teffective_to\trate\tsource\n")
				for i := range prices {
					p := &prices[i]
					to := "open"
					if !p.EffectiveTo.IsZero() {
						to = p.EffectiveTo.UTC().Format("2006-01-02")
					}
					tbl.printf("%s\t%s\t%d\t%d\t%d\t%d\t%s\t%s\t%d%%\t%s\n",
						p.ModelDefinitionID, p.Currency,
						p.MicrosPerInputToken, p.MicrosPerOutputToken,
						p.MicrosPerCacheToken, p.MicrosPerReasoningToken,
						p.EffectiveFrom.UTC().Format("2006-01-02"), to,
						p.MaxReservationRate, p.Source)
				}
				return tbl.flush()
			})
		},
	}
	cmd.Flags().StringVar(&pricesPath, "prices", "", "path to the operator price file (default: prices.yaml beside config)")
	return cmd
}

func newUsageReconcileCmd(gf *globalFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "reconcile",
		Short: "Expire stale reservations and release expired ones; never deletes rows",
		RunE: func(cmd *cobra.Command, args []string) error {
			return withUsage(cmd, gf, "", func(ctx context.Context, logger *slog.Logger, cfg *config.Config, ledger *usage.Ledger, _ *usage.PriceRegistry) error {
				expired, err := ledger.ExpireStale(ctx)
				if err != nil {
					return err
				}
				reconciled, err := ledger.Reconcile(ctx)
				if err != nil {
					return err
				}
				logger.InfoContext(ctx, "usage reconcile complete",
					"component", "usage",
					"operation", "reconcile",
					"expired", expired,
					"reconciled", reconciled,
				)
				_, err = fmt.Fprintf(cmd.OutOrStdout(), "expired: %d\nreconciled: %d\n", expired, reconciled)
				return err
			})
		},
	}
}
