package child

import (
	stdcontext "context"
	"encoding/json"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/durable"
)

func TestStarterRoutesThroughDurableRuntime(t *testing.T) {
	runtime := durable.NewFake()
	payloads := make(chan []byte, 1)
	runtime.RegisterHandler(HandlerName, func(_ stdcontext.Context, inv durable.Invocation) error {
		payloads <- inv.Payload()
		return nil
	})
	launcher, err := AdaptRuntime(runtime)
	if err != nil {
		t.Fatalf("AdaptRuntime: %v", err)
	}
	service, err := NewService(newFakeRegistry())
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	driver, err := NewDriver(service, launcher, NewLedger(nil, 0), nil)
	if err != nil {
		t.Fatalf("NewDriver: %v", err)
	}
	spawn, created, err := driver.RunChild(t.Context(), testSpec(), time.Now().UTC())
	if err != nil || !created {
		t.Fatalf("RunChild: %+v, %v, %v", spawn, created, err)
	}
	var gotPayload []byte
	select {
	case gotPayload = <-payloads:
	case <-time.After(5 * time.Second):
		t.Fatal("durable handler never saw the start payload")
	}
	var decoded struct {
		ChildID       string `json:"child_id"`
		SessionID     string `json:"session_id"`
		DurableKey    string `json:"durable_key"`
		ReservationID string `json:"reservation_id"`
		Deadline      string `json:"deadline"`
	}
	if err := json.Unmarshal(gotPayload, &decoded); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	if decoded.ChildID != "ch-1" || decoded.DurableKey != "child/ch-1" || decoded.ReservationID == "" {
		t.Fatalf("payload = %+v", decoded)
	}
	if decoded.Deadline == "" {
		t.Fatal("payload must carry the parent-confirmed deadline")
	}
	status, err := runtime.Status(t.Context(), durable.RunRef{Key: "child/ch-1"})
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for status.State == durable.RunRunning && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
		status, err = runtime.Status(t.Context(), durable.RunRef{Key: "child/ch-1"})
		if err != nil {
			t.Fatalf("Status: %v", err)
		}
	}
	if status.State != durable.RunSucceeded {
		t.Fatalf("run state = %q", status.State)
	}
}

func TestStarterReleasesWhenRuntimeHasNoHandler(t *testing.T) {
	runtime := durable.NewFake()
	launcher, err := AdaptRuntime(runtime)
	if err != nil {
		t.Fatalf("AdaptRuntime: %v", err)
	}
	service, err := NewService(newFakeRegistry())
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	ledger := &fakeLedger{}
	driver, err := NewDriver(service, launcher, ledger, nil)
	if err != nil {
		t.Fatalf("NewDriver: %v", err)
	}
	if _, _, err := driver.RunChild(t.Context(), testSpec(), time.Now().UTC()); err == nil {
		t.Fatal("unregistered handler must fail closed")
	}
	ledger.mu.Lock()
	defer ledger.mu.Unlock()
	if len(ledger.reserved) != 1 || len(ledger.released) != 1 {
		t.Fatalf("reserved = %v released = %v", ledger.reserved, ledger.released)
	}
}

func TestAdaptRuntimeRejectsNil(t *testing.T) {
	if _, err := AdaptRuntime(nil); err == nil {
		t.Fatal("expected nil runtime rejection")
	} else if code, ok := CodeOf(err); !ok || code != ErrorCodeChildUnavailable {
		t.Fatalf("code = %v, %v (%v)", code, ok, err)
	}
	if _, err := NewStarter(nil, &fakeLauncher{}, &fakeLedger{}, nil); err == nil {
		t.Error("expected nil service rejection")
	}
}
