package broadcast

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/durable"
)

type fakeRunStore struct {
	mu           sync.Mutex
	records      map[string]ItemRecord
	byKey        map[string]string
	cursors      map[string]time.Time
	attemptTimes map[string][]time.Time
	slotTimes    []time.Time
}

func newFakeRunStore() *fakeRunStore {
	return &fakeRunStore{records: map[string]ItemRecord{}, byKey: map[string]string{}, cursors: map[string]time.Time{}, attemptTimes: map[string][]time.Time{}}
}

func (s *fakeRunStore) seed(record *ItemRecord) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.records[record.ID] = *record
	s.byKey[record.Producer+"|"+record.IdempotencyKey] = record.ID
}

func (s *fakeRunStore) Insert(_ context.Context, record *ItemRecord) (ItemRecord, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := record.Producer + "|" + record.IdempotencyKey
	if id, ok := s.byKey[key]; ok {
		existing := s.records[id]
		if existing.ContentDigest != record.ContentDigest {
			return ItemRecord{}, false, Errorf(ErrorCodeConflict, "idempotency key is already claimed with different content")
		}
		return existing, true, nil
	}
	s.records[record.ID] = *record
	s.byKey[key] = record.ID
	return *record, false, nil
}

func (s *fakeRunStore) CreateDigest(_ context.Context, parent *ItemRecord, childIDs []string) (ItemRecord, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := parent.Producer + "|" + parent.IdempotencyKey
	if id, ok := s.byKey[key]; ok {
		return s.records[id], true, nil
	}
	for _, id := range childIDs {
		child, ok := s.records[id]
		if !ok || child.State != StateHeld {
			return ItemRecord{}, false, Errorf(ErrorCodeConflict, "digest children are not all held")
		}
	}
	s.records[parent.ID] = *parent
	s.byKey[key] = parent.ID
	for _, id := range childIDs {
		child := s.records[id]
		child.State = StateCancelled
		child.DigestParentID = parent.ID
		s.records[id] = child
	}
	return *parent, false, nil
}

func toFull(record *ItemRecord) FullItem {
	return FullItem{
		Item: Item{
			ID: record.ID, Producer: record.Producer, IdempotencyKey: record.IdempotencyKey,
			ContentDigest: record.ContentDigest, Priority: record.Priority,
			DestinationAlias: record.DestinationAlias, ContentJSON: record.ContentJSON,
			State: record.State, NotBefore: record.NotBefore, CreatedAt: record.CreatedAt,
		},
		AttemptCount:   record.AttemptCount,
		EffectID:       record.EffectID,
		DigestParentID: record.DigestParentID,
	}
}

func (s *fakeRunStore) Load(_ context.Context, id string) (FullItem, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	record, ok := s.records[id]
	if !ok {
		return FullItem{}, false, nil
	}
	return toFull(&record), true, nil
}

func (s *fakeRunStore) ListHeld(_ context.Context, alias, priority string, notBefore time.Time, limit int) ([]FullItem, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []FullItem
	for id := range s.records {
		record := s.records[id]
		if record.State != StateHeld || record.DestinationAlias != alias || record.Priority != priority {
			continue
		}
		if record.NotBefore.After(notBefore) {
			continue
		}
		out = append(out, toFull(&record))
		if len(out) >= limit {
			break
		}
	}
	return out, nil
}

func (s *fakeRunStore) Settle(_ context.Context, id, state, effectID string, _ time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	record, ok := s.records[id]
	if !ok {
		return Errorf(ErrorCodeInvalidArgument, "broadcast item not found")
	}
	if terminalState(record.State) {
		return Errorf(ErrorCodeConflict, "broadcast item is already terminal")
	}
	record.State = state
	record.EffectID = effectID
	s.records[id] = record
	return nil
}

func (s *fakeRunStore) NoteAttempt(_ context.Context, id string, attempt int64, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	record, ok := s.records[id]
	if !ok {
		return Errorf(ErrorCodeInvalidArgument, "broadcast item not found")
	}
	if s.attemptTimes == nil {
		s.attemptTimes = map[string][]time.Time{}
	}
	s.attemptTimes[id] = append(s.attemptTimes[id], now.UTC())
	if attempt > record.AttemptCount {
		record.AttemptCount = attempt
		s.records[id] = record
	}
	return nil
}

func (s *fakeRunStore) ClaimDispatchSlot(_ context.Context, alias string, now time.Time, gap time.Duration) (time.Time, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.slotTimes = append(s.slotTimes, now.UTC())
	slot := now.UTC()
	if cursor, ok := s.cursors[alias]; ok && cursor.After(slot) {
		slot = cursor
	}
	s.cursors[alias] = slot.Add(gap)
	return slot, nil
}

func (s *fakeRunStore) stateOf(id string) (state string, attempts int64, effect string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	record := s.records[id]
	return record.State, record.AttemptCount, record.EffectID
}

func (s *fakeRunStore) attemptStamps(id string) []time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]time.Time(nil), s.attemptTimes[id]...)
}

type sendScript struct {
	outcome SendOutcome
	err     error
}

type fakeSender struct {
	mu      sync.Mutex
	scripts []sendScript
	calls   []SendRequest
}

func (s *fakeSender) Send(_ context.Context, req *SendRequest) (SendOutcome, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, *req)
	if len(s.scripts) == 0 {
		return SendOutcome{Succeeded: true, IntentID: "intent-default"}, nil
	}
	script := s.scripts[0]
	s.scripts = s.scripts[1:]
	return script.outcome, script.err
}

func (s *fakeSender) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.calls)
}

func successOutcome(intent string) sendScript {
	return sendScript{outcome: SendOutcome{Succeeded: true, IntentID: intent}}
}

type runnerFixture struct {
	runner  *Runner
	store   *fakeRunStore
	sender  *fakeSender
	runtime *durable.Fake
	clock   *durable.ManualClock
}

func testRunPolicy() RunPolicy {
	return RunPolicy{
		MaxAttempts:    3,
		MaxDeliveryAge: 24 * time.Hour,
		DispatchGap:    0,
		Fallback:       map[string]string{},
		MaxDigestItems: 20,
		MaxDigestBytes: 12000,
	}
}

func newRunnerFixture(scripts []sendScript) *runnerFixture {
	return newRunnerFixtureWithPolicy(testRunPolicy(), scripts)
}

func newRunnerFixtureWithPolicy(policy RunPolicy, scripts []sendScript) *runnerFixture {
	start := time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)
	clock := durable.NewManualClock(start)
	runtime := durable.NewFake().WithClock(clock)
	store := newFakeRunStore()
	sender := &fakeSender{scripts: scripts}
	runner, err := NewRunner(
		store,
		map[string]string{"default": "discord:owner"},
		map[string]Sender{"discord": sender},
		policy,
		nil,
		nil,
	)
	if err != nil {
		panic(err)
	}
	runtime.RegisterHandler(HandlerName, runner.Handle)
	return &runnerFixture{runner: runner, store: store, sender: sender, runtime: runtime, clock: clock}
}

func seedRunItem(fixture *runnerFixture, id, state string, createdAt time.Time) {
	fixture.store.seed(&ItemRecord{
		ID: id, Producer: "cron", IdempotencyKey: "key-" + id,
		ContentDigest: "digest-" + id, Priority: PriorityWarning,
		DestinationAlias: "default", ContentJSON: `{"text":"hello"}`,
		State: state, NotBefore: createdAt, CreatedAt: createdAt, UpdatedAt: createdAt,
	})
}

func startBroadcastRun(t *testing.T, fixture *runnerFixture, id string) {
	t.Helper()
	if _, err := fixture.runtime.Start(t.Context(), durable.StartRequest{
		Handler: HandlerName, Key: id, Payload: []byte(id),
	}); err != nil {
		t.Fatalf("Start(): %v", err)
	}
}

func waitBroadcastState(t *testing.T, fixture *runnerFixture, id, want string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		fixture.clock.Advance(time.Second)
		time.Sleep(time.Millisecond)
		if state, _, _ := fixture.store.stateOf(id); state == want {
			return
		}
	}
	state, attempts, effect := fixture.store.stateOf(id)
	t.Fatalf("item %s state = %q (attempts %d, effect %q), want %q", id, state, attempts, effect, want)
}

func TestRunner_DeliversImmediately(t *testing.T) {
	fixture := newRunnerFixture([]sendScript{successOutcome("intent-1")})
	now := time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)
	seedRunItem(fixture, "bcst-1", StateScheduled, now)
	startBroadcastRun(t, fixture, "bcst-1")
	waitBroadcastState(t, fixture, "bcst-1", StateSucceeded)
	state, attempts, effect := fixture.store.stateOf("bcst-1")
	if state != StateSucceeded || attempts != 1 || effect != "intent-1" {
		t.Errorf("settled = %q/%d/%q", state, attempts, effect)
	}
	if fixture.sender.callCount() != 1 {
		t.Fatalf("sends = %d, want 1", fixture.sender.callCount())
	}
	call := fixture.sender.calls[0]
	if call.Route.Source != "discord" || call.Route.Instance != "owner" {
		t.Errorf("route = %+v", call.Route)
	}
	if call.Text != "hello" {
		t.Errorf("text = %q, want rendered content", call.Text)
	}
	if call.IdempotencyKey != "bcst:bcst-1:a1" {
		t.Errorf("key = %q", call.IdempotencyKey)
	}
	if call.SessionID != "broadcast:default" {
		t.Errorf("session = %q", call.SessionID)
	}
}

func TestRunner_AmbiguousStopsWithoutRetry(t *testing.T) {
	fixture := newRunnerFixture([]sendScript{{outcome: SendOutcome{Ambiguous: true, IntentID: "intent-x"}}})
	now := time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)
	seedRunItem(fixture, "bcst-1", StateScheduled, now)
	startBroadcastRun(t, fixture, "bcst-1")
	waitBroadcastState(t, fixture, "bcst-1", StateUnknown)
	if fixture.sender.callCount() != 1 {
		t.Errorf("sends = %d, want exactly 1 (no retry on unknown)", fixture.sender.callCount())
	}
	if _, _, effect := fixture.store.stateOf("bcst-1"); effect != "intent-x" {
		t.Errorf("effect link = %q, want intent-x", effect)
	}
}

func TestRunner_TransportErrorRetriesThenSucceeds(t *testing.T) {
	fixture := newRunnerFixture([]sendScript{
		{err: errors.New("connection reset")},
		{err: errors.New("connection reset")},
		successOutcome("intent-3"),
	})
	now := time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)
	seedRunItem(fixture, "bcst-1", StateScheduled, now)
	startBroadcastRun(t, fixture, "bcst-1")
	waitBroadcastState(t, fixture, "bcst-1", StateSucceeded)
	if fixture.sender.callCount() != 3 {
		t.Fatalf("sends = %d, want 3 attempts", fixture.sender.callCount())
	}
	keys := map[string]bool{}
	for _, call := range fixture.sender.calls {
		keys[call.IdempotencyKey] = true
	}
	for _, want := range []string{"bcst:bcst-1:a1", "bcst:bcst-1:a2", "bcst:bcst-1:a3"} {
		if !keys[want] {
			t.Errorf("missing idempotency key %q in %v", want, keys)
		}
	}
}

func TestRunner_HeldItemWaitsUntilRelease(t *testing.T) {
	fixture := newRunnerFixture([]sendScript{successOutcome("intent-held")})
	release := time.Now().UTC().Add(time.Hour)
	fixture.store.seed(&ItemRecord{
		ID: "bcst-held", Producer: "cron", IdempotencyKey: "key-held", ContentDigest: "d-held",
		Priority: PriorityInfo, DestinationAlias: "default", ContentJSON: `{"text":"held"}`,
		State: StateHeld, NotBefore: release, CreatedAt: release.Add(-time.Hour), UpdatedAt: release.Add(-time.Hour),
	})
	startBroadcastRun(t, fixture, "bcst-held")
	time.Sleep(200 * time.Millisecond)
	if got := fixture.sender.callCount(); got != 0 {
		t.Fatalf("sends before release = %d, want 0", got)
	}
	waitBroadcastState(t, fixture, "bcst-held", StateSucceeded)
	if fixture.sender.callCount() != 1 {
		t.Fatalf("sends after release = %d, want 1", fixture.sender.callCount())
	}
}

func TestRunner_RetryAttemptsUseFreshClock(t *testing.T) {
	fixture := newRunnerFixture([]sendScript{
		{err: errors.New("connection reset")},
		{err: errors.New("connection reset")},
		successOutcome("intent-3"),
	})
	now := time.Now().UTC()
	fixture.store.seed(&ItemRecord{
		ID: "bcst-retry", Producer: "cron", IdempotencyKey: "key-retry", ContentDigest: "d-retry",
		Priority: PriorityWarning, DestinationAlias: "default", ContentJSON: `{"text":"hello"}`,
		State: StateScheduled, NotBefore: now, CreatedAt: now, UpdatedAt: now,
	})
	startBroadcastRun(t, fixture, "bcst-retry")
	waitBroadcastState(t, fixture, "bcst-retry", StateSucceeded)
	if fixture.sender.callCount() != 3 {
		t.Fatalf("sends = %d, want 3 attempts", fixture.sender.callCount())
	}
	stamps := fixture.store.attemptStamps("bcst-retry")
	if len(stamps) != 3 {
		t.Fatalf("attempt timestamps = %d, want 3", len(stamps))
	}
	if stamps[0].Equal(stamps[1]) || stamps[1].Equal(stamps[2]) || stamps[0].Equal(stamps[2]) {
		t.Errorf("attempt timestamps are not distinct: %v", stamps)
	}
}

func TestRunner_WindowRunUsesIndependentClocks(t *testing.T) {
	fixture := newRunnerFixture([]sendScript{successOutcome("intent-a"), successOutcome("intent-b")})
	base := time.Now().UTC().Add(-time.Hour)
	fixture.store.seed(&ItemRecord{
		ID: "bcst-wa", Producer: "cron", IdempotencyKey: "key-wa", ContentDigest: "d-wa",
		Priority: PriorityInfo, DestinationAlias: "default", ContentJSON: `{"text":"a"}`,
		State: StateHeld, NotBefore: base, CreatedAt: base, UpdatedAt: base,
	})
	fixture.store.seed(&ItemRecord{
		ID: "bcst-wb", Producer: "cron", IdempotencyKey: "key-wb", ContentDigest: "d-wb",
		Priority: PriorityInfo, DestinationAlias: "default", ContentJSON: `{"text":"b"}`,
		State: StateHeld, NotBefore: base.Add(time.Minute), CreatedAt: base, UpdatedAt: base,
	})
	startBroadcastRun(t, fixture, "bcst-wa")
	waitBroadcastState(t, fixture, "bcst-wa", StateSucceeded)
	waitBroadcastState(t, fixture, "bcst-wb", StateSucceeded)
	if fixture.sender.callCount() != 2 {
		t.Fatalf("sends = %d, want 2 independent deliveries", fixture.sender.callCount())
	}
	first := fixture.store.attemptStamps("bcst-wa")
	second := fixture.store.attemptStamps("bcst-wb")
	if len(first) == 0 || len(second) == 0 {
		t.Fatalf("missing attempt timestamps: %v %v", first, second)
	}
	if first[0].Equal(second[0]) {
		t.Errorf("window items share one clock timestamp: %v", first[0])
	}
}

func TestRunner_DefinitiveFailureFallsBack(t *testing.T) {
	policy := testRunPolicy()
	policy.Fallback = map[string]string{"default": "discord:fallback-channel"}
	fixture := newRunnerFixtureWithPolicy(policy, []sendScript{
		{outcome: SendOutcome{IntentID: "intent-bad"}},
		successOutcome("intent-fb"),
	})
	fallbackSender := &fakeSender{scripts: []sendScript{successOutcome("intent-fb")}}
	fixture.runner.senders["discord"] = &routeSwitchSender{
		primary:  fixture.sender,
		fallback: fallbackSender,
	}
	now := time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)
	seedRunItem(fixture, "bcst-1", StateScheduled, now)
	startBroadcastRun(t, fixture, "bcst-1")
	waitBroadcastState(t, fixture, "bcst-1", StateSucceeded)
	if _, _, effect := fixture.store.stateOf("bcst-1"); effect != "intent-fb" {
		t.Errorf("effect link = %q, want fallback intent", effect)
	}
	if fallbackSender.callCount() != 1 {
		t.Errorf("fallback sends = %d, want 1", fallbackSender.callCount())
	}
	if key := fallbackSender.calls[0].IdempotencyKey; key != "bcst:bcst-1:fb" {
		t.Errorf("fallback key = %q", key)
	}
}

type routeSwitchSender struct {
	primary  *fakeSender
	fallback *fakeSender
}

func (s *routeSwitchSender) Send(ctx context.Context, req *SendRequest) (SendOutcome, error) {
	if req.Route.Instance == "fallback-channel" {
		return s.fallback.Send(ctx, req)
	}
	return s.primary.Send(ctx, req)
}

func TestRunner_DefinitiveFailureWithoutFallbackFails(t *testing.T) {
	fixture := newRunnerFixture([]sendScript{{outcome: SendOutcome{IntentID: "intent-bad"}}})
	now := time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)
	seedRunItem(fixture, "bcst-1", StateScheduled, now)
	startBroadcastRun(t, fixture, "bcst-1")
	waitBroadcastState(t, fixture, "bcst-1", StateFailed)
}

func TestRunner_ExpiredItemFailsWithoutSend(t *testing.T) {
	fixture := newRunnerFixture(nil)
	created := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	seedRunItem(fixture, "bcst-1", StateScheduled, created)
	fixture.clock.Advance(48 * time.Hour)
	startBroadcastRun(t, fixture, "bcst-1")
	waitBroadcastState(t, fixture, "bcst-1", StateFailed)
	if fixture.sender.callCount() != 0 {
		t.Error("expired item sent")
	}
}

func TestRunner_HeldItemsDigestOnce(t *testing.T) {
	fixture := newRunnerFixture([]sendScript{successOutcome("intent-digest")})
	release := time.Date(2026, 9, 13, 7, 0, 0, 0, time.UTC)
	fixture.store.seed(&ItemRecord{
		ID: "bcst-1", Producer: "cron", IdempotencyKey: "key-1", ContentDigest: "d1",
		Priority: PriorityInfo, DestinationAlias: "default", ContentJSON: `{"text":"one"}`,
		State: StateHeld, NotBefore: release, CreatedAt: release.Add(-time.Hour), UpdatedAt: release.Add(-time.Hour),
	})
	fixture.store.seed(&ItemRecord{
		ID: "bcst-2", Producer: "cron", IdempotencyKey: "key-2", ContentDigest: "d2",
		Priority: PriorityInfo, DestinationAlias: "default", ContentJSON: `{"text":"two"}`,
		State: StateHeld, NotBefore: release, CreatedAt: release.Add(-30 * time.Minute), UpdatedAt: release.Add(-30 * time.Minute),
	})
	startBroadcastRun(t, fixture, "bcst-1")
	time.Sleep(100 * time.Millisecond)
	fixture.clock.Advance(24 * time.Hour)
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		fixture.clock.Advance(time.Second)
		time.Sleep(time.Millisecond)
		first, _, _ := fixture.store.stateOf("bcst-1")
		second, _, _ := fixture.store.stateOf("bcst-2")
		if first == StateCancelled && second == StateCancelled {
			break
		}
	}
	for _, id := range []string{"bcst-1", "bcst-2"} {
		if state, _, _ := fixture.store.stateOf(id); state != StateCancelled {
			t.Fatalf("child %s state = %q, want cancelled+linked", id, state)
		}
	}
	delivered := time.Now().Add(10 * time.Second)
	for time.Now().Before(delivered) {
		fixture.clock.Advance(time.Second)
		time.Sleep(time.Millisecond)
		if fixture.sender.callCount() == 1 {
			break
		}
	}
	if fixture.sender.callCount() != 1 {
		t.Fatalf("sends = %d, want exactly 1 digest delivery", fixture.sender.callCount())
	}
	text := fixture.sender.calls[0].Text
	if !strings.Contains(text, "one") || !strings.Contains(text, "two") {
		t.Errorf("digest text not composed: %q", text)
	}
	startBroadcastRun(t, fixture, "bcst-2")
	quiet := time.Now().Add(2 * time.Second)
	for time.Now().Before(quiet) {
		fixture.clock.Advance(100 * time.Millisecond)
		time.Sleep(time.Millisecond)
	}
	if fixture.sender.callCount() != 1 {
		t.Errorf("late run sent again: %d sends", fixture.sender.callCount())
	}
}

func TestRunner_SkipsTerminalAndMissing(t *testing.T) {
	fixture := newRunnerFixture(nil)
	now := time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)
	seedRunItem(fixture, "bcst-done", StateSucceeded, now)
	startBroadcastRun(t, fixture, "bcst-done")
	startBroadcastRun(t, fixture, "bcst-ghost")
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		fixture.clock.Advance(100 * time.Millisecond)
		time.Sleep(time.Millisecond)
	}
	if fixture.sender.callCount() != 0 {
		t.Error("terminal or missing item sent")
	}
}

func TestRunner_PacedSlotSleeps(t *testing.T) {
	policy := testRunPolicy()
	policy.DispatchGap = time.Minute
	fixture := newRunnerFixtureWithPolicy(policy, []sendScript{successOutcome("intent-1")})
	now := time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)
	seedRunItem(fixture, "bcst-1", StateScheduled, now)
	startBroadcastRun(t, fixture, "bcst-1")
	waitBroadcastState(t, fixture, "bcst-1", StateSucceeded)
	if fixture.sender.callCount() != 1 {
		t.Error("paced send missing")
	}
}

func TestRunner_RejectsBadHandles(t *testing.T) {
	fixture := newRunnerFixture(nil)
	var nilCtx context.Context
	if err := fixture.runner.Handle(nilCtx, nil); err == nil {
		t.Error("nil context accepted")
	}
	if err := fixture.runner.Handle(t.Context(), nil); err == nil {
		t.Error("nil invocation accepted")
	}
}

func TestNewRunner_RejectsBadPolicy(t *testing.T) {
	store := newFakeRunStore()
	senders := map[string]Sender{"discord": &fakeSender{}}
	routes := map[string]string{"default": "discord:owner"}
	good := testRunPolicy()
	if _, err := NewRunner(nil, routes, senders, good, nil, nil); err == nil {
		t.Error("nil store accepted")
	}
	badAttempts := testRunPolicy()
	badAttempts.MaxAttempts = 0
	if _, err := NewRunner(store, routes, senders, badAttempts, nil, nil); err == nil {
		t.Error("non-positive attempts accepted")
	}
	badAge := testRunPolicy()
	badAge.MaxDeliveryAge = 0
	if _, err := NewRunner(store, routes, senders, badAge, nil, nil); err == nil {
		t.Error("non-positive age accepted")
	}
	badGap := testRunPolicy()
	badGap.DispatchGap = -time.Second
	if _, err := NewRunner(store, routes, senders, badGap, nil, nil); err == nil {
		t.Error("negative gap accepted")
	}
	badRoute := map[string]string{"default": "https://evil.example/hook"}
	if _, err := NewRunner(store, badRoute, senders, good, nil, nil); err == nil {
		t.Error("bad route accepted")
	}
}

func TestRenderText(t *testing.T) {
	if got := RenderText(`"plain alert"`); got != "plain alert" {
		t.Errorf("string content = %q", got)
	}
	if got := RenderText(`{"text":"hello"}`); got != "hello" {
		t.Errorf("text field = %q", got)
	}
	if got := RenderText(`not json`); got != "not json" {
		t.Errorf("raw fallback = %q", got)
	}
	digest := `{"window":"w","destination":"d","priority":"info","count":2,"items":[{"id":"a","content":{"text":"one"}},{"id":"b","content":{"text":"two"}}]}`
	if got := RenderText(digest); got != "one\n---\ntwo" {
		t.Errorf("digest render = %q", got)
	}
}

func TestRetryBackoffBounded(t *testing.T) {
	seen := map[time.Duration]bool{}
	for attempt := 1; attempt <= 10; attempt++ {
		delay := retryBackoff(attempt)
		if delay <= 0 || delay > retryMaxDelay {
			t.Errorf("backoff(%d) = %v out of bounds", attempt, delay)
		}
		seen[delay] = true
	}
	if len(seen) < 3 {
		t.Error("backoff does not grow")
	}
}

func TestSubmit_StartsRun(t *testing.T) {
	store := newFakeItemStore()
	var started []string
	var mu sync.Mutex
	broadcaster, err := New(
		testPolicy(),
		&fakeRegistry{registered: map[string]bool{"discord": true}},
		store,
		func(_ context.Context, itemID string) error {
			mu.Lock()
			defer mu.Unlock()
			started = append(started, itemID)
			return nil
		},
		nil,
	)
	if err != nil {
		t.Fatalf("New(): %v", err)
	}
	item, _, err := broadcaster.Submit(t.Context(), testNotification())
	if err != nil {
		t.Fatalf("Submit(): %v", err)
	}
	if len(started) != 1 || started[0] != item.ID {
		t.Errorf("started runs = %v, want [%s]", started, item.ID)
	}
	if _, _, err := broadcaster.Submit(t.Context(), testNotification()); err != nil {
		t.Fatalf("replay submit: %v", err)
	}
	if len(started) != 2 {
		t.Errorf("replay did not restart the run: %v", started)
	}
	failing, err := New(
		testPolicy(),
		&fakeRegistry{registered: map[string]bool{"discord": true}},
		newFakeItemStore(),
		func(context.Context, string) error { return errors.New("runtime down") },
		nil,
	)
	if err != nil {
		t.Fatalf("New(): %v", err)
	}
	if _, _, err := failing.Submit(t.Context(), testNotification()); err == nil {
		t.Error("starter failure accepted")
	} else if code, ok := CodeOf(err); !ok || code != ErrorCodeUnavailable {
		t.Errorf("starter failure code = %v, %v", code, ok)
	}
}

func TestResumeActive(t *testing.T) {
	clock := durable.NewManualClock(time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC))
	runtime := durable.NewFake().WithClock(clock)
	runtime.RegisterHandler(HandlerName, func(context.Context, durable.Invocation) error { return nil })
	store := &fakeResumeStore{items: []FullItem{
		{Item: Item{ID: "bcst-1"}},
		{Item: Item{ID: "bcst-2"}},
	}}
	count, err := ResumeActive(t.Context(), runtime, store)
	if err != nil {
		t.Fatalf("ResumeActive(): %v", err)
	}
	if count != 2 {
		t.Errorf("resumed = %d, want 2", count)
	}
	if _, err := runtime.Start(t.Context(), durable.StartRequest{Handler: HandlerName, Key: "bcst-1", Payload: []byte("x")}); err != nil {
		t.Fatalf("convergent start: %v", err)
	}
	var nilCtx context.Context
	if _, err := ResumeActive(nilCtx, runtime, store); err == nil {
		t.Error("nil context accepted")
	}
}

type fakeResumeStore struct {
	items []FullItem
}

func (s *fakeResumeStore) ListActive(context.Context, int) ([]FullItem, error) {
	return s.items, nil
}
