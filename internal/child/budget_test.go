package child

import (
	"sync"
	"testing"
)

func TestLedgerChargeEnforcesCapUnderConcurrency(t *testing.T) {
	ledger := NewLedger(nil, 0)
	reservation, err := ledger.Reserve(t.Context(), "inv-1", "owner-1", 64, 640)
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	const workers = 8
	const perWorker = 16
	var wg sync.WaitGroup
	succeeded := make([]int, workers)
	for i := range workers {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for range perWorker {
				if err := ledger.Charge(t.Context(), reservation.ID, 1, 10); err == nil {
					succeeded[i]++
				} else if code, ok := CodeOf(err); !ok || code != ErrorCodeBudgetExceeded {
					t.Errorf("unexpected charge error: %v", err)
					return
				}
			}
		}(i)
	}
	wg.Wait()
	total := 0
	for _, n := range succeeded {
		total += n
	}
	if total != 64 {
		t.Fatalf("charged total = %d, want 64", total)
	}
	if tokens, _ := ledger.ParentUsage("inv-1"); tokens != 64 {
		t.Fatalf("parent tokens = %d, want 64", tokens)
	}
	if tokens, _ := ledger.OwnerUsage("owner-1"); tokens != 64 {
		t.Fatalf("owner tokens = %d, want 64", tokens)
	}
	if err := ledger.Charge(t.Context(), reservation.ID, 1, 1); err == nil {
		t.Fatal("charge past the cap must fail")
	} else if code, ok := CodeOf(err); !ok || code != ErrorCodeBudgetExceeded {
		t.Fatalf("code = %v, %v (%v)", code, ok, err)
	}
}

func TestLedgerAggregatesMoveTogether(t *testing.T) {
	ledger := NewLedger(nil, 0)
	first, err := ledger.Reserve(t.Context(), "inv-1", "owner-1", 1000, 1000)
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	second, err := ledger.Reserve(t.Context(), "inv-1", "owner-2", 1000, 1000)
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	if err := ledger.Charge(t.Context(), first.ID, 10, 5); err != nil {
		t.Fatalf("Charge: %v", err)
	}
	if err := ledger.Charge(t.Context(), second.ID, 4, 2); err != nil {
		t.Fatalf("Charge: %v", err)
	}
	if tokens, cost := ledger.ParentUsage("inv-1"); tokens != 14 || cost != 7 {
		t.Fatalf("parent usage = (%d, %d), want (14, 7)", tokens, cost)
	}
	if tokens, cost := ledger.OwnerUsage("owner-1"); tokens != 10 || cost != 5 {
		t.Fatalf("owner-1 usage = (%d, %d), want (10, 5)", tokens, cost)
	}
	if tokens, cost := ledger.OwnerUsage("owner-2"); tokens != 4 || cost != 2 {
		t.Fatalf("owner-2 usage = (%d, %d), want (4, 2)", tokens, cost)
	}
}

func TestLedgerReleaseEndsCharging(t *testing.T) {
	ledger := NewLedger(nil, 0)
	reservation, err := ledger.Reserve(t.Context(), "inv-1", "owner-1", 100, 100)
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	if err := ledger.Charge(t.Context(), reservation.ID, 10, 10); err != nil {
		t.Fatalf("Charge: %v", err)
	}
	if err := ledger.Release(t.Context(), reservation.ID); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if err := ledger.Release(t.Context(), reservation.ID); err != nil {
		t.Fatalf("second Release must stay idempotent: %v", err)
	}
	if err := ledger.Charge(t.Context(), reservation.ID, 1, 1); err == nil {
		t.Fatal("charge after release must fail")
	} else if code, ok := CodeOf(err); !ok || code != ErrorCodeChildConflict {
		t.Fatalf("code = %v, %v (%v)", code, ok, err)
	}
	if tokens, _ := ledger.ParentUsage("inv-1"); tokens != 10 {
		t.Fatalf("released usage must stay accounted: %d", tokens)
	}
}

func TestLedgerRejectsBadInput(t *testing.T) {
	ledger := NewLedger(nil, 0)
	if _, err := ledger.Reserve(t.Context(), "", "owner-1", 1, 1); err == nil {
		t.Error("expected empty invocation rejection")
	}
	if _, err := ledger.Reserve(t.Context(), "inv-1", "owner-1", -1, 1); err == nil {
		t.Error("expected negative cap rejection")
	}
	if err := ledger.Charge(t.Context(), "missing", 1, 1); err == nil {
		t.Error("expected unknown reservation rejection")
	} else if code, ok := CodeOf(err); !ok || code != ErrorCodeChildNotFound {
		t.Fatalf("code = %v, %v (%v)", code, ok, err)
	}
	reservation, err := ledger.Reserve(t.Context(), "inv-1", "owner-1", 100, 100)
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	if err := ledger.Charge(t.Context(), reservation.ID, -1, 0); err == nil {
		t.Error("expected negative usage rejection")
	}
	if err := ledger.Release(t.Context(), "missing"); err == nil {
		t.Error("expected unknown release rejection")
	}
}
