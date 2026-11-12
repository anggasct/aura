package sqlite

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/anggasct/aura/internal/platform/durable"
)

const refuseLeaseSQL = `CREATE TRIGGER refuse_lease BEFORE INSERT ON durable_host BEGIN SELECT RAISE(ABORT, 'lease refused'); END`

func TestAcquireWritesTheLeaseBeforeRun(t *testing.T) {
	path := newDatabaseFile(t)
	db := openDB(t, path)
	rt := newRuntime(t, openDB(t, path), nil)
	ran := make(chan struct{}, 1)
	rt.RegisterHandler("job", func(context.Context, durable.Invocation) error {
		ran <- struct{}{}
		return nil
	})
	ref := mustStart(t, rt, durable.StartRequest{Handler: "job", Key: "k-acquire"})
	if err := rt.Acquire(t.Context()); err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if n := countRows(t, db, `SELECT COUNT(*) FROM durable_host`); n != 1 {
		t.Fatalf("lease rows after Acquire = %d, want 1", n)
	}
	var owner string
	if err := db.QueryRowContext(t.Context(), `SELECT owner FROM durable_host`).Scan(&owner); err != nil {
		t.Fatalf("read the lease: %v", err)
	}
	if row := readRun(t, db, ref.Key); row.owner.Valid {
		t.Fatalf("run owner after Acquire = %q, want none", row.owner.String)
	}
	h := startHost(t, rt)
	receive(t, ran)
	waitState(t, rt, runWant{key: ref.Key, state: durable.RunSucceeded})
	if n := countRows(t, db, `SELECT COUNT(*) FROM durable_host WHERE owner = ?`, owner); n != 1 {
		t.Fatalf("leases of the acquired owner while serving = %d, want 1", n)
	}
	if n := countRows(t, db, `SELECT COUNT(*) FROM durable_host`); n != 1 {
		t.Fatalf("lease rows while serving = %d, want 1", n)
	}
	if err := h.stop(t); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if n := countRows(t, db, `SELECT COUNT(*) FROM durable_host`); n != 0 {
		t.Fatalf("lease rows after the stop = %d, want 0", n)
	}
}

func TestAcquireRejectsASecondHost(t *testing.T) {
	rt := newRuntime(t, openDB(t, newDatabaseFile(t)), nil)
	if err := rt.Acquire(t.Context()); err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	requireCode(t, rt.Acquire(t.Context()), ErrorCodeHostRunning)
	startHost(t, rt)
	requireCode(t, rt.Run(t.Context()), ErrorCodeHostRunning)
	running := newRuntime(t, openDB(t, newDatabaseFile(t)), nil)
	startHost(t, running)
	requireCode(t, running.Acquire(t.Context()), ErrorCodeHostRunning)
}

func TestAcquireFailsWithoutALease(t *testing.T) {
	path := newDatabaseFile(t)
	execSQL(t, openDB(t, path), refuseLeaseSQL)
	rt := newRuntime(t, openDB(t, path), nil)
	err := rt.Acquire(t.Context())
	if detail := requireCode(t, err, ErrorCodeHostLease); !strings.Contains(detail, "lease refused") {
		t.Fatalf("lease error detail = %q, want the cause", detail)
	}
	if cause := errors.Unwrap(err); cause == nil || !strings.Contains(cause.Error(), "lease refused") {
		t.Fatalf("lease error cause = %v, want the refused insert", cause)
	}
	requireCode(t, rt.Acquire(t.Context()), ErrorCodeHostRunning)
	requireCode(t, rt.Run(t.Context()), ErrorCodeHostRunning)
	second := newRuntime(t, openDB(t, path), nil)
	requireCode(t, second.Run(t.Context()), ErrorCodeHostLease)
}
