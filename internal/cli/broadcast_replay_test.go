package cli

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/broadcast"
	"github.com/anggasct/aura/internal/durable"
	"github.com/anggasct/aura/internal/effect"
	"github.com/anggasct/aura/internal/store"
	toolsbuiltin "github.com/anggasct/aura/internal/tools/builtin"
)

type countingEffectProvider struct {
	invokes atomic.Int64
	fail    atomic.Bool
}

func (p *countingEffectProvider) SupportsIdempotency() bool { return false }

func (p *countingEffectProvider) Invoke(_ context.Context, _ *effect.Invocation) (effect.Outcome, error) {
	p.invokes.Add(1)
	if p.fail.Load() {
		return effect.Outcome{}, errors.New("provider transport reset")
	}
	return effect.Outcome{Succeeded: true, Receipt: json.RawMessage(`{"message_id":"m1"}`)}, nil
}

type journaledTestSender struct {
	effects  *effect.Executor
	provider effect.Provider
}

func (s *journaledTestSender) Send(ctx context.Context, req *broadcast.SendRequest) (broadcast.SendOutcome, error) {
	payload, err := json.Marshal(map[string]string{"text": req.Text})
	if err != nil {
		return broadcast.SendOutcome{}, err
	}
	intent, err := s.effects.Execute(ctx, &effect.PrepareRequest{
		SessionID:      req.SessionID,
		IdempotencyKey: req.IdempotencyKey,
		Provider:       "test",
		Operation:      "send",
		Classification: effect.ClassificationEffectful,
		Request:        payload,
		EventKind:      effect.EventKindChannelRequested,
	}, s.provider)
	if err != nil {
		return broadcast.SendOutcome{}, err
	}
	switch intent.State {
	case effect.StateSucceeded:
		return broadcast.SendOutcome{IntentID: intent.ID, Receipt: intent.ProviderReceipt, Succeeded: true}, nil
	case effect.StateUnknown:
		return broadcast.SendOutcome{IntentID: intent.ID, Ambiguous: true}, nil
	default:
		return broadcast.SendOutcome{IntentID: intent.ID}, nil
	}
}

type replayFixture struct {
	db       *sql.DB
	store    *broadcastItemStore
	effects  *effect.Executor
	provider *countingEffectProvider
	runner   *broadcast.Runner
}

func newReplayFixture(t *testing.T, gap time.Duration) *replayFixture {
	t.Helper()
	db, err := store.OpenDB(t.Context(), t.TempDir()+"/replay.db")
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := store.Migrate(t.Context(), db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	sessions := store.NewSessionService(db)
	now := time.Now().UTC()
	if err := sessions.Create(t.Context(), &store.Session{
		ID: "broadcast:default", OwnerID: "broadcast",
		Metadata: json.RawMessage(`{}`), CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("Create session: %v", err)
	}
	executor, err := toolsbuiltin.NewChannelEffects(db, slog.Default())
	if err != nil {
		t.Fatalf("NewChannelEffects: %v", err)
	}
	provider := &countingEffectProvider{}
	adapter := &broadcastItemStore{store: store.NewBroadcastStore(db)}
	runner, err := broadcast.NewRunner(
		adapter,
		map[string]string{"default": "test:owner"},
		map[string]broadcast.Sender{"test": &journaledTestSender{effects: executor, provider: provider}},
		broadcast.RunPolicy{
			MaxAttempts: 3, MaxDeliveryAge: 24 * time.Hour, DispatchGap: gap,
			Fallback: map[string]string{}, MaxDigestItems: 20, MaxDigestBytes: 12000,
		},
		slog.Default(),
		nil,
	)
	if err != nil {
		t.Fatalf("NewRunner(): %v", err)
	}
	return &replayFixture{db: db, store: adapter, effects: executor, provider: provider, runner: runner}
}

func seedReplayItem(t *testing.T, fixture *replayFixture, id, state string, notBefore time.Time) {
	t.Helper()
	now := time.Now().UTC()
	if _, _, err := fixture.store.Insert(t.Context(), &broadcast.ItemRecord{
		ID: id, Producer: "cron", IdempotencyKey: "key-" + id, ContentDigest: "digest-" + id,
		Priority: "info", DestinationAlias: "default", ContentJSON: `{"text":"hello"}`,
		State: state, NotBefore: notBefore, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
}

func startReplayRun(t *testing.T, runtime *durable.Fake, runner *broadcast.Runner, id string) durable.RunRef {
	t.Helper()
	runtime.RegisterHandler(broadcast.HandlerName, runner.Handle)
	ref, err := runtime.Start(t.Context(), durable.StartRequest{
		Handler: broadcast.HandlerName, Key: id, Payload: []byte(id),
	})
	if err != nil {
		t.Fatalf("Start(): %v", err)
	}
	return ref
}

func waitBroadcastRow(t *testing.T, fixture *replayFixture, id, want string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		item, found, err := fixture.store.Load(t.Context(), id)
		if err != nil {
			t.Fatalf("Load(): %v", err)
		}
		if found && item.State == want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	item, _, _ := fixture.store.Load(t.Context(), id)
	t.Fatalf("item %s state = %q, want %q", id, item.State, want)
}

func TestBroadcastReplay_CrashBetweenSendAndSettle(t *testing.T) {
	fixture := newReplayFixture(t, 0)
	seedReplayItem(t, fixture, "bcst-1", "scheduled", time.Now().UTC())

	crashing, err := broadcast.NewRunner(
		&crashAdapter{store: fixture.store},
		map[string]string{"default": "test:owner"},
		map[string]broadcast.Sender{"test": &journaledTestSender{effects: fixture.effects, provider: fixture.provider}},
		broadcast.RunPolicy{
			MaxAttempts: 3, MaxDeliveryAge: 24 * time.Hour,
			Fallback: map[string]string{}, MaxDigestItems: 20, MaxDigestBytes: 12000,
		},
		slog.Default(),
		nil,
	)
	if err != nil {
		t.Fatalf("NewRunner(): %v", err)
	}
	first := durable.NewFake()
	first.RegisterHandler(broadcast.HandlerName, crashing.Handle)
	ref, err := first.Start(t.Context(), durable.StartRequest{
		Handler: broadcast.HandlerName, Key: "bcst-1", Payload: []byte("bcst-1"),
	})
	if err != nil {
		t.Fatalf("Start(): %v", err)
	}
	first.WaitReady(ref)
	if got := fixture.provider.invokes.Load(); got != 1 {
		t.Fatalf("first drive invokes = %d, want 1", got)
	}
	item, _, err := fixture.store.Load(t.Context(), "bcst-1")
	if err != nil {
		t.Fatalf("Load(): %v", err)
	}
	if item.State != "scheduled" {
		t.Fatalf("crashed row state = %q, want still scheduled", item.State)
	}

	second := durable.NewFake()
	startReplayRun(t, second, fixture.runner, "bcst-1")
	waitBroadcastRow(t, fixture, "bcst-1", "succeeded")
	if got := fixture.provider.invokes.Load(); got != 1 {
		t.Errorf("redrive invokes = %d, want 1 (effect idempotency converges)", got)
	}
}

type crashAdapter struct {
	store *broadcastItemStore
}

func (s *crashAdapter) Insert(ctx context.Context, record *broadcast.ItemRecord) (broadcast.ItemRecord, bool, error) {
	return s.store.Insert(ctx, record)
}

func (s *crashAdapter) CreateDigest(ctx context.Context, parent *broadcast.ItemRecord, childIDs []string) (broadcast.ItemRecord, bool, error) {
	return s.store.CreateDigest(ctx, parent, childIDs)
}

func (s *crashAdapter) Load(ctx context.Context, id string) (broadcast.FullItem, bool, error) {
	return s.store.Load(ctx, id)
}

func (s *crashAdapter) ListHeld(ctx context.Context, alias, priority string, notBefore time.Time, limit int) ([]broadcast.FullItem, error) {
	return s.store.ListHeld(ctx, alias, priority, notBefore, limit)
}

func (s *crashAdapter) Settle(context.Context, string, string, string, time.Time) error {
	return errors.New("simulated crash before settle")
}

func (s *crashAdapter) NoteAttempt(ctx context.Context, id string, attempt int64, now time.Time) error {
	return s.store.NoteAttempt(ctx, id, attempt, now)
}

func (s *crashAdapter) ClaimDispatchSlot(ctx context.Context, alias string, now time.Time, gap time.Duration) (time.Time, error) {
	return s.store.ClaimDispatchSlot(ctx, alias, now, gap)
}

func TestBroadcastReplay_CancelMidSleepRedrives(t *testing.T) {
	clock := durable.NewManualClock(time.Now().UTC())
	first := durable.NewFake().WithClock(clock)
	fixture := newReplayFixture(t, time.Hour)
	seedReplayItem(t, fixture, "bcst-1", "scheduled", time.Now().UTC())
	first.RegisterHandler(broadcast.HandlerName, fixture.runner.Handle)
	ref, err := first.Start(t.Context(), durable.StartRequest{
		Handler: broadcast.HandlerName, Key: "bcst-1", Payload: []byte("bcst-1"),
	})
	if err != nil {
		t.Fatalf("Start(): %v", err)
	}
	if err := first.Cancel(t.Context(), ref); err != nil {
		t.Fatalf("Cancel(): %v", err)
	}
	if got := fixture.provider.invokes.Load(); got != 0 {
		t.Fatalf("cancelled run sent %d times", got)
	}
	item, _, err := fixture.store.Load(t.Context(), "bcst-1")
	if err != nil {
		t.Fatalf("Load(): %v", err)
	}
	if item.State != "scheduled" {
		t.Fatalf("cancelled row state = %q, want scheduled", item.State)
	}

	secondClock := durable.NewManualClock(time.Now().UTC())
	second := durable.NewFake().WithClock(secondClock)
	startReplayRun(t, second, fixture.runner, "bcst-1")
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		secondClock.Advance(time.Second)
		time.Sleep(time.Millisecond)
		current, _, err := fixture.store.Load(t.Context(), "bcst-1")
		if err != nil {
			t.Fatalf("Load(): %v", err)
		}
		if current.State == "succeeded" {
			break
		}
	}
	final, _, err := fixture.store.Load(t.Context(), "bcst-1")
	if err != nil {
		t.Fatalf("Load(): %v", err)
	}
	if final.State != "succeeded" {
		t.Fatalf("redriven row state = %q, want succeeded", final.State)
	}
	if got := fixture.provider.invokes.Load(); got != 1 {
		t.Errorf("redrive invokes = %d, want 1", got)
	}
}
