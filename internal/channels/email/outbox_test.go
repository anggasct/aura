package email

import (
	"context"
	"errors"
	"log/slog"
	"testing"

	"github.com/anggasct/aura/internal/platform/effect"
	"github.com/anggasct/aura/internal/platform/fault"
	turnstore "github.com/anggasct/aura/internal/platform/turn/store"
)

type fakeRecoverer struct {
	provider string
	report   effect.RecoveryReport
	err      error
}

func (r *fakeRecoverer) RecoverProvider(_ context.Context, provider string) (effect.RecoveryReport, error) {
	r.provider = provider
	return r.report, r.err
}

func TestAdaptersHealthChecks(t *testing.T) {
	var empty Adapters
	if checks := empty.HealthChecks(); len(checks) != 0 {
		t.Fatalf("empty HealthChecks() = %+v, want nil", checks)
	}
	h := newHarness(t, harnessOptions{})
	h.adapter.markFault(faultLoginRejected)
	adapters := Adapters{h.adapter}
	checks := adapters.HealthChecks()
	if len(checks) != 1 || checks[0].ID != "email" || checks[0].Remediation != RemediationReviewMailbox {
		t.Fatalf("HealthChecks() = %+v", checks)
	}
	findings := checks[0].Checker.Check(t.Context())
	if len(findings) != 1 || findings[0].Code != faultLoginRejected {
		t.Fatalf("Check() = %+v, want %s", findings, faultLoginRejected)
	}
}

func TestOutboxRoutesAndSends(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	h.start()
	h.deliver(mailSpec{from: friendAddress, subject: "Hello", messageID: "outbox-1@example.com"}.raw())
	h.wantTurns(1)
	sessionID := h.sink.all()[0].ConversationID

	outbox := NewOutbox([]*Adapter{h.adapter})
	route, ok := outbox.RouteOf(sessionID)
	if !ok || route != (Route{Account: testAccount, Sender: friendAddress}) {
		t.Fatalf("RouteOf(%q) = %+v %v", sessionID, route, ok)
	}
	if _, ok := outbox.RouteOf("unknown"); ok {
		t.Fatal("RouteOf(unknown) returned ok")
	}
	if _, err := outbox.Send(t.Context(), nil); !fault.HasCode(err, ErrorCodeInvalidArgument) {
		t.Fatalf("Send(nil) = %v, want invalid_argument", err)
	}
	if _, err := outbox.Send(t.Context(), &Outgoing{SessionID: "unknown", IdempotencyKey: "k1", To: friendAddress, Subject: "Hi", Body: "body"}); !errors.Is(err, ErrNoRoute) {
		t.Fatalf("Send(unknown) = %v, want ErrNoRoute", err)
	}
	if err := (&storeSessions{sessions: turnstore.NewSessionService(h.db)}).EnsureSession(t.Context(), sessionID, "email:"+friendAddress, testPersona); err != nil {
		t.Fatalf("EnsureSession: %v", err)
	}
	msgID, err := outbox.Send(t.Context(), &Outgoing{SessionID: sessionID, IdempotencyKey: "k2", To: friendAddress, Subject: "Hi", Body: "body"})
	if err != nil || msgID == "" {
		t.Fatalf("Send() = %q, %v", msgID, err)
	}
}

func TestRecoverEffects(t *testing.T) {
	if err := RecoverEffects(t.Context(), nil, slog.New(slog.DiscardHandler)); !fault.HasCode(err, ErrorCodeInvalidArgument) {
		t.Fatalf("RecoverEffects(nil) = %v, want invalid_argument", err)
	}
	rec := &fakeRecoverer{report: effect.RecoveryReport{Scanned: 2, Claimed: 1}}
	if err := RecoverEffects(t.Context(), rec, slog.New(slog.DiscardHandler)); err != nil {
		t.Fatalf("RecoverEffects() = %v", err)
	}
	if rec.provider != ProviderName {
		t.Fatalf("provider = %q, want %q", rec.provider, ProviderName)
	}
	wantErr := errors.New("db locked")
	rec.err = wantErr
	if err := RecoverEffects(t.Context(), rec, nil); !errors.Is(err, wantErr) {
		t.Fatalf("RecoverEffects(err) = %v, want %v", err, wantErr)
	}
}
