package child

import (
	stdcontext "context"
	"errors"
	"testing"
	"time"
)

func TestDriverReleasesOnLaunchFailure(t *testing.T) {
	service, err := NewService(newFakeRegistry())
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	launcher := &fakeLauncher{fail: errors.New("durable down")}
	ledger := &fakeLedger{}
	driver, err := NewDriver(service, launcher, ledger, nil)
	if err != nil {
		t.Fatalf("NewDriver: %v", err)
	}
	if _, _, err := driver.RunChild(t.Context(), testSpec(), time.Now().UTC()); err == nil {
		t.Fatal("expected launcher failure")
	}
	ledger.mu.Lock()
	defer ledger.mu.Unlock()
	if len(ledger.reserved) != 1 || len(ledger.released) != 1 {
		t.Fatalf("reserved = %v released = %v", ledger.reserved, ledger.released)
	}
	if ledger.released[0] != "res-inv-1" {
		t.Fatalf("released = %v", ledger.released)
	}
}

func TestDriverReleasesOnKeyMismatch(t *testing.T) {
	service, err := NewService(newFakeRegistry())
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	launcher := &fakeLauncher{keyOver: "child/wrong"}
	ledger := &fakeLedger{}
	driver, err := NewDriver(service, launcher, ledger, nil)
	if err != nil {
		t.Fatalf("NewDriver: %v", err)
	}
	if _, _, err := driver.RunChild(t.Context(), testSpec(), time.Now().UTC()); err == nil {
		t.Fatal("expected key mismatch failure")
	} else if code, ok := CodeOf(err); !ok || code != ErrorCodeChildInvalid {
		t.Fatalf("code = %v, %v (%v)", code, ok, err)
	}
	ledger.mu.Lock()
	defer ledger.mu.Unlock()
	if len(ledger.reserved) != 1 || len(ledger.released) != 1 {
		t.Fatalf("reserved = %v released = %v", ledger.reserved, ledger.released)
	}
}

func TestDriverValidatesBeforeReserve(t *testing.T) {
	now := time.Now().UTC()
	invalid := testSpec()
	invalid.Task = ""
	cases := map[string]struct {
		spec *Spec
		when time.Time
	}{
		"nil spec":       {spec: nil, when: now},
		"zero timestamp": {spec: testSpec(), when: time.Time{}},
		"invalid spec":   {spec: invalid, when: now},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			driver, _, ledger := testDriver(t)
			if _, _, err := driver.RunChild(t.Context(), tc.spec, tc.when); err == nil {
				t.Fatal("expected validation failure")
			}
			ledger.mu.Lock()
			defer ledger.mu.Unlock()
			if len(ledger.reserved) != 0 {
				t.Fatalf("validation must not touch the ledger: %v", ledger.reserved)
			}
		})
	}
	t.Run("nil context", func(t *testing.T) {
		var nilCtx stdcontext.Context
		driver, _, ledger := testDriver(t)
		if _, _, err := driver.RunChild(nilCtx, testSpec(), now); err == nil {
			t.Fatal("expected validation failure")
		}
		ledger.mu.Lock()
		defer ledger.mu.Unlock()
		if len(ledger.reserved) != 0 {
			t.Fatalf("validation must not touch the ledger: %v", ledger.reserved)
		}
	})
	t.Run("cancelled", func(t *testing.T) {
		cancelled, cancel := stdcontext.WithCancel(stdcontext.Background())
		cancel()
		driver, _, ledger := testDriver(t)
		if _, _, err := driver.RunChild(cancelled, testSpec(), now); err == nil {
			t.Fatal("expected validation failure")
		}
		ledger.mu.Lock()
		defer ledger.mu.Unlock()
		if len(ledger.reserved) != 0 {
			t.Fatalf("validation must not touch the ledger: %v", ledger.reserved)
		}
	})
}

func TestDriverRejectsInvalidSpecCodes(t *testing.T) {
	driver, _, _ := testDriver(t)
	if _, _, err := driver.RunChild(t.Context(), nil, time.Now().UTC()); err == nil {
		t.Fatal("expected nil spec rejection")
	} else if code, ok := CodeOf(err); !ok || code != ErrorCodeInvalidArgument {
		t.Fatalf("code = %v, %v (%v)", code, ok, err)
	}
	if _, _, err := driver.RunChild(t.Context(), testSpec(), time.Time{}); err == nil {
		t.Fatal("expected zero timestamp rejection")
	} else if code, ok := CodeOf(err); !ok || code != ErrorCodeInvalidArgument {
		t.Fatalf("code = %v, %v (%v)", code, ok, err)
	}
}
