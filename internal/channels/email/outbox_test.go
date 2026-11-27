package email

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/platform/durable/durabletest"
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
	clock := durabletest.NewManualClock(fixedNow)
	logger := slog.New(slog.DiscardHandler)
	t.Run("a missing journal or clock is refused", func(t *testing.T) {
		for name, deps := range map[string]*RecoveryDeps{
			"nil deps":   nil,
			"no journal": {Logger: logger, Clock: clock},
			"no clock":   {Journal: &fakeRecoverer{}, Logger: logger},
		} {
			if _, err := RecoverEffects(t.Context(), deps); !fault.HasCode(err, ErrorCodeInvalidArgument) {
				t.Fatalf("%s: RecoverEffects = %v, want invalid_argument", name, err)
			}
		}
	})
	t.Run("one pass over the email provider reports when the next could claim more", func(t *testing.T) {
		retryAt := fixedNow.Add(time.Minute)
		rec := &fakeRecoverer{report: effect.RecoveryReport{Scanned: 2, Claimed: 1, RetryAt: retryAt}}
		got, err := RecoverEffects(t.Context(), &RecoveryDeps{Journal: rec, Logger: logger, Clock: clock})
		if err != nil || !got.Equal(retryAt) {
			t.Fatalf("RecoverEffects = %v, %v; want %v", got, err, retryAt)
		}
		if rec.provider != ProviderName {
			t.Fatalf("provider = %q, want %q", rec.provider, ProviderName)
		}
	})
	t.Run("a journal failure fails the pass", func(t *testing.T) {
		wantErr := errors.New("db locked")
		rec := &fakeRecoverer{err: wantErr}
		if _, err := RecoverEffects(t.Context(), &RecoveryDeps{Journal: rec, Clock: clock}); !errors.Is(err, wantErr) {
			t.Fatalf("RecoverEffects(err) = %v, want %v", err, wantErr)
		}
	})
}

type timerSignalClock struct {
	*durabletest.ManualClock
	timers chan struct{}
}

func (c *timerSignalClock) Timer(d time.Duration) <-chan time.Time {
	fired := c.ManualClock.Timer(d)
	c.timers <- struct{}{}
	return fired
}

func newTimerSignalClock(start time.Time) *timerSignalClock {
	return &timerSignalClock{ManualClock: durabletest.NewManualClock(start), timers: make(chan struct{}, 16)}
}

func TestRecoverEffectsAtWaitsForTheRetryTime(t *testing.T) {
	clock := newTimerSignalClock(fixedNow)
	rec := &fakeRecoverer{}
	deps := &RecoveryDeps{Journal: rec, Logger: slog.New(slog.DiscardHandler), Clock: clock}
	done := make(chan error, 1)
	go func() { done <- RecoverEffectsAt(t.Context(), deps, fixedNow.Add(45*time.Second)) }()
	<-clock.timers

	if rec.provider != "" {
		t.Fatal("the second pass ran before the retry time")
	}
	clock.Advance(45 * time.Second)
	if err := <-done; err != nil {
		t.Fatalf("RecoverEffectsAt: %v", err)
	}
	if rec.provider != ProviderName {
		t.Fatalf("provider = %q, want the email provider recovered at the retry time", rec.provider)
	}
}

func TestRecoverEffectsAtStopsWithItsContext(t *testing.T) {
	clock := newTimerSignalClock(fixedNow)
	rec := &fakeRecoverer{}
	deps := &RecoveryDeps{Journal: rec, Clock: clock}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- RecoverEffectsAt(ctx, deps, fixedNow.Add(time.Minute)) }()
	<-clock.timers

	cancel()

	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("RecoverEffectsAt = %v, want context.Canceled", err)
	}
	if rec.provider != "" {
		t.Fatal("a cancelled wait still ran the pass")
	}
}

func startEmailIntent(t *testing.T, f *deliveryFixture, journal *effect.Journal, provider, key string) *effect.Intent {
	t.Helper()
	prepared, err := journal.Prepare(t.Context(), &effect.PrepareRequest{
		SessionID:      deliverySession,
		IdempotencyKey: key,
		Provider:       provider,
		Operation:      OpSendReply,
		Classification: effect.ClassificationEffectful,
		Request:        json.RawMessage(`{"text":"hi"}`),
		EventKind:      effect.EventKindChannelRequested,
	})
	if err != nil {
		t.Fatalf("Prepare %s: %v", key, err)
	}
	started, err := journal.Start(t.Context(), prepared.ID)
	if err != nil {
		t.Fatalf("Start %s: %v", key, err)
	}
	return started
}

func intentState(t *testing.T, journal *effect.Journal, intent *effect.Intent) effect.State {
	t.Helper()
	current, err := journal.Get(t.Context(), intent.ID)
	if err != nil {
		t.Fatalf("Get %s: %v", intent.IdempotencyKey, err)
	}
	return current.State
}

func recoveryFixture(t *testing.T) (*deliveryFixture, *effect.Journal, *timerSignalClock) {
	t.Helper()
	f := newDeliveryFixture(t)
	f.request("ensure-session", defaultReply(), "x")
	clock := newTimerSignalClock(fixedNow)
	journal, err := effect.NewJournal(f.h.db, effect.Options{Clock: clock, Logger: f.h.logger, Turns: turnstore.NewTurnStore(f.h.db)})
	if err != nil {
		t.Fatalf("NewJournal: %v", err)
	}
	return f, journal, clock
}

func TestEmailStartupRecoveryWaitsForLeases(t *testing.T) {
	f, journal, clock := recoveryFixture(t)
	expired := startEmailIntent(t, f, journal, ProviderName, "expired")
	clock.Advance(40 * time.Second)
	live := startEmailIntent(t, f, journal, ProviderName, "live")
	legacy := startEmailIntent(t, f, journal, ProviderName, "legacy")
	if _, err := f.h.db.ExecContext(t.Context(), `UPDATE effect_intent SET lease_token = NULL, lease_expires_at = NULL WHERE id = ?`, legacy.ID); err != nil {
		t.Fatalf("clear lease: %v", err)
	}
	foreign := startEmailIntent(t, f, journal, "telegram", "foreign")
	clock.Advance(30 * time.Second)
	deps := &RecoveryDeps{Journal: journal, Logger: f.h.logger, Clock: clock}

	retryAt, err := RecoverEffects(t.Context(), deps)

	if err != nil {
		t.Fatalf("RecoverEffects: %v", err)
	}
	if want := fixedNow.Add(100 * time.Second); !retryAt.Equal(want) {
		t.Fatalf("retry time = %v, want the live lease's expiry %v", retryAt, want)
	}
	for intent, want := range map[*effect.Intent]effect.State{expired: effect.StateUnknown, legacy: effect.StateUnknown, live: effect.StateStarted, foreign: effect.StateStarted} {
		if got := intentState(t, journal, intent); got != want {
			t.Errorf("%s after the first pass = %s, want %s", intent.IdempotencyKey, got, want)
		}
	}

	done := make(chan error, 1)
	go func() { done <- RecoverEffectsAt(t.Context(), deps, retryAt) }()
	<-clock.timers
	clock.Advance(30*time.Second + time.Nanosecond)
	if err := <-done; err != nil {
		t.Fatalf("RecoverEffectsAt: %v", err)
	}
	for intent, want := range map[*effect.Intent]effect.State{live: effect.StateUnknown, foreign: effect.StateStarted} {
		if got := intentState(t, journal, intent); got != want {
			t.Errorf("%s after the second pass = %s, want %s", intent.IdempotencyKey, got, want)
		}
	}
}

func TestEmailStartupRecoveryUnchanged(t *testing.T) {
	f, journal, clock := recoveryFixture(t)
	mine := startEmailIntent(t, f, journal, ProviderName, "mine")
	foreign := startEmailIntent(t, f, journal, "telegram", "foreign")
	if _, err := f.h.db.ExecContext(t.Context(), `UPDATE effect_intent SET lease_token = NULL, lease_expires_at = NULL`); err != nil {
		t.Fatalf("clear leases: %v", err)
	}

	retryAt, err := RecoverEffects(t.Context(), &RecoveryDeps{Journal: journal, Logger: f.h.logger, Clock: clock})

	if err != nil || !retryAt.IsZero() {
		t.Fatalf("RecoverEffects = %v, %v; want every leaseless intent claimed at once with nothing to retry", retryAt, err)
	}
	if got := intentState(t, journal, mine); got != effect.StateUnknown {
		t.Errorf("email intent = %s, want unknown", got)
	}
	if got := intentState(t, journal, foreign); got != effect.StateStarted {
		t.Errorf("foreign intent = %s, want started", got)
	}
}
