package child

import (
	stdcontext "context"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

type fakeHandlerRuns struct {
	runs   map[string]HandlerRun
	states []string
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

type fakeSessionExecutor struct {
	result Result
	err    error
	cancel error
}

func (f *fakeSessionExecutor) RunSession(_ stdcontext.Context, sessionID string, _ time.Time) (Result, error) {
	if sessionID == "" {
		return Result{}, errors.New("empty session")
	}
	return f.result, f.err
}

func (f *fakeSessionExecutor) CancelSession(_ stdcontext.Context, _ string) error {
	return f.cancel
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
