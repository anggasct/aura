package broadcast

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/platform/durable"
	"github.com/anggasct/aura/internal/platform/durable/sqlite"
	"github.com/anggasct/aura/internal/store/storetest"
)

const (
	hostPoll    = 10 * time.Millisecond
	hostTTL     = time.Second
	hostGrace   = 5 * time.Second
	hostTimeout = 20 * time.Second
	hostStep    = time.Minute
)

type tripwire struct {
	mu      sync.Mutex
	call    string
	nth     int
	seen    map[string]int
	entered chan struct{}
}

func newTripwire(call string, nth int) *tripwire {
	return &tripwire{call: call, nth: nth, seen: map[string]int{}, entered: make(chan struct{})}
}

func (w *tripwire) hold(done <-chan struct{}, call string) {
	w.mu.Lock()
	w.seen[call]++
	trips := call == w.call && w.seen[call] == w.nth
	w.mu.Unlock()
	if !trips {
		return
	}
	close(w.entered)
	<-done
}

func (w *tripwire) tripped() bool {
	select {
	case <-w.entered:
		return true
	default:
		return false
	}
}

type trippingInvocation struct {
	durable.Invocation
	wire *tripwire
	done <-chan struct{}
}

func (i *trippingInvocation) RunAction(ctx context.Context, key string, fn func(context.Context) ([]byte, error)) ([]byte, error) {
	out, err := i.Invocation.RunAction(ctx, key, fn)
	i.wire.hold(ctx.Done(), key)
	return out, err
}

func (i *trippingInvocation) Sleep(d time.Duration) error {
	if err := i.Invocation.Sleep(d); err != nil {
		return err
	}
	i.wire.hold(i.done, "Sleep")
	return nil
}

type recordingRunStore struct {
	*fakeRunStore
	wire   *tripwire
	callMu sync.Mutex
	calls  []string
	claims []time.Time
}

func (s *recordingRunStore) enter(done <-chan struct{}, method, id string) {
	s.callMu.Lock()
	s.calls = append(s.calls, method+" "+id)
	s.callMu.Unlock()
	s.wire.hold(done, method)
}

func (s *recordingRunStore) Load(ctx context.Context, id string) (FullItem, bool, error) {
	s.enter(ctx.Done(), "Load", id)
	return s.fakeRunStore.Load(ctx, id)
}

func (s *recordingRunStore) ListHeld(ctx context.Context, alias, priority string, notBefore time.Time, limit int) ([]FullItem, error) {
	s.enter(ctx.Done(), "ListHeld", alias)
	return s.fakeRunStore.ListHeld(ctx, alias, priority, notBefore, limit)
}

func (s *recordingRunStore) CreateDigest(ctx context.Context, parent *ItemRecord, childIDs []string) (ItemRecord, bool, error) {
	s.enter(ctx.Done(), "CreateDigest", parent.ID)
	return s.fakeRunStore.CreateDigest(ctx, parent, childIDs)
}

func (s *recordingRunStore) Settle(ctx context.Context, id, state, effectID string, now time.Time) error {
	s.enter(ctx.Done(), "Settle", id)
	return s.fakeRunStore.Settle(ctx, id, state, effectID, now)
}

func (s *recordingRunStore) NoteAttempt(ctx context.Context, id string, attempt int64, now time.Time) error {
	s.enter(ctx.Done(), "NoteAttempt", id)
	return s.fakeRunStore.NoteAttempt(ctx, id, attempt, now)
}

func (s *recordingRunStore) ClaimDispatchSlot(ctx context.Context, alias string, now time.Time, gap time.Duration) (time.Time, error) {
	s.enter(ctx.Done(), "ClaimDispatchSlot", alias)
	slot, err := s.fakeRunStore.ClaimDispatchSlot(ctx, alias, now, gap)
	s.callMu.Lock()
	s.claims = append(s.claims, slot)
	s.callMu.Unlock()
	return slot, err
}

func (s *recordingRunStore) recorded() []string {
	s.callMu.Lock()
	defer s.callMu.Unlock()
	return append([]string(nil), s.calls...)
}

func (s *recordingRunStore) claimed() []time.Time {
	s.callMu.Lock()
	defer s.callMu.Unlock()
	return append([]time.Time(nil), s.claims...)
}

func (s *recordingRunStore) count(method string) int {
	count := 0
	for _, call := range s.recorded() {
		if strings.HasPrefix(call, method+" ") {
			count++
		}
	}
	return count
}

func (s *recordingRunStore) setCursor(alias string, cursor time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cursors[alias] = cursor.UTC()
}

func (s *recordingRunStore) cursor(alias string) time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cursors[alias]
}

func (s *recordingRunStore) settleElsewhere(id, state string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	record := s.records[id]
	record.State = state
	s.records[id] = record
}

func writingCall(call string) bool {
	method, _, _ := strings.Cut(call, " ")
	switch method {
	case "Insert", "CreateDigest", "Settle", "NoteAttempt", "ClaimDispatchSlot":
		return true
	default:
		return false
	}
}

type scriptedSender struct {
	name    string
	wire    *tripwire
	clock   *durable.ManualClock
	mu      sync.Mutex
	calls   []SendRequest
	times   []time.Time
	respond func(ctx context.Context, call int) (SendOutcome, error)
}

func (s *scriptedSender) Send(ctx context.Context, req *SendRequest) (SendOutcome, error) {
	s.mu.Lock()
	s.calls = append(s.calls, *req)
	s.times = append(s.times, s.clock.Now())
	call := len(s.calls)
	respond := s.respond
	s.mu.Unlock()
	s.wire.hold(ctx.Done(), s.name)
	if respond == nil {
		return SendOutcome{Succeeded: true, IntentID: "intent-" + s.name}, nil
	}
	return respond(ctx, call)
}

func (s *scriptedSender) script(respond func(ctx context.Context, call int) (SendOutcome, error)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.respond = respond
}

func (s *scriptedSender) sent() []SendRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]SendRequest(nil), s.calls...)
}

func (s *scriptedSender) sentAt() []time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]time.Time(nil), s.times...)
}

func failsFirstWith(outcome SendOutcome, err error) func(ctx context.Context, call int) (SendOutcome, error) {
	return func(_ context.Context, call int) (SendOutcome, error) {
		if call == 1 {
			return outcome, err
		}
		return SendOutcome{Succeeded: true, IntentID: "intent-retry"}, nil
	}
}

func (r *recordingObserver) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.events)
}

func (r *recordingObserver) since(mark int) []Observation {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]Observation(nil), r.events[mark:]...)
}

type hostSetup struct {
	policy RunPolicy
	block  string
	nth    int
}

type broadcastHost struct {
	t        *testing.T
	db       *sql.DB
	clock    *durable.ManualClock
	wire     *tripwire
	store    *recordingRunStore
	primary  *scriptedSender
	fallback *scriptedSender
	observer *recordingObserver
	runner   *Runner
	runtime  *sqlite.Runtime
	stop     context.CancelFunc
	stopped  chan error
}

func newBroadcastHost(t *testing.T, setup *hostSetup) *broadcastHost {
	t.Helper()
	clock := durable.NewManualClock(time.Now())
	wire := newTripwire(setup.block, setup.nth)
	host := &broadcastHost{
		t:        t,
		db:       storetest.OpenMigrated(t),
		clock:    clock,
		wire:     wire,
		store:    &recordingRunStore{fakeRunStore: newFakeRunStore(), wire: wire},
		primary:  &scriptedSender{name: "primary", wire: wire, clock: clock},
		fallback: &scriptedSender{name: "fallback", wire: wire, clock: clock},
		observer: &recordingObserver{},
	}
	runner, err := NewRunner(
		host.store,
		map[string]string{"default": "primary:owner"},
		map[string]Sender{"primary": host.primary, "backup": host.fallback},
		setup.policy,
		slog.New(slog.DiscardHandler),
		host.observer.observe,
	)
	if err != nil {
		t.Fatalf("NewRunner(): %v", err)
	}
	host.runner = runner
	host.startHost()
	t.Cleanup(host.stopHost)
	return host
}

func withFallback(policy RunPolicy, attempts int) RunPolicy {
	policy.MaxAttempts = attempts
	policy.Fallback = map[string]string{"default": "backup:other"}
	return policy
}

func (h *broadcastHost) handle(ctx context.Context, inv durable.Invocation) error {
	return h.runner.Handle(ctx, &trippingInvocation{Invocation: inv, wire: h.wire, done: ctx.Done()})
}

func (h *broadcastHost) startHost() {
	h.t.Helper()
	rt, err := sqlite.New(&sqlite.Options{DB: h.db, Logger: slog.New(slog.DiscardHandler), Clock: h.clock, PollInterval: hostPoll, HostTTL: hostTTL, StopGrace: hostGrace})
	if err != nil {
		h.t.Fatalf("sqlite.New: %v", err)
	}
	rt.RegisterHandler(HandlerName, h.handle)
	ctx, stop := context.WithCancel(context.WithoutCancel(h.t.Context()))
	stopped := make(chan error, 1)
	go func() { stopped <- rt.Run(ctx) }()
	h.runtime, h.stop, h.stopped = rt, stop, stopped
}

func (h *broadcastHost) stopHost() {
	h.t.Helper()
	if h.stop == nil {
		return
	}
	h.stop()
	h.stop = nil
	select {
	case err := <-h.stopped:
		if err != nil {
			h.t.Errorf("stop the durable host: %v", err)
		}
	case <-time.After(hostTimeout):
		h.t.Error("the durable host did not stop")
	}
}

func (h *broadcastHost) seed(id, state string, notBefore time.Time) {
	now := time.Now().UTC()
	created := notBefore.UTC()
	if now.Before(created) {
		created = now
	}
	created = created.Add(-time.Minute)
	h.store.seed(&ItemRecord{
		ID: id, Producer: "cron", IdempotencyKey: "key-" + id, ContentDigest: "digest-" + id,
		Priority: PriorityInfo, DestinationAlias: "default", ContentJSON: `{"text":"` + id + `"}`,
		State: state, NotBefore: notBefore.UTC(), CreatedAt: created, UpdatedAt: created,
	})
}

func (h *broadcastHost) startRun(id string) durable.RunRef {
	h.t.Helper()
	ref, err := h.runtime.Start(h.t.Context(), durable.StartRequest{Handler: HandlerName, Key: id, Payload: []byte(id)})
	if err != nil {
		h.t.Fatalf("start the durable run: %v", err)
	}
	return ref
}

func (h *broadcastHost) await(what string, step time.Duration, ready func() bool) {
	h.t.Helper()
	deadline := time.NewTimer(hostTimeout)
	defer deadline.Stop()
	tick := time.NewTicker(2 * time.Millisecond)
	defer tick.Stop()
	for !ready() {
		select {
		case <-tick.C:
			if step > 0 {
				h.clock.Advance(step)
			}
		case <-deadline.C:
			h.t.Fatalf("timed out waiting until %s", what)
		}
	}
}

func (h *broadcastHost) awaitTripped() {
	h.t.Helper()
	h.await("the "+h.wire.call+" call is entered", hostStep, h.wire.tripped)
}

func (h *broadcastHost) awaitItem(id, state string) {
	h.t.Helper()
	h.await("item "+id+" is "+state, hostStep, func() bool {
		current, _, _ := h.store.stateOf(id)
		return current == state
	})
}

func (h *broadcastHost) durableState(ref durable.RunRef) durable.RunState {
	h.t.Helper()
	status, err := h.runtime.Status(h.t.Context(), ref)
	if err != nil {
		h.t.Fatalf("status of %s: %v", ref.Key, err)
	}
	return status.State
}

func (h *broadcastHost) awaitDurableState(ref durable.RunRef, state durable.RunState) {
	h.t.Helper()
	h.await("durable run "+ref.Key+" is "+string(state), hostStep, func() bool { return h.durableState(ref) == state })
}

func (h *broadcastHost) queryStrings(query, runKey string) []string {
	h.t.Helper()
	rows, err := h.db.QueryContext(h.t.Context(), query, runKey)
	if err != nil {
		h.t.Fatalf("query the journal: %v", err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var value string
		if err := rows.Scan(&value); err != nil {
			h.t.Fatalf("scan the journal: %v", err)
		}
		out = append(out, value)
	}
	if err := rows.Err(); err != nil {
		h.t.Fatalf("read the journal: %v", err)
	}
	return out
}

func (h *broadcastHost) actionKeys(ref durable.RunRef) []string {
	h.t.Helper()
	return h.queryStrings(`SELECT action_key FROM durable_action WHERE run_key = ? ORDER BY action_key`, ref.Key)
}

func (h *broadcastHost) actionResult(ref durable.RunRef, key string) string {
	h.t.Helper()
	var result string
	if err := h.db.QueryRowContext(h.t.Context(), `SELECT CAST(result AS TEXT) FROM durable_action WHERE run_key = ? AND action_key = ?`, ref.Key, key).Scan(&result); err != nil {
		h.t.Fatalf("read action %s: %v", key, err)
	}
	return result
}

func (h *broadcastHost) timerFireTimes(ref durable.RunRef) []time.Time {
	h.t.Helper()
	var out []time.Time
	for _, raw := range h.queryStrings(`SELECT fire_at FROM durable_timer WHERE run_key = ? ORDER BY seq`, ref.Key) {
		at, err := time.Parse(time.RFC3339Nano, raw)
		if err != nil {
			h.t.Fatalf("parse a timer: %v", err)
		}
		out = append(out, at)
	}
	return out
}

func (h *broadcastHost) dropLease() {
	h.t.Helper()
	if _, err := h.db.ExecContext(h.t.Context(), `DELETE FROM durable_host`); err != nil {
		h.t.Fatalf("delete the host lease: %v", err)
	}
}

type hostMark struct {
	ref      durable.RunRef
	calls    int
	sends    int
	observed int
	actions  []string
}

func (h *broadcastHost) mark(ref durable.RunRef) hostMark {
	h.t.Helper()
	return hostMark{
		ref:      ref,
		calls:    len(h.store.recorded()),
		sends:    len(h.primary.sent()) + len(h.fallback.sent()),
		observed: h.observer.count(),
		actions:  h.actionKeys(ref),
	}
}

func (h *broadcastHost) requireNothingAfter(mark *hostMark, ownRow string) {
	h.t.Helper()
	for _, call := range h.store.recorded()[mark.calls:] {
		if writingCall(call) {
			h.t.Errorf("store write after the stop: %s", call)
		}
	}
	if sends := len(h.primary.sent()) + len(h.fallback.sent()); sends != mark.sends {
		h.t.Errorf("sends after the stop = %d, want %d", sends, mark.sends)
	}
	if observed := h.observer.since(mark.observed); len(observed) > 0 {
		h.t.Errorf("observations after the stop = %+v, want none", observed)
	}
	var added []string
	for _, key := range h.actionKeys(mark.ref) {
		if !slices.Contains(mark.actions, key) {
			added = append(added, key)
		}
	}
	if len(added) > 1 || (len(added) == 1 && (ownRow == "" || !strings.HasPrefix(added[0], ownRow))) {
		h.t.Errorf("action rows written after the stop = %v, want none beyond the blocked call's own %q", added, ownRow)
	}
	if state := h.durableState(mark.ref); state != durable.RunRunning {
		h.t.Errorf("durable run after the stop = %s, want %s", state, durable.RunRunning)
	}
}

type preemptCase struct {
	name      string
	block     string
	nth       int
	ownRow    string
	arrange   func(h *broadcastHost) string
	meanwhile func(h *broadcastHost)
}

func runPreemptCase(t *testing.T, tc *preemptCase) {
	t.Helper()
	host := newBroadcastHost(t, &hostSetup{policy: testRunPolicy(), block: tc.block, nth: tc.nth})
	ref := host.startRun(tc.arrange(host))
	host.awaitTripped()
	if tc.meanwhile != nil {
		tc.meanwhile(host)
	}
	mark := host.mark(ref)
	host.stopHost()
	host.requireNothingAfter(&mark, tc.ownRow)
}

func scheduledNow(h *broadcastHost) string {
	h.seed("bcst-1", StateScheduled, time.Now())
	return "bcst-1"
}

func TestRunnerResumesItsReleaseSleepAfterTheHostStops(t *testing.T) {
	host := newBroadcastHost(t, &hostSetup{policy: testRunPolicy()})
	host.seed("bcst-1", StateHeld, time.Now().Add(time.Hour))
	ref := host.startRun("bcst-1")
	host.await("the release sleep is recorded", 0, func() bool { return len(host.timerFireTimes(ref)) == 1 })
	host.stopHost()

	for _, call := range host.store.recorded() {
		if writingCall(call) {
			t.Errorf("store write during the release sleep: %s", call)
		}
	}
	if sends := len(host.primary.sent()); sends != 0 {
		t.Errorf("sends during the release sleep = %d, want 0", sends)
	}
	if state := host.durableState(ref); state != durable.RunRunning {
		t.Errorf("durable run after the stop = %s, want %s", state, durable.RunRunning)
	}

	host.startHost()
	host.awaitItem("bcst-1", StateSucceeded)
	host.awaitDurableState(ref, durable.RunSucceeded)
	if sends := len(host.primary.sent()); sends != 1 {
		t.Errorf("sends = %d, want 1", sends)
	}
}

func TestRunnerLeavesAnItemWhoseSendItsHostPreempted(t *testing.T) {
	t.Run("a primary attempt", func(t *testing.T) {
		host := newBroadcastHost(t, &hostSetup{policy: testRunPolicy(), block: "primary", nth: 1})
		host.primary.script(failsFirstWith(SendOutcome{}, errors.New("connection reset")))
		ref := host.startRun(scheduledNow(host))
		host.awaitTripped()
		mark := host.mark(ref)
		host.stopHost()
		host.requireNothingAfter(&mark, "")

		host.startHost()
		host.awaitItem("bcst-1", StateSucceeded)
		host.awaitDurableState(ref, durable.RunSucceeded)
		sent := host.primary.sent()
		if len(sent) != 2 || sent[1].IdempotencyKey != sent[0].IdempotencyKey || sent[1].IdempotencyKey != "bcst:bcst-1:a1" {
			t.Errorf("primary sends = %+v, want the first attempt's key twice", sent)
		}
		if _, attempts, _ := host.store.stateOf("bcst-1"); attempts != 1 {
			t.Errorf("attempt count = %d, want 1", attempts)
		}
	})
	t.Run("the fallback send", func(t *testing.T) {
		host := newBroadcastHost(t, &hostSetup{policy: withFallback(testRunPolicy(), 1), block: "fallback", nth: 1})
		host.primary.script(func(context.Context, int) (SendOutcome, error) {
			return SendOutcome{IntentID: "intent-rejected"}, nil
		})
		host.fallback.script(failsFirstWith(SendOutcome{}, errors.New("connection reset")))
		ref := host.startRun(scheduledNow(host))
		host.awaitTripped()
		mark := host.mark(ref)
		host.stopHost()
		host.requireNothingAfter(&mark, "")

		host.startHost()
		host.awaitItem("bcst-1", StateSucceeded)
		host.awaitDurableState(ref, durable.RunSucceeded)
		sent := host.fallback.sent()
		if len(sent) != 2 || sent[1].IdempotencyKey != "bcst:bcst-1:fb" || sent[0].IdempotencyKey != sent[1].IdempotencyKey {
			t.Errorf("fallback sends = %+v, want the fallback key twice", sent)
		}
		if sends := len(host.primary.sent()); sends != 1 {
			t.Errorf("primary sends = %d, want 1", sends)
		}
	})
}

func TestRunnerDoesNotFallBackWhenItsLastAttemptIsPreempted(t *testing.T) {
	cases := []struct {
		name    string
		outcome SendOutcome
		err     error
	}{
		{name: "an error", err: errors.New("connection reset")},
		{name: "a failed outcome", outcome: SendOutcome{IntentID: "intent-rejected"}},
		{name: "an ambiguous outcome", outcome: SendOutcome{IntentID: "intent-unclear", Ambiguous: true}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			host := newBroadcastHost(t, &hostSetup{policy: withFallback(testRunPolicy(), 1), block: "primary", nth: 1})
			host.primary.script(failsFirstWith(tc.outcome, tc.err))
			ref := host.startRun(scheduledNow(host))
			host.awaitTripped()
			mark := host.mark(ref)
			host.stopHost()
			host.requireNothingAfter(&mark, "")
			if sends := len(host.fallback.sent()); sends != 0 {
				t.Errorf("fallback sends after the stop = %d, want 0", sends)
			}

			host.startHost()
			host.awaitItem("bcst-1", StateSucceeded)
			host.awaitDurableState(ref, durable.RunSucceeded)
			sent := host.primary.sent()
			if len(sent) != 2 || sent[1].IdempotencyKey != "bcst:bcst-1:a1" {
				t.Errorf("primary sends = %+v, want the first attempt's key twice", sent)
			}
			if sends := len(host.fallback.sent()); sends != 0 {
				t.Errorf("fallback sends = %d, want 0", sends)
			}
		})
	}
}

func TestRunnerSettlesASuccessItsHostPreemptedWithoutSendingAgain(t *testing.T) {
	host := newBroadcastHost(t, &hostSetup{policy: testRunPolicy(), block: "primary", nth: 1})
	ref := host.startRun(scheduledNow(host))
	host.awaitTripped()
	mark := host.mark(ref)
	host.stopHost()
	host.requireNothingAfter(&mark, "send:")
	if !slices.Contains(host.actionKeys(ref), "send:bcst-1:a1") {
		t.Errorf("action rows = %v, want the send's outcome journaled", host.actionKeys(ref))
	}
	if settles := host.store.count("Settle"); settles != 0 {
		t.Errorf("settles after the stop = %d, want 0", settles)
	}

	host.startHost()
	host.awaitItem("bcst-1", StateSucceeded)
	host.awaitDurableState(ref, durable.RunSucceeded)
	if sends := len(host.primary.sent()); sends != 1 {
		t.Errorf("primary sends = %d, want 1", sends)
	}
}

func TestRunnerClaimsItsDispatchSlotOnce(t *testing.T) {
	t.Run("a replay claims nothing", func(t *testing.T) {
		policy := testRunPolicy()
		policy.DispatchGap = time.Minute
		host := newBroadcastHost(t, &hostSetup{policy: policy})
		host.store.setCursor("default", time.Now().Add(time.Minute))
		ref := host.startRun(scheduledNow(host))
		host.await("the slot sleep is recorded", 0, func() bool { return len(host.timerFireTimes(ref)) == 1 })
		host.stopHost()

		host.startHost()
		host.awaitItem("bcst-1", StateSucceeded)
		host.awaitDurableState(ref, durable.RunSucceeded)
		claims := host.store.claimed()
		if len(claims) != 1 {
			t.Fatalf("slot claims = %v, want one", claims)
		}
		if recorded := host.actionResult(ref, "slot:bcst-1:a1"); recorded != claims[0].UTC().Format(time.RFC3339Nano) {
			t.Errorf("journaled slot = %s, want the claimed %s", recorded, claims[0])
		}
		if next := host.store.cursor("default"); !next.Equal(claims[0].Add(policy.DispatchGap)) {
			t.Errorf("next slot = %s, want one gap after %s", next, claims[0])
		}
		fires, sentAt := host.timerFireTimes(ref), host.primary.sentAt()
		if len(fires) != 1 || len(sentAt) != 1 || sentAt[0].Before(fires[0]) {
			t.Errorf("send at %v, slot sleeps until %v, want one send at the recorded slot", sentAt, fires)
		}
	})
	t.Run("each attempt claims its own slot", func(t *testing.T) {
		policy := testRunPolicy()
		policy.DispatchGap = time.Minute
		host := newBroadcastHost(t, &hostSetup{policy: policy})
		host.store.setCursor("default", time.Now().Add(time.Minute))
		host.primary.script(failsFirstWith(SendOutcome{}, errors.New("connection reset")))
		ref := host.startRun(scheduledNow(host))
		host.awaitItem("bcst-1", StateSucceeded)
		host.awaitDurableState(ref, durable.RunSucceeded)

		claims := host.store.claimed()
		if len(claims) != 2 || !claims[1].Equal(claims[0].Add(policy.DispatchGap)) {
			t.Fatalf("slot claims = %v, want two, one gap apart", claims)
		}
		keys := host.actionKeys(ref)
		for _, key := range []string{"slot:bcst-1:a1", "slot:bcst-1:a2"} {
			if !slices.Contains(keys, key) {
				t.Errorf("action rows = %v, want %s", keys, key)
			}
		}
		fires, sentAt := host.timerFireTimes(ref), host.primary.sentAt()
		if len(fires) != 3 || len(sentAt) != 2 {
			t.Fatalf("timers = %v, sends at %v, want a slot sleep, a backoff and a slot sleep before two sends", fires, sentAt)
		}
		if !fires[2].After(fires[1]) || sentAt[1].Before(fires[2]) {
			t.Errorf("second send at %s, backoff until %s, second slot until %s, want the send at its own slot after the backoff", sentAt[1], fires[1], fires[2])
		}
	})
}

func TestRunnerKeepsARunWhoseHostLostItsLease(t *testing.T) {
	host := newBroadcastHost(t, &hostSetup{policy: testRunPolicy(), block: "primary", nth: 1})
	host.primary.script(func(ctx context.Context, call int) (SendOutcome, error) {
		if call == 1 {
			return SendOutcome{}, ctx.Err()
		}
		return SendOutcome{Succeeded: true, IntentID: "intent-delivered"}, nil
	})
	ref := host.startRun(scheduledNow(host))
	host.awaitTripped()
	host.dropLease()
	host.awaitDurableState(ref, durable.RunSucceeded)

	if state, _, effect := host.store.stateOf("bcst-1"); state != StateSucceeded || effect != "intent-delivered" {
		t.Errorf("item = %s linked to %q, want succeeded linked to the delivered intent", state, effect)
	}
	if settles := host.store.count("Settle"); settles != 1 {
		t.Errorf("settles = %d, want 1", settles)
	}
	sent := host.primary.sent()
	if len(sent) != 2 || sent[0].IdempotencyKey != sent[1].IdempotencyKey {
		t.Errorf("primary sends = %+v, want the interrupted send made again under its key", sent)
	}
}

func TestRunnerStopsBetweenTheDeliveriesOfAWindow(t *testing.T) {
	base := time.Now().Add(-time.Hour)
	single := func(h *broadcastHost, at time.Time) {
		h.seed("bcst-single", StateHeld, at)
	}
	group := func(h *broadcastHost, prefix string, at time.Time) {
		h.seed(prefix+"-1", StateHeld, at)
		h.seed(prefix+"-2", StateHeld, at)
	}
	cases := []preemptCase{
		{name: "the settle of the single", block: "Settle", nth: 1, arrange: func(h *broadcastHost) string {
			single(h, base)
			group(h, "bcst-group", base.Add(time.Minute))
			return "bcst-single"
		}},
		{name: "the listing of the window", block: "ListHeld", nth: 1, arrange: func(h *broadcastHost) string {
			group(h, "bcst-group", base)
			single(h, base.Add(time.Minute))
			return "bcst-group-1"
		}},
		{name: "a digest that another run took", block: "CreateDigest", nth: 1, arrange: func(h *broadcastHost) string {
			group(h, "bcst-group", base)
			group(h, "bcst-later", base.Add(time.Minute))
			return "bcst-group-1"
		}, meanwhile: func(h *broadcastHost) {
			h.store.settleElsewhere("bcst-group-2", StateCancelled)
		}},
		{name: "the digest of a group", block: "CreateDigest", nth: 1, arrange: func(h *broadcastHost) string {
			group(h, "bcst-group", base)
			return "bcst-group-1"
		}},
	}
	for i := range cases {
		t.Run(cases[i].name, func(t *testing.T) { runPreemptCase(t, &cases[i]) })
	}
}

func TestRunnerWritesNothingAfterItsContextEnded(t *testing.T) {
	expired := func(h *broadcastHost) string {
		h.seed("bcst-1", StateScheduled, time.Now().Add(-25*time.Hour))
		return "bcst-1"
	}
	paced := func(h *broadcastHost) string {
		h.store.setCursor("default", time.Now().Add(time.Minute))
		return scheduledNow(h)
	}
	cases := []preemptCase{
		{name: "the load in the handler", block: "Load", nth: 1, arrange: scheduledNow},
		{name: "the start clock of an expired item", block: "clock:start", nth: 1, arrange: expired},
		{name: "the settle of an expired item", block: "Settle", nth: 1, arrange: expired},
		{name: "the release sleep", block: "Sleep", nth: 1, arrange: func(h *broadcastHost) string {
			h.seed("bcst-1", StateScheduled, time.Now().Add(time.Hour))
			return "bcst-1"
		}},
		{name: "the load in the delivery", block: "Load", nth: 2, arrange: scheduledNow},
		{name: "the attempt clock", block: "clock:attempt:bcst-1:a1", nth: 1, arrange: scheduledNow},
		{name: "the dispatch slot claim of a paced item", block: "ClaimDispatchSlot", nth: 1, ownRow: "slot:", arrange: paced},
		{name: "the slot sleep", block: "Sleep", nth: 1, arrange: paced},
		{name: "the attempt note", block: "NoteAttempt", nth: 1, arrange: scheduledNow},
		{name: "a primary send that succeeded", block: "primary", nth: 1, ownRow: "send:", arrange: scheduledNow},
		{name: "the backoff sleep", block: "Sleep", nth: 1, arrange: func(h *broadcastHost) string {
			h.primary.script(failsFirstWith(SendOutcome{}, errors.New("connection reset")))
			return scheduledNow(h)
		}},
		{name: "the settle of a delivery", block: "Settle", nth: 1, arrange: scheduledNow},
	}
	for i := range cases {
		t.Run(cases[i].name, func(t *testing.T) { runPreemptCase(t, &cases[i]) })
	}
}
