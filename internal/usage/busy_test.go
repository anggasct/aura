package usage

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/store"
)

// A write lock held by another connection must not fail the reserve: the
// begin retries transient SQLITE_BUSY until the holder releases. The lock
// is released only after a busy begin failure has actually been observed,
// so the retry path — not lock timing — carries the reserve.
func TestReserveRetriesWhileWriteLockHeld(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	dsn := t.TempDir() + "/usage.db"

	seed, err := store.OpenDB(ctx, dsn)
	if err != nil {
		t.Fatalf("open seed db: %v", err)
	}
	if err := store.Migrate(ctx, seed); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if err := seed.Close(); err != nil {
		t.Fatalf("close seed db: %v", err)
	}

	db, err := store.OpenDBWithOptions(ctx, dsn, store.OpenOptions{BusyTimeout: 50 * time.Millisecond})
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	reg := NewPriceRegistry()
	if err := reg.Put(testPrice("primary")); err != nil {
		t.Fatal(err)
	}
	l, err := NewLedger(db, LedgerOptions{
		Prices:           reg,
		Currency:         "USD",
		DailyCapMicros:   1000000,
		MonthlyCapMicros: 10000000,
		ReservationTTL:   time.Hour,
	})
	if err != nil {
		t.Fatalf("NewLedger: %v", err)
	}

	holder, err := store.OpenDBWithOptions(ctx, dsn, store.OpenOptions{BusyTimeout: 50 * time.Millisecond})
	if err != nil {
		t.Fatalf("open holder: %v", err)
	}
	t.Cleanup(func() { _ = holder.Close() })
	lockTx, err := holder.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin holder tx: %v", err)
	}

	busyFailures := 0
	busyObserved := make(chan struct{}, busyMaxAttempts)
	setBusyBeginObserver(func() {
		busyFailures++
		busyObserved <- struct{}{}
	})
	t.Cleanup(func() { setBusyBeginObserver(nil) })

	reserveDone := make(chan error, 1)
	go func() {
		_, rerr := l.Reserve(ctx, ReserveRequest{
			InvocationID:             "inv-retry",
			ModelDefinitionID:        "primary",
			KnownInputTokens:         100,
			RequestedMaxOutputTokens: 200,
		})
		reserveDone <- rerr
	}()

	select {
	case <-busyObserved:
	case <-ctx.Done():
		t.Fatalf("no busy begin observed before timeout: %v", ctx.Err())
	}
	if err := lockTx.Rollback(); err != nil {
		t.Fatalf("release lock: %v", err)
	}

	select {
	case err := <-reserveDone:
		if err != nil {
			t.Fatalf("Reserve under contention = %v, want success after retry", err)
		}
	case <-ctx.Done():
		<-reserveDone
		t.Fatalf("Reserve did not finish: %v", ctx.Err())
	}
	// Reading busyFailures is safe here: the reserve goroutine finished,
	// and every observer call precedes Reserve returning.
	if busyFailures != 1 {
		t.Fatalf("busy begin failures = %d, want exactly 1 before the release", busyFailures)
	}
}

// A permanent failure at BEGIN surfaces immediately; only SQLITE_BUSY is
// retried.
func TestBeginTxDoesNotRetryPermanentErrors(t *testing.T) {
	var attempts int
	db := &fakeBeginner{onBegin: func() { attempts++ }, result: errBoom}
	if _, err := beginTx(context.Background(), db, "probe"); !errors.Is(err, errBoom) {
		t.Fatalf("err = %v, want boom", err)
	}
	if attempts != 1 {
		t.Errorf("attempts = %d, want exactly 1 (no retry for non-busy)", attempts)
	}
}

// A busy BEGIN that never clears exhausts its attempt budget instead of
// spinning forever: a real holder connection keeps the write lock while the
// probe connection runs with a tiny busy_timeout, so every attempt fails with
// genuine driver SQLITE_BUSY. The observed busy count pins the budget
// without timing assumptions.
func TestBeginTxBoundedUnderSustainedBusy(t *testing.T) {
	ctx := context.Background()
	dsn := t.TempDir() + "/usage.db"

	seed, err := store.OpenDB(ctx, dsn)
	if err != nil {
		t.Fatalf("open seed db: %v", err)
	}
	if err := store.Migrate(ctx, seed); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if err := seed.Close(); err != nil {
		t.Fatalf("close seed db: %v", err)
	}

	holder, err := store.OpenDB(ctx, dsn)
	if err != nil {
		t.Fatalf("open holder: %v", err)
	}
	t.Cleanup(func() { _ = holder.Close() })
	lockTx, err := holder.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin holder tx: %v", err)
	}
	defer func() { _ = lockTx.Rollback() }()

	prober, err := store.OpenDBWithOptions(ctx, dsn, store.OpenOptions{BusyTimeout: 20 * time.Millisecond})
	if err != nil {
		t.Fatalf("open prober: %v", err)
	}
	t.Cleanup(func() { _ = prober.Close() })

	var busyAttempts int
	setBusyBeginObserver(func() { busyAttempts++ })
	t.Cleanup(func() { setBusyBeginObserver(nil) })

	_, err = beginTx(ctx, prober, "probe")
	if err == nil {
		t.Fatal("begin succeeded although the write lock was held")
	}
	if !isTransientBusy(err) {
		t.Fatalf("err = %v (%T), want wrapped driver SQLITE_BUSY", err, err)
	}
	// The final attempt fails without notifying, so the observer sees
	// exactly the retry count.
	if busyAttempts != busyMaxAttempts-1 {
		t.Fatalf("busy begin retries = %d, want %d before giving up", busyAttempts, busyMaxAttempts-1)
	}
}

var errBoom = errors.New("boom")

type fakeBeginner struct {
	onBegin func()
	result  error
}

func (f *fakeBeginner) BeginTx(context.Context, *sql.TxOptions) (*sql.Tx, error) {
	f.onBegin()
	return nil, f.result
}
