package sqlite

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"

	"github.com/anggasct/aura/internal/platform/durable"
)

func journalingHandler(actions chan<- string) durable.Handler {
	return func(ctx context.Context, inv durable.Invocation) error {
		payload := string(inv.Payload())
		out, err := inv.RunAction(ctx, "step", func(context.Context) ([]byte, error) {
			actions <- payload
			return []byte(payload), nil
		})
		if err != nil {
			return err
		}
		if string(out) != payload {
			return fmt.Errorf("action replayed %q", out)
		}
		if payload != "first" {
			return nil
		}
		if err := inv.Sleep(0); err != nil {
			return err
		}
		if _, ok := inv.Signal(ctx, "go"); !ok {
			return ctx.Err()
		}
		if _, _, ok := inv.Wait(ctx, "answer", 0); !ok {
			return ctx.Err()
		}
		return errors.New("first run fails")
	}
}

func TestReuseEndedDropsTheOldJournal(t *testing.T) {
	t.Run("the new run starts with an empty journal", reuseStartsWithAnEmptyJournal)
	t.Run("a late write of the old run is refused", reuseFencesTheOldRun)
	t.Run("a failed insert keeps the old run", reuseKeepsTheOldRunWhenTheInsertFails)
}

func reuseStartsWithAnEmptyJournal(t *testing.T) {
	path := newDatabaseFile(t)
	db := openDB(t, path)
	actions := make(chan string, 2)
	register := func(rt *Runtime) { rt.RegisterHandler("journal", journalingHandler(actions)) }
	first := newRuntime(t, openDB(t, path), nil)
	register(first)
	h := startHost(t, first)
	ref := mustStart(t, first, durable.StartRequest{Handler: "journal", Key: "k-reused", Payload: []byte("first")})
	if err := first.ResolveApproval(t.Context(), durable.ResolveApprovalRequest{ApprovalID: durable.FormatApprovalAddr(ref.Key, "decision")}); err != nil {
		t.Fatalf("ResolveApproval: %v", err)
	}
	mustSignal(t, first, sentSignal{ref: ref, name: "go", payload: "x"})
	if got := receive(t, actions); got != "first" {
		t.Fatalf("first action = %q", got)
	}
	waitState(t, first, runWant{key: ref.Key, state: durable.RunFailed})
	if got := childRows(t, db, ref.Key); len(got) != len(childTables) {
		t.Fatalf("journal of the first run = %v, want a row in every child table", got)
	}
	ended := readRun(t, db, ref.Key)
	if err := h.stop(t); err != nil {
		t.Fatalf("stop: %v", err)
	}
	client := newRuntime(t, openDB(t, path), nil)
	client.DeclareHandlers("journal")
	mustStart(t, client, durable.StartRequest{Handler: "journal", Key: ref.Key, Payload: []byte("second"), Reuse: durable.ReuseEnded})
	if got := childRows(t, db, ref.Key); len(got) != 0 {
		t.Fatalf("journal of the replaced run = %v, want none", got)
	}
	replaced := readRun(t, db, ref.Key)
	if replaced.state != string(durable.RunRunning) || replaced.detail != "" || replaced.owner.Valid || replaced.cancelRequested || replaced.claim != ended.claim {
		t.Fatalf("replaced run = %+v, want running, unowned, no cancel request, claim %d", replaced, ended.claim)
	}
	if n := countRows(t, db, `SELECT COUNT(*) FROM durable_run WHERE key = ? AND payload = ?`, ref.Key, []byte("second")); n != 1 {
		t.Fatalf("runs with the new payload = %d, want 1", n)
	}
	second := restart(t, &restartPlan{path: path, register: register})
	if got := receive(t, actions); got != "second" {
		t.Fatalf("action of the new run = %q, want it to run", got)
	}
	waitState(t, second, runWant{key: ref.Key, state: durable.RunSucceeded})
	if row := readRun(t, db, ref.Key); row.claim != ended.claim+1 {
		t.Fatalf("claim of the new run = %d, want %d", row.claim, ended.claim+1)
	}
}

func reuseFencesTheOldRun(t *testing.T) {
	db := openDB(t, newDatabaseFile(t))
	rt := newRuntime(t, db, nil)
	entered := make(chan string, 2)
	late := make(chan struct{})
	lateResult := make(chan error, 1)
	rt.RegisterHandler("job", func(ctx context.Context, inv durable.Invocation) error {
		entered <- string(inv.Payload())
		if string(inv.Payload()) == "second" {
			return parkUntilStopped(ctx)
		}
		go func() {
			select {
			case <-late:
			case <-t.Context().Done():
				return
			}
			_, err := inv.RunAction(ctx, "late", func(context.Context) ([]byte, error) { return []byte("stale"), nil })
			lateResult <- err
		}()
		return errors.New("first run fails")
	})
	startHost(t, rt)
	ref := mustStart(t, rt, durable.StartRequest{Handler: "job", Key: "k-fenced", Payload: []byte("first")})
	receive(t, entered)
	waitState(t, rt, runWant{key: ref.Key, state: durable.RunFailed})
	ended := readRun(t, db, ref.Key)
	mustStart(t, rt, durable.StartRequest{Handler: "job", Key: ref.Key, Payload: []byte("second"), Reuse: durable.ReuseEnded})
	if got := receive(t, entered); got != "second" {
		t.Fatalf("second invocation payload = %q", got)
	}
	reclaimed := readRun(t, db, ref.Key)
	if reclaimed.owner.String != rt.hostID || reclaimed.claim != ended.claim+1 {
		t.Fatalf("reclaimed run = %+v, want owned by the same host under claim %d", reclaimed, ended.claim+1)
	}
	close(late)
	requireCode(t, receive(t, lateResult), ErrorCodeOwnershipLost)
	if n := countRows(t, db, `SELECT COUNT(*) FROM durable_action WHERE run_key = ?`, ref.Key); n != 0 {
		t.Fatalf("action rows of the new run = %d, want 0", n)
	}
	if row := readRun(t, db, ref.Key); row != reclaimed {
		t.Fatalf("run after the late write = %+v, want %+v", row, reclaimed)
	}
}

func reuseKeepsTheOldRunWhenTheInsertFails(t *testing.T) {
	db := openDB(t, newDatabaseFile(t))
	rt := newRuntime(t, db, nil)
	rt.RegisterHandler("job", func(ctx context.Context, inv durable.Invocation) error {
		if _, err := inv.RunAction(ctx, "step", func(context.Context) ([]byte, error) { return []byte("x"), nil }); err != nil {
			return err
		}
		return errors.New("first run fails")
	})
	startHost(t, rt)
	ref := mustStart(t, rt, durable.StartRequest{Handler: "job", Key: "k-refused", Payload: []byte("first")})
	waitState(t, rt, runWant{key: ref.Key, state: durable.RunFailed})
	execSQL(t, db, `CREATE TRIGGER durable_run_refuse_insert BEFORE INSERT ON durable_run WHEN NEW.key = 'k-refused' BEGIN SELECT RAISE(ABORT, 'insert refused'); END`)
	before := readRun(t, db, ref.Key)
	if _, err := rt.Start(t.Context(), durable.StartRequest{Handler: "job", Key: ref.Key, Payload: []byte("second"), Reuse: durable.ReuseEnded}); err == nil {
		t.Fatal("Start succeeded although the insert was refused")
	}
	if after := readRun(t, db, ref.Key); after != before {
		t.Fatalf("run after the refused insert = %+v, want %+v", after, before)
	}
	if n := countRows(t, db, `SELECT COUNT(*) FROM durable_action WHERE run_key = ? AND action_key = 'step'`, ref.Key); n != 1 {
		t.Fatalf("action rows = %d, want the old one", n)
	}
}

func TestReuseEndedRefusesAKeyOfAnotherHandler(t *testing.T) {
	db := openDB(t, newDatabaseFile(t))
	rt := newRuntime(t, db, nil)
	rt.DeclareHandlers("job", "other")
	insertRun(t, db, runFixture{key: "k-ended", handler: "job", claim: 2})
	execSQL(t, db, `UPDATE durable_run SET state = 'failed', detail = 'boom' WHERE key = 'k-ended'`)
	insertRun(t, db, runFixture{key: "k-running", handler: "job", claim: 1})
	for _, key := range []string{"k-ended", "k-running"} {
		insertChildren(t, db, key)
		before := readRun(t, db, key)
		_, err := rt.Start(t.Context(), durable.StartRequest{Handler: "other", Key: key, Payload: []byte("other"), Reuse: durable.ReuseEnded})
		requireCode(t, err, ErrorCodeKeyInUse)
		if !errors.Is(err, durable.ErrKeyInUse) {
			t.Fatalf("err = %v, want ErrKeyInUse", err)
		}
		if after := readRun(t, db, key); after != before {
			t.Fatalf("run %q = %+v, want %+v", key, after, before)
		}
		if got := childRows(t, db, key); len(got) != len(childTables) {
			t.Fatalf("journal of %q = %v, want it kept", key, got)
		}
		if n := countRows(t, db, `SELECT COUNT(*) FROM durable_run WHERE key = ? AND handler = 'job' AND payload = ?`, key, []byte{}); n != 1 {
			t.Fatalf("run %q changed its handler or payload", key)
		}
	}
}

func TestReuseEndedNeedsAKnownHandler(t *testing.T) {
	path := newDatabaseFile(t)
	db := openDB(t, path)
	server := newRuntime(t, openDB(t, path), nil)
	payloads := make(chan string, 2)
	server.RegisterHandler("job", func(_ context.Context, inv durable.Invocation) error {
		payloads <- string(inv.Payload())
		return nil
	})
	startHost(t, server)
	ref := mustStart(t, server, durable.StartRequest{Handler: "job", Key: "k-known", Payload: []byte("first")})
	waitState(t, server, runWant{key: ref.Key, state: durable.RunSucceeded})
	before := readRun(t, db, ref.Key)
	client := newRuntime(t, openDB(t, path), nil)
	req := durable.StartRequest{Handler: "job", Key: ref.Key, Payload: []byte("second"), Reuse: durable.ReuseEnded}
	_, err := client.Start(t.Context(), req)
	requireCode(t, err, ErrorCodeHandlerNotRegistered)
	if after := readRun(t, db, ref.Key); after != before {
		t.Fatalf("run after a refused start = %+v, want %+v", after, before)
	}
	client.DeclareHandlers("job")
	mustStart(t, client, req)
	waitState(t, client, runWant{key: ref.Key, state: durable.RunSucceeded})
	got := []string{receive(t, payloads), receive(t, payloads)}
	if !slices.Equal(got, []string{"first", "second"}) {
		t.Fatalf("invocations = %q, want first and second", got)
	}
	if row := readRun(t, db, ref.Key); row.claim != before.claim+1 {
		t.Fatalf("claim = %d, want %d", row.claim, before.claim+1)
	}
}
