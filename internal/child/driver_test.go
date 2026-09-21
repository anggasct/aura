package child

import (
	stdcontext "context"
	"errors"
	"sync"
	"testing"
	"time"
)

type fakeLauncher struct {
	mu      sync.Mutex
	starts  []StartRequest
	fail    error
	keyOver string
}

func (f *fakeLauncher) Start(_ stdcontext.Context, req StartRequest) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.starts = append(f.starts, req)
	if f.fail != nil {
		return "", f.fail
	}
	if f.keyOver != "" {
		return f.keyOver, nil
	}
	return req.DurableKey, nil
}

type fakeLedger struct {
	mu       sync.Mutex
	reserved []string
	owners   []string
	released []string
	charged  []string
	fail     error
}

func (f *fakeLedger) Reserve(_ stdcontext.Context, invocationID, ownerID string, maxTokens, maxCost int64) (BudgetReservation, error) {
	if f.fail != nil {
		return BudgetReservation{}, f.fail
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reserved = append(f.reserved, invocationID)
	f.owners = append(f.owners, ownerID)
	return BudgetReservation{ID: "res-" + invocationID, ExpiresAt: time.Now().UTC().Add(time.Hour)}, nil
}

func (f *fakeLedger) Charge(_ stdcontext.Context, reservationID string, tokens, cost int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.charged = append(f.charged, reservationID)
	return nil
}

func (f *fakeLedger) Release(_ stdcontext.Context, reservationID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.released = append(f.released, reservationID)
	return nil
}

func testDriver(t *testing.T) (*Driver, *fakeLauncher, *fakeLedger) {
	t.Helper()
	service, err := NewService(newFakeRegistry())
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	launcher := &fakeLauncher{}
	ledger := &fakeLedger{}
	driver, err := NewDriver(service, launcher, ledger, nil)
	if err != nil {
		t.Fatalf("NewDriver: %v", err)
	}
	return driver, launcher, ledger
}

func TestDriverStartsInvocation(t *testing.T) {
	driver, launcher, ledger := testDriver(t)
	now := time.Now().UTC()
	spawn, created, err := driver.RunChild(t.Context(), testSpec(), now)
	if err != nil || !created {
		t.Fatalf("RunChild: %+v, %v, %v", spawn, created, err)
	}
	if len(launcher.starts) != 1 || launcher.starts[0].DurableKey != "child/ch-1" {
		t.Fatalf("starts = %+v", launcher.starts)
	}
	if len(ledger.reserved) != 1 || len(ledger.released) != 0 {
		t.Fatalf("reserved = %v released = %v", ledger.reserved, ledger.released)
	}
}

func TestDriverReleasesOnSpawnConflict(t *testing.T) {
	driver, _, ledger := testDriver(t)
	now := time.Now().UTC()
	if _, _, err := driver.RunChild(t.Context(), testSpec(), now); err != nil {
		t.Fatalf("RunChild: %v", err)
	}
	altered := testSpec()
	altered.Task = "different work"
	if _, _, err := driver.RunChild(t.Context(), altered, now); err == nil {
		t.Fatal("altered replay must fail")
	}
	if len(ledger.reserved) != 2 || len(ledger.released) != 1 {
		t.Fatalf("reserved = %v released = %v", ledger.reserved, ledger.released)
	}
}

func TestDriverReusesExistingChild(t *testing.T) {
	driver, launcher, ledger := testDriver(t)
	now := time.Now().UTC()
	if _, _, err := driver.RunChild(t.Context(), testSpec(), now); err != nil {
		t.Fatalf("RunChild: %v", err)
	}
	second, created, err := driver.RunChild(t.Context(), testSpec(), now)
	if err != nil || created || second.ID != "ch-1" {
		t.Fatalf("idempotent replay: %+v, %v, %v", second, created, err)
	}
	if len(launcher.starts) != 1 {
		t.Fatalf("replay must not start a second invocation: %+v", launcher.starts)
	}
	if len(ledger.reserved) != 2 || len(ledger.released) != 1 {
		t.Fatalf("reserved = %v released = %v", ledger.reserved, ledger.released)
	}
}

func TestDriverRejectsKeyMismatch(t *testing.T) {
	service, err := NewService(newFakeRegistry())
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	launcher := &fakeLauncher{keyOver: "child/wrong"}
	driver, err := NewDriver(service, launcher, &fakeLedger{}, nil)
	if err != nil {
		t.Fatalf("NewDriver: %v", err)
	}
	if _, _, err := driver.RunChild(t.Context(), testSpec(), time.Now().UTC()); err == nil {
		t.Fatal("durable key mismatch must fail")
	} else if code, ok := CodeOf(err); !ok || code != ErrorCodeChildInvalid {
		t.Fatalf("code = %v, %v (%v)", code, ok, err)
	}
}

func TestDriverPropagatesFailures(t *testing.T) {
	service, err := NewService(newFakeRegistry())
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	brokenLedger := &fakeLedger{fail: errors.New("ledger down")}
	driver, err := NewDriver(service, &fakeLauncher{}, brokenLedger, nil)
	if err != nil {
		t.Fatalf("NewDriver: %v", err)
	}
	if _, _, err := driver.RunChild(t.Context(), testSpec(), time.Now().UTC()); err == nil {
		t.Error("expected ledger failure")
	}
	brokenLauncher := &fakeLauncher{fail: errors.New("durable down")}
	driver, err = NewDriver(service, brokenLauncher, &fakeLedger{}, nil)
	if err != nil {
		t.Fatalf("NewDriver: %v", err)
	}
	if _, _, err := driver.RunChild(t.Context(), testSpec(), time.Now().UTC()); err == nil {
		t.Error("expected launcher failure")
	}
	if _, err := NewDriver(nil, &fakeLauncher{}, &fakeLedger{}, nil); err == nil {
		t.Error("expected nil service rejection")
	}
	if _, err := NewDriver(service, nil, &fakeLedger{}, nil); err == nil {
		t.Error("expected nil launcher rejection")
	}
	if _, err := NewDriver(service, &fakeLauncher{}, nil, nil); err == nil {
		t.Error("expected nil ledger rejection")
	}
	var nilCtx stdcontext.Context
	driver, _, _ = testDriver(t)
	if _, _, err := driver.RunChild(nilCtx, testSpec(), time.Now().UTC()); err == nil {
		t.Error("expected nil context rejection")
	}
}
