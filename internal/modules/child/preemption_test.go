package child

import (
	stdcontext "context"
	"database/sql"
	"log/slog"
	"slices"
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
	hostedChild = "child/ch-1"
)

type recordingRuns struct {
	mu      sync.Mutex
	run     HandlerRun
	states  []string
	results int
}

func (r *recordingRuns) GetRun(_ stdcontext.Context, id string) (HandlerRun, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.run, id == r.run.ID, nil
}

func (r *recordingRuns) SetState(_ stdcontext.Context, _, state string, _ time.Time) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.states = append(r.states, state)
	r.run.State = state
	return nil
}

func (r *recordingRuns) SetResult(_ stdcontext.Context, _ string, _ *Result, _ time.Time) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.results++
	return nil
}

func (r *recordingRuns) recorded() (states []string, results int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.states...), r.results
}

type recordingSignaler struct {
	mu      sync.Mutex
	signals []string
}

func (s *recordingSignaler) SignalChild(_ stdcontext.Context, _, _ string, payload []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.signals = append(s.signals, string(payload))
	return nil
}

func (s *recordingSignaler) sent() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.signals...)
}

func (f *fakeHandlerLedger) counts() (charged, released int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.charged), len(f.released)
}

type scriptedSessions struct {
	mu      sync.Mutex
	calls   int
	respond func(ctx stdcontext.Context, call int) (Result, error)
}

func (s *scriptedSessions) RunSession(ctx stdcontext.Context, _ RunRequest) (Result, error) {
	s.mu.Lock()
	s.calls++
	call := s.calls
	s.mu.Unlock()
	return s.respond(ctx, call)
}

func (s *scriptedSessions) CancelSession(stdcontext.Context, string) error {
	return nil
}

func (s *scriptedSessions) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

func completedSession() Result {
	return Result{Status: "completed", Output: "done", TokensUsed: 1, CompletedAt: time.Now().UTC()}
}

func failsOnceWhenItsContextEnds(entered chan struct{}) func(ctx stdcontext.Context, call int) (Result, error) {
	return func(ctx stdcontext.Context, call int) (Result, error) {
		if call > 1 {
			return completedSession(), nil
		}
		close(entered)
		<-ctx.Done()
		return Result{}, ctx.Err()
	}
}

type childHost struct {
	t        *testing.T
	db       *sql.DB
	runs     *recordingRuns
	ledger   *fakeHandlerLedger
	signaler *recordingSignaler
	handler  *Handler
	runtime  *sqlite.Runtime
	stop     stdcontext.CancelFunc
	stopped  chan error
}

func newChildHost(t *testing.T, sessions *scriptedSessions) *childHost {
	t.Helper()
	runs := &recordingRuns{run: HandlerRun{ID: "ch-1", SessionID: "sess-child", State: StatusQueued, Deadline: time.Now().UTC().Add(time.Hour), ContextDigest: "digest-ch-1", DurableKey: hostedChild, ParentInvocation: "inv-1"}}
	ledger := &fakeHandlerLedger{}
	handler, err := NewHandlerWithLedger(runs, sessions, ledger)
	if err != nil {
		t.Fatalf("NewHandlerWithLedger: %v", err)
	}
	signaler := &recordingSignaler{}
	handler.SetSignaler(signaler)
	host := &childHost{t: t, db: storetest.OpenMigrated(t), runs: runs, ledger: ledger, signaler: signaler, handler: handler}
	host.startHost()
	t.Cleanup(host.stopHost)
	return host
}

func (h *childHost) startHost() {
	h.t.Helper()
	rt, err := sqlite.New(&sqlite.Options{DB: h.db, Logger: slog.New(slog.DiscardHandler), PollInterval: hostPoll, HostTTL: hostTTL, StopGrace: hostGrace})
	if err != nil {
		h.t.Fatalf("sqlite.New: %v", err)
	}
	rt.RegisterHandler(HandlerName, func(ctx stdcontext.Context, inv durable.Invocation) error {
		return h.handler.HandleDurable(ctx, inv, time.Now)
	})
	ctx, stop := stdcontext.WithCancel(stdcontext.WithoutCancel(h.t.Context()))
	stopped := make(chan error, 1)
	go func() { stopped <- rt.Run(ctx) }()
	h.runtime, h.stop, h.stopped = rt, stop, stopped
}

func (h *childHost) stopHost() {
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

func (h *childHost) startChild() durable.RunRef {
	h.t.Helper()
	ref, err := h.runtime.Start(h.t.Context(), durable.StartRequest{Handler: HandlerName, Key: hostedChild, Payload: startPayload(h.t)})
	if err != nil {
		h.t.Fatalf("start the child run: %v", err)
	}
	return ref
}

func (h *childHost) durableState(ref durable.RunRef) durable.RunState {
	h.t.Helper()
	status, err := h.runtime.Status(h.t.Context(), ref)
	if err != nil {
		h.t.Fatalf("status of %s: %v", ref.Key, err)
	}
	return status.State
}

func (h *childHost) awaitDurableState(ref durable.RunRef, state durable.RunState) {
	h.t.Helper()
	deadline := time.NewTimer(hostTimeout)
	defer deadline.Stop()
	tick := time.NewTicker(2 * time.Millisecond)
	defer tick.Stop()
	for h.durableState(ref) != state {
		select {
		case <-tick.C:
		case <-deadline.C:
			h.t.Fatalf("durable run %s = %s, want %s", ref.Key, h.durableState(ref), state)
		}
	}
}

func (h *childHost) dropLease() {
	h.t.Helper()
	if _, err := h.db.ExecContext(h.t.Context(), `DELETE FROM durable_host`); err != nil {
		h.t.Fatalf("delete the host lease: %v", err)
	}
}

func (h *childHost) requireUnsettled() {
	h.t.Helper()
	states, results := h.runs.recorded()
	if len(states) == 0 || slices.ContainsFunc(states, func(state string) bool { return state != StatusRunning }) {
		h.t.Errorf("states = %v, want running only", states)
	}
	if results != 0 {
		h.t.Errorf("stored results = %d, want 0", results)
	}
	if charged, released := h.ledger.counts(); charged != 0 || released != 0 {
		h.t.Errorf("ledger charges = %d, releases = %d, want none", charged, released)
	}
	if signals := h.signaler.sent(); len(signals) != 0 {
		h.t.Errorf("settlement signals = %v, want none", signals)
	}
}

type settlement struct {
	state    string
	charged  int
	released int
	results  int
	signals  int
}

func (h *childHost) requireSettled(want settlement) {
	h.t.Helper()
	states, results := h.runs.recorded()
	charged, released := h.ledger.counts()
	got := settlement{charged: charged, released: released, results: results, signals: len(h.signaler.sent())}
	if len(states) > 0 {
		got.state = states[len(states)-1]
	}
	if got != want {
		h.t.Errorf("settlement = %+v, want %+v (states %v)", got, want, states)
	}
}

func awaitClosed(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(hostTimeout):
		t.Fatalf("timed out waiting until %s", what)
	}
}

func TestHandlerLeavesAChildRunningWhenItsHostStops(t *testing.T) {
	entered := make(chan struct{})
	sessions := &scriptedSessions{respond: failsOnceWhenItsContextEnds(entered)}
	host := newChildHost(t, sessions)
	ref := host.startChild()
	awaitClosed(t, entered, "the session runs")
	host.stopHost()

	host.requireUnsettled()
	if state := host.durableState(ref); state != durable.RunRunning {
		t.Errorf("durable run after the stop = %s, want %s", state, durable.RunRunning)
	}

	host.startHost()
	host.awaitDurableState(ref, durable.RunSucceeded)
	host.requireSettled(settlement{state: StatusSucceeded, charged: 1, released: 1, results: 1, signals: 1})
	if calls := sessions.callCount(); calls != 2 {
		t.Errorf("executor calls = %d, want 2", calls)
	}
}

func TestHandlerDoesNotJournalASessionItsHostPreempted(t *testing.T) {
	cases := []struct {
		name  string
		first func() Result
		calls int
	}{
		{name: "a session the stop cancelled runs again", first: func() Result { return Result{Status: "cancelled"} }, calls: 2},
		{name: "a session that completed after the stop is kept", first: completedSession, calls: 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			entered := make(chan struct{})
			sessions := &scriptedSessions{respond: func(ctx stdcontext.Context, call int) (Result, error) {
				if call > 1 {
					return completedSession(), nil
				}
				close(entered)
				<-ctx.Done()
				return tc.first(), nil
			}}
			host := newChildHost(t, sessions)
			ref := host.startChild()
			awaitClosed(t, entered, "the session runs")
			host.stopHost()
			host.requireUnsettled()

			host.startHost()
			host.awaitDurableState(ref, durable.RunSucceeded)
			host.requireSettled(settlement{state: StatusSucceeded, charged: 1, released: 1, results: 1, signals: 1})
			if states, _ := host.runs.recorded(); slices.Contains(states, StatusCancelled) {
				t.Errorf("states = %v, want no %s", states, StatusCancelled)
			}
			if calls := sessions.callCount(); calls != tc.calls {
				t.Errorf("executor calls = %d, want %d", calls, tc.calls)
			}
		})
	}
}

func TestHandlerKeepsAChildWhoseHostLostItsLease(t *testing.T) {
	entered := make(chan struct{})
	sessions := &scriptedSessions{respond: failsOnceWhenItsContextEnds(entered)}
	host := newChildHost(t, sessions)
	ref := host.startChild()
	awaitClosed(t, entered, "the session runs")
	host.dropLease()
	host.awaitDurableState(ref, durable.RunSucceeded)

	host.requireSettled(settlement{state: StatusSucceeded, charged: 1, released: 1, results: 1, signals: 1})
	if states, _ := host.runs.recorded(); slices.Contains(states, StatusCancelled) {
		t.Errorf("states = %v, want no %s", states, StatusCancelled)
	}
	if calls := sessions.callCount(); calls != 2 {
		t.Errorf("executor calls = %d, want 2", calls)
	}
}

func TestHandlerStillCancelsACancelledChild(t *testing.T) {
	entered := make(chan struct{})
	sessions := &scriptedSessions{respond: failsOnceWhenItsContextEnds(entered)}
	host := newChildHost(t, sessions)
	ref := host.startChild()
	awaitClosed(t, entered, "the session runs")
	if err := host.runtime.Cancel(t.Context(), ref); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	host.awaitDurableState(ref, durable.RunCancelled)

	host.requireSettled(settlement{state: StatusCancelled, charged: 0, released: 1, results: 0, signals: 1})
}
