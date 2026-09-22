package child

import (
	stdcontext "context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/durable"
)

type fakeHandlerRuns struct {
	runs    map[string]HandlerRun
	states  []string
	results map[string]Result
}

func (f *fakeHandlerRuns) GetRun(_ stdcontext.Context, id string) (HandlerRun, bool, error) {
	run, ok := f.runs[id]
	return run, ok, nil
}

func (f *fakeHandlerRuns) SetState(_ stdcontext.Context, id, state string, _ time.Time) error {
	f.states = append(f.states, id+":"+state)
	if run, ok := f.runs[id]; ok {
		run.State = state
		f.runs[id] = run
	}
	return nil
}

func (f *fakeHandlerRuns) SetResult(_ stdcontext.Context, id string, result *Result, _ time.Time) error {
	if f.results == nil {
		f.results = map[string]Result{}
	}
	f.results[id] = *result
	return nil
}

type fakeSessionExecutor struct {
	mu     sync.Mutex
	calls  int
	result Result
	err    error
	cancel error
}

func (f *fakeSessionExecutor) RunSession(_ stdcontext.Context, sessionID string, _ time.Time) (Result, error) {
	f.mu.Lock()
	f.calls++
	f.mu.Unlock()
	if sessionID == "" {
		return Result{}, errors.New("empty session")
	}
	return f.result, f.err
}

func (f *fakeSessionExecutor) Calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func (f *fakeSessionExecutor) CancelSession(_ stdcontext.Context, _ string) error {
	return f.cancel
}

type fakeHandlerLedger struct {
	mu       sync.Mutex
	charged  []string
	released []string
}

func (f *fakeHandlerLedger) Reserve(_ stdcontext.Context, invocationID, ownerID string, maxTokens, maxCost int64) (BudgetReservation, error) {
	return BudgetReservation{ID: "res-" + invocationID, ExpiresAt: time.Now().UTC().Add(time.Hour)}, nil
}

func (f *fakeHandlerLedger) Charge(_ stdcontext.Context, reservationID string, tokens, cost int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.charged = append(f.charged, reservationID)
	return nil
}

func (f *fakeHandlerLedger) Release(_ stdcontext.Context, reservationID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.released = append(f.released, reservationID)
	return nil
}

type fakeJournalInvocation struct {
	mu      sync.Mutex
	payload []byte
	journal map[string][]byte
}

func newFakeJournal(payload []byte) *fakeJournalInvocation {
	return &fakeJournalInvocation{payload: payload, journal: map[string][]byte{}}
}

func (f *fakeJournalInvocation) Run() durable.RunRef { return durable.RunRef{Key: "test"} }
func (f *fakeJournalInvocation) Payload() []byte     { return f.payload }
func (f *fakeJournalInvocation) Signal(_ stdcontext.Context, _ string) ([]byte, bool) {
	return nil, false
}
func (f *fakeJournalInvocation) Sleep(_ time.Duration) error { return nil }
func (f *fakeJournalInvocation) Timer(d time.Duration) <-chan time.Time {
	ch := make(chan time.Time, 1)
	ch <- time.Now().UTC()
	close(ch)
	return ch
}
func (f *fakeJournalInvocation) Wait(_ stdcontext.Context, _ string, _ time.Duration) (payload []byte, timedOut, ok bool) {
	return nil, false, false
}
func (f *fakeJournalInvocation) RunAction(ctx stdcontext.Context, key string, fn func(stdcontext.Context) ([]byte, error)) ([]byte, error) {
	f.mu.Lock()
	if cached, ok := f.journal[key]; ok {
		out := append([]byte(nil), cached...)
		f.mu.Unlock()
		return out, nil
	}
	f.mu.Unlock()
	runCtx := stdcontext.WithoutCancel(ctx)
	out, err := fn(runCtx)
	if err != nil {
		return nil, err
	}
	f.mu.Lock()
	f.journal[key] = append([]byte(nil), out...)
	f.mu.Unlock()
	return out, nil
}

func testHandler() (*Handler, *fakeHandlerRuns, *fakeSessionExecutor) {
	runs := &fakeHandlerRuns{runs: map[string]HandlerRun{
		"ch-1": {ID: "ch-1", SessionID: "sess-child", State: StatusQueued, Deadline: time.Now().UTC().Add(time.Minute)},
	}}
	exec := &fakeSessionExecutor{result: Result{Status: "completed", Output: "done"}}
	handler, err := NewHandler(runs, exec)
	if err != nil {
		panic(err)
	}
	return handler, runs, exec
}

func startPayload(t *testing.T) []byte {
	t.Helper()
	raw, err := json.Marshal(StartPayload{ChildID: "ch-1", SessionID: "sess-child", DurableKey: "child/ch-1", ReservationID: "res-1"})
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	return raw
}

func TestHandlerSettlesSuccess(t *testing.T) {
	handler, runs, _ := testHandler()
	now := time.Now().UTC()
	if err := handler.HandleInvocation(t.Context(), startPayload(t), func() time.Time { return now }); err != nil {
		t.Fatalf("HandleInvocation: %v", err)
	}
	if len(runs.states) != 2 || runs.states[0] != "ch-1:running" || runs.states[1] != "ch-1:succeeded" {
		t.Fatalf("states = %v", runs.states)
	}
}

func TestHandlerTerminalMapping(t *testing.T) {
	for _, tc := range []struct {
		result Result
		err    error
		want   string
	}{
		{Result{Status: "completed"}, nil, StatusSucceeded},
		{Result{Status: "cancelled"}, nil, StatusCancelled},
		{Result{Status: "deadline"}, nil, StatusDeadline},
		{Result{Status: "completed"}, errors.New("boom"), StatusFailed},
		{Result{Status: "weird"}, nil, StatusFailed},
	} {
		runs := &fakeHandlerRuns{runs: map[string]HandlerRun{
			"ch-1": {ID: "ch-1", SessionID: "sess-child", State: StatusQueued, Deadline: time.Now().UTC().Add(time.Minute)},
		}}
		exec := &fakeSessionExecutor{result: tc.result, err: tc.err}
		handler, err := NewHandler(runs, exec)
		if err != nil {
			t.Fatalf("NewHandler: %v", err)
		}
		if err := handler.HandleInvocation(t.Context(), startPayload(t), time.Now().UTC); err == nil && tc.err != nil {
			t.Fatalf("expected propagated error for %+v", tc)
		}
		if got := runs.runs["ch-1"].State; got != tc.want {
			t.Errorf("result %+v err %v: state = %s, want %s", tc.result, tc.err, got, tc.want)
		}
	}
}

func TestHandlerRejects(t *testing.T) {
	handler, _, _ := testHandler()
	if err := handler.HandleInvocation(t.Context(), []byte("{nope"), time.Now().UTC); err == nil {
		t.Error("expected malformed payload rejection")
	}
	if err := handler.HandleInvocation(t.Context(), []byte(`{"child_id":"","session_id":""}`), time.Now().UTC); err == nil {
		t.Error("expected incomplete payload rejection")
	}
	missing, err := json.Marshal(StartPayload{ChildID: "ghost", SessionID: "sess-ghost"})
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	if err := handler.HandleInvocation(t.Context(), missing, time.Now().UTC); err == nil {
		t.Error("expected missing child rejection")
	}
	var nilCtx stdcontext.Context
	if err := handler.HandleInvocation(nilCtx, startPayload(t), time.Now().UTC); err == nil {
		t.Error("expected nil context rejection")
	}
	if err := handler.HandleInvocation(t.Context(), startPayload(t), nil); err == nil {
		t.Error("expected nil clock rejection")
	}
	if _, err := NewHandler(nil, &fakeSessionExecutor{}); err == nil {
		t.Error("expected nil runs rejection")
	}
	if _, err := NewHandler(&fakeHandlerRuns{}, nil); err == nil {
		t.Error("expected nil executor rejection")
	}
}

func TestHandlerJournaledExecution(t *testing.T) {
	runs := &fakeHandlerRuns{runs: map[string]HandlerRun{
		"ch-1": {ID: "ch-1", SessionID: "sess-child", State: StatusQueued, Deadline: time.Now().UTC().Add(time.Minute)},
	}}
	exec := &fakeSessionExecutor{result: Result{Status: "completed", Output: "done", TokensUsed: 10, CostMicros: 5, CompletedAt: time.Now().UTC()}}
	handler, err := NewHandler(runs, exec)
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	inv := newFakeJournal(startPayload(t))
	if err := handler.HandleDurable(t.Context(), inv, time.Now().UTC); err != nil {
		t.Fatalf("HandleDurable: %v", err)
	}
	if exec.Calls() != 1 {
		t.Fatalf("executor calls = %d, want 1", exec.Calls())
	}
	if got := runs.runs["ch-1"].State; got != StatusSucceeded {
		t.Fatalf("state = %s, want succeeded", got)
	}
}

func TestHandlerCrashResumeWithoutDuplicateWork(t *testing.T) {
	runs := &fakeHandlerRuns{runs: map[string]HandlerRun{
		"ch-1": {ID: "ch-1", SessionID: "sess-child", State: StatusQueued, Deadline: time.Now().UTC().Add(time.Minute)},
	}}
	exec := &fakeSessionExecutor{result: Result{Status: "completed", Output: "done", CompletedAt: time.Now().UTC()}}
	handler, err := NewHandler(runs, exec)
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	payload := startPayload(t)
	inv := newFakeJournal(payload)
	if err := handler.HandleDurable(t.Context(), inv, time.Now().UTC); err != nil {
		t.Fatalf("first HandleDurable: %v", err)
	}
	if exec.Calls() != 1 {
		t.Fatalf("first calls = %d, want 1", exec.Calls())
	}
	runs.runs["ch-1"] = HandlerRun{ID: "ch-1", SessionID: "sess-child", State: StatusQueued, Deadline: time.Now().UTC().Add(time.Minute)}
	runs.states = nil
	if err := handler.HandleDurable(t.Context(), inv, time.Now().UTC); err != nil {
		t.Fatalf("replay HandleDurable: %v", err)
	}
	if exec.Calls() != 1 {
		t.Fatalf("replay must not re-execute, calls = %d, want 1", exec.Calls())
	}
	if got := runs.runs["ch-1"].State; got != StatusSucceeded {
		t.Fatalf("replay state = %s, want succeeded", got)
	}
}

func TestHandlerDeadlineMapping(t *testing.T) {
	runs := &fakeHandlerRuns{runs: map[string]HandlerRun{
		"ch-1": {ID: "ch-1", SessionID: "sess-child", State: StatusQueued, Deadline: time.Now().UTC().Add(-time.Minute)},
	}}
	exec := &fakeSessionExecutor{result: Result{Status: "completed", CompletedAt: time.Now().UTC()}}
	handler, err := NewHandler(runs, exec)
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	if err := handler.HandleInvocation(t.Context(), startPayload(t), time.Now().UTC); err == nil {
		t.Fatal("past deadline must fail")
	}
	if got := runs.runs["ch-1"].State; got != StatusDeadline {
		t.Fatalf("past deadline state = %s, want deadline_exceeded", got)
	}
	if exec.Calls() != 0 {
		t.Fatalf("past deadline must not execute, calls = %d", exec.Calls())
	}
	runs2 := &fakeHandlerRuns{runs: map[string]HandlerRun{
		"ch-1": {ID: "ch-1", SessionID: "sess-child", State: StatusQueued, Deadline: time.Now().UTC().Add(time.Minute)},
	}}
	exec2 := &fakeSessionExecutor{result: Result{Status: "deadline", CompletedAt: time.Now().UTC()}}
	handler2, _ := NewHandler(runs2, exec2)
	if err := handler2.HandleInvocation(t.Context(), startPayload(t), time.Now().UTC); err != nil {
		t.Fatalf("deadline result: %v", err)
	}
	if got := runs2.runs["ch-1"].State; got != StatusDeadline {
		t.Fatalf("deadline result state = %s", got)
	}
}

func TestHandlerBudgetChargeAndRelease(t *testing.T) {
	runs := &fakeHandlerRuns{runs: map[string]HandlerRun{
		"ch-1": {ID: "ch-1", SessionID: "sess-child", State: StatusQueued, Deadline: time.Now().UTC().Add(time.Minute)},
	}}
	exec := &fakeSessionExecutor{result: Result{Status: "completed", TokensUsed: 42, CostMicros: 7, CompletedAt: time.Now().UTC()}}
	ledger := &fakeHandlerLedger{}
	handler, err := NewHandlerWithLedger(runs, exec, ledger)
	if err != nil {
		t.Fatalf("NewHandlerWithLedger: %v", err)
	}
	if err := handler.HandleInvocation(t.Context(), startPayload(t), time.Now().UTC); err != nil {
		t.Fatalf("HandleInvocation: %v", err)
	}
	if len(ledger.charged) != 1 || ledger.charged[0] != "res-1" {
		t.Fatalf("charged = %v, want [res-1]", ledger.charged)
	}
	if len(ledger.released) != 1 || ledger.released[0] != "res-1" {
		t.Fatalf("released = %v, want [res-1]", ledger.released)
	}
}

func TestHandlerPersistsTypedResult(t *testing.T) {
	runs := &fakeHandlerRuns{runs: map[string]HandlerRun{
		"ch-1": {ID: "ch-1", SessionID: "sess-child", State: StatusQueued, Deadline: time.Now().UTC().Add(time.Minute)},
	}}
	exec := &fakeSessionExecutor{result: Result{Status: "completed", Output: "summary", TokensUsed: 3, CostMicros: 1, CompletedAt: time.Now().UTC()}}
	handler, err := NewHandler(runs, exec)
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	if err := handler.HandleInvocation(t.Context(), startPayload(t), time.Now().UTC); err != nil {
		t.Fatalf("HandleInvocation: %v", err)
	}
	got, ok := runs.results["ch-1"]
	if !ok {
		t.Fatal("typed result was not persisted")
	}
	if got.Output != "summary" || got.TokensUsed != 3 {
		t.Fatalf("persisted result = %+v", got)
	}
	badRuns := &fakeHandlerRuns{runs: map[string]HandlerRun{
		"ch-1": {ID: "ch-1", SessionID: "sess-child", State: StatusQueued, Deadline: time.Now().UTC().Add(time.Minute)},
	}}
	badExec := &fakeSessionExecutor{result: Result{Status: "completed", TokensUsed: -1, CompletedAt: time.Now().UTC()}}
	badHandler, _ := NewHandler(badRuns, badExec)
	if err := badHandler.HandleInvocation(t.Context(), startPayload(t), time.Now().UTC); err == nil {
		t.Fatal("negative usage must fail")
	}
	if got := badRuns.runs["ch-1"].State; got != StatusFailed {
		t.Fatalf("invalid result state = %s, want failed", got)
	}
}

func TestHandlerNonResumableBecomesInterrupted(t *testing.T) {
	runs := &fakeHandlerRuns{runs: map[string]HandlerRun{
		"ch-1": {ID: "ch-1", SessionID: "sess-child", State: StatusQueued, Deadline: time.Now().UTC().Add(time.Minute)},
	}}
	exec := &fakeSessionExecutor{err: ErrNonResumable}
	handler, err := NewHandler(runs, exec)
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	if err := handler.HandleInvocation(t.Context(), startPayload(t), time.Now().UTC); err == nil {
		t.Fatal("non-resumable must propagate")
	}
	if got := runs.runs["ch-1"].State; got != StatusInterrupted {
		t.Fatalf("non-resumable state = %s, want interrupted", got)
	}
}
