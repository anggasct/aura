package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/store"
	"github.com/anggasct/aura/internal/usage"
)

func writeUsageConfig(t *testing.T, dataRoot string) string {
	t.Helper()
	cfg := filepath.Join(t.TempDir(), "config.yaml")
	content := "version: 1\nstorage:\n  path: " + dataRoot +
		"\nusage:\n  currency: USD\n  daily_budget_micros: 1000000\n  monthly_budget_micros: 10000000\n  reservation_ttl: 1h\n"
	if err := os.WriteFile(cfg, []byte(content), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return cfg
}

func seedUsage(t *testing.T, dataRoot string) {
	t.Helper()
	ctx := context.Background()
	db, err := store.OpenDB(ctx, filepath.Join(dataRoot, "aura.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer func() { _ = db.Close() }()
	if err := store.Migrate(ctx, db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	reg := usage.NewPriceRegistry()
	price := &usage.Price{
		ModelDefinitionID:    "primary",
		Currency:             "USD",
		MicrosPerInputToken:  10,
		MicrosPerOutputToken: 30,
		EffectiveFrom:        time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		MaxReservationRate:   200,
		Source:               "test",
	}
	if err := reg.Put(price); err != nil {
		t.Fatal(err)
	}
	ledger, err := usage.NewLedger(db, usage.LedgerOptions{
		Prices: reg, Currency: "USD", DailyCapMicros: 1000000, MonthlyCapMicros: 10000000, ReservationTTL: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	reservation, err := ledger.Reserve(ctx, usage.ReserveRequest{
		InvocationID: "inv-1", Attempt: 0, ModelDefinitionID: "primary",
		KnownInputTokens: 100, RequestedMaxOutputTokens: 200,
	})
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	if _, err := ledger.Settle(ctx, &usage.SettleRequest{
		ReservationID: reservation.ID,
		Usage:         usage.Usage{InputTokens: 80, OutputTokens: 120},
	}); err != nil {
		t.Fatalf("settle: %v", err)
	}
}

func TestUsageStatus(t *testing.T) {
	dataRoot := t.TempDir()
	cfg := writeUsageConfig(t, dataRoot)
	seedUsage(t, dataRoot)

	out, err := execute(t, "usage", "status", "--config", cfg)
	if err != nil {
		t.Fatalf("usage status: %v", err)
	}
	for _, want := range []string{"window", "day", "month", "used (micros)", "cap (micros)", "remaining (micros)", "active reservations: 0"} {
		if !strings.Contains(out, want) {
			t.Errorf("status output missing %q:\n%s", want, out)
		}
	}
}

func TestUsageEntries(t *testing.T) {
	dataRoot := t.TempDir()
	cfg := writeUsageConfig(t, dataRoot)
	seedUsage(t, dataRoot)

	out, err := execute(t, "usage", "entries", "--config", cfg)
	if err != nil {
		t.Fatalf("usage entries: %v", err)
	}
	for _, want := range []string{"recorded_at", "reservation_id", "model", "primary", "reported"} {
		if !strings.Contains(out, want) {
			t.Errorf("entries output missing %q:\n%s", want, out)
		}
	}
}

func TestUsageEntriesNegativeLimitRejected(t *testing.T) {
	dataRoot := t.TempDir()
	cfg := writeUsageConfig(t, dataRoot)

	root := newRootCmd()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"usage", "entries", "--config", cfg, "--limit", "-5"})
	err := root.Execute()
	if err == nil {
		t.Fatal("expected a usage error for negative --limit")
	}
	var ue *usageError
	if !errors.As(err, &ue) {
		t.Errorf("negative --limit must be a usage error (exit 2), got %T: %v", err, err)
	}
}

func TestUsageEntriesEmpty(t *testing.T) {
	dataRoot := t.TempDir()
	cfg := writeUsageConfig(t, dataRoot)

	out, err := execute(t, "usage", "entries", "--config", cfg)
	if err != nil {
		t.Fatalf("usage entries: %v", err)
	}
	if !strings.Contains(out, "no usage entries") {
		t.Errorf("expected empty message, got:\n%s", out)
	}
}

func TestUsagePrices(t *testing.T) {
	dataRoot := t.TempDir()
	cfg := writeUsageConfig(t, dataRoot)
	prices := filepath.Join(t.TempDir(), "prices.yaml")
	content := "version: 1\nprices:\n  - model_definition_id: primary\n    currency: USD\n" +
		"    micros_per_input_token: 10\n    micros_per_output_token: 30\n" +
		"    effective_from: \"2026-01-01T00:00:00Z\"\n    source: test\n    max_reservation_rate: 200\n"
	if err := os.WriteFile(prices, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	out, err := execute(t, "usage", "prices", "--config", cfg, "--prices", prices)
	if err != nil {
		t.Fatalf("usage prices: %v", err)
	}
	for _, want := range []string{"model", "primary", "USD", "200%"} {
		if !strings.Contains(out, want) {
			t.Errorf("prices output missing %q:\n%s", want, out)
		}
	}
}

func TestUsagePricesMissingExplicitFile(t *testing.T) {
	dataRoot := t.TempDir()
	cfg := writeUsageConfig(t, dataRoot)
	_, err := execute(t, "usage", "prices", "--config", cfg, "--prices", filepath.Join(t.TempDir(), "nope.yaml"))
	if err == nil {
		t.Fatal("expected error for missing explicit prices file")
	}
}

func TestUsageReconcile(t *testing.T) {
	dataRoot := t.TempDir()
	cfg := writeUsageConfig(t, dataRoot)
	seedUsage(t, dataRoot)

	out, err := execute(t, "usage", "reconcile", "--config", cfg)
	if err != nil {
		t.Fatalf("usage reconcile: %v", err)
	}
	for _, want := range []string{"expired:", "reconciled:"} {
		if !strings.Contains(out, want) {
			t.Errorf("reconcile output missing %q:\n%s", want, out)
		}
	}
}
