package handoff

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/store/storetest"
)

type stubNotifier struct {
	target Target
	err    error
	calls  int
}

func (n *stubNotifier) NotifyHandoff(context.Context, string, string) (Target, error) {
	n.calls++
	return n.target, n.err
}

func TestHandoffModuleAndServiceValidation(t *testing.T) {
	if _, err := New(nil); !fault.HasCode(err, ErrorCodeInvalidArgument) {
		t.Fatalf("New(nil) = %v, want %s", err, ErrorCodeInvalidArgument)
	}
	if _, err := New(&ModuleDeps{}); !fault.HasCode(err, ErrorCodeInvalidArgument) {
		t.Fatalf("New(nil db) = %v, want %s", err, ErrorCodeInvalidArgument)
	}
	if _, err := NewService(nil, nil, nil); !fault.HasCode(err, ErrorCodeInvalidArgument) {
		t.Fatalf("NewService(nil) = %v, want %s", err, ErrorCodeInvalidArgument)
	}
	if _, err := NewGate(nil, time.Hour, nil); !fault.HasCode(err, ErrorCodeInvalidArgument) {
		t.Fatalf("NewGate(nil) = %v, want %s", err, ErrorCodeInvalidArgument)
	}

	db := storetest.OpenMigrated(t)
	now := time.Date(2026, 11, 5, 9, 0, 0, 0, time.UTC)
	seedPersonaSession(t, db, "main/discord:office/dm:9", "discord:9", "main")
	elsewhere := &stubNotifier{err: ErrNoTarget}
	office := &stubNotifier{target: Target{Channel: ChannelDiscord, Account: "office", ChannelID: "4400"}}

	mod, err := New(&ModuleDeps{
		DB:        db,
		Notifiers: []Notifier{elsewhere, office},
		Timeout:   time.Hour,
		Now:       func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if len(mod.Tools()) != 1 || mod.Store() == nil || mod.Replies() == nil || mod.Gate() == nil || mod.Service() == nil {
		t.Fatalf("module components incomplete: tools=%d", len(mod.Tools()))
	}
	if err := mod.Service().OpenHandoff(t.Context(), nil); !fault.HasCode(err, ErrorCodeInvalidArgument) {
		t.Fatalf("OpenHandoff(nil) = %v, want %s", err, ErrorCodeInvalidArgument)
	}
	req := &Request{SessionID: "main/discord:office/dm:9", PrincipalID: "discord:9", AgentID: "main", Reason: "refund"}
	if err := mod.Service().OpenHandoff(t.Context(), req); err != nil {
		t.Fatalf("OpenHandoff: %v", err)
	}
	if elsewhere.calls != 1 || office.calls != 1 {
		t.Fatalf("calls elsewhere=%d office=%d", elsewhere.calls, office.calls)
	}
	if state, err := mod.Gate().Check(t.Context(), req.SessionID); err != nil || state != StateOpen {
		t.Fatalf("Gate.Check = %q, %v", state, err)
	}
	now = now.Add(2 * time.Hour)
	if state, err := mod.Gate().Check(t.Context(), req.SessionID); err != nil || state != StateExpired {
		t.Fatalf("Gate.Check after timeout = %q, %v", state, err)
	}

	failing, err := NewService(mod.Store(), []Notifier{&stubNotifier{err: errors.New("dm closed")}}, nil)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	if err := failing.OpenHandoff(t.Context(), req); err == nil {
		t.Fatal("expected notifier failure")
	}
	empty, err := NewService(mod.Store(), nil, nil)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	if err := empty.OpenHandoff(t.Context(), req); !fault.HasCode(err, ErrorCodeUnavailable) {
		t.Fatalf("OpenHandoff without channels = %v, want %s", err, ErrorCodeUnavailable)
	}
}
