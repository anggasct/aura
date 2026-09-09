package runtimesessions

import (
	"testing"

	"github.com/anggasct/aura/internal/runtime"
)

func testDescriptor(turnID string) *Descriptor {
	return &Descriptor{
		TurnID:         turnID,
		SessionID:      "session-1",
		PrincipalID:    "owner",
		Origin:         "terminal",
		Parts:          []Part{{Text: "hello"}},
		IdempotencyKey: "key-" + turnID,
	}
}

func codeOf(t *testing.T, err error) runtime.ErrorCode {
	t.Helper()
	if err == nil {
		t.Fatal("expected an error")
	}
	code, ok := runtime.CodeOf(err)
	if !ok {
		t.Fatalf("expected a coded error, got %v", err)
	}
	return code
}

func TestEnqueueFirstTurnStartsImmediately(t *testing.T) {
	state := NewState()
	result, err := Enqueue(state, testDescriptor("turn-1"), 1, 4)
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if result.ToStart == nil || result.ToStart.Sequence != 1 {
		t.Fatalf("sequence = %+v, want 1", result.ToStart)
	}
	if result.ToStart == nil || result.ToStart.Descriptor.TurnID != "turn-1" {
		t.Fatalf("first turn must start immediately, got %+v", result.ToStart)
	}
	if state.Active == nil || state.Active.Descriptor.TurnID != "turn-1" {
		t.Fatalf("active turn not recorded: %+v", state.Active)
	}
}

func TestEnqueueWhileActiveQueuesFIFO(t *testing.T) {
	state := NewState()
	if _, err := Enqueue(state, testDescriptor("turn-1"), 1, 4); err != nil {
		t.Fatalf("enqueue turn-1: %v", err)
	}
	for i, id := range []string{"turn-2", "turn-3"} {
		result, err := Enqueue(state, testDescriptor(id), uint64(i+2), 4)
		if err != nil {
			t.Fatalf("enqueue %s: %v", id, err)
		}
		if result.ToStart != nil {
			t.Fatalf("turn %s must queue while turn-1 is active", id)
		}
	}
	released, err := Release(state, "turn-1")
	if err != nil {
		t.Fatalf("release: %v", err)
	}
	if released.ToStart == nil || released.ToStart.Descriptor.TurnID != "turn-2" {
		t.Fatalf("expected turn-2 to start, got %+v", released.ToStart)
	}
	released, err = Release(state, "turn-2")
	if err != nil {
		t.Fatalf("release turn-2: %v", err)
	}
	if released.ToStart == nil || released.ToStart.Descriptor.TurnID != "turn-3" {
		t.Fatalf("expected turn-3 to start, got %+v", released.ToStart)
	}
	released, err = Release(state, "turn-3")
	if err != nil {
		t.Fatalf("release turn-3: %v", err)
	}
	if released.ToStart != nil {
		t.Fatalf("no turn must start on empty queue, got %+v", released.ToStart)
	}
}

func TestEnqueueRespectsBound(t *testing.T) {
	state := NewState()
	if _, err := Enqueue(state, testDescriptor("turn-1"), 1, 1); err != nil {
		t.Fatalf("enqueue turn-1: %v", err)
	}
	if _, err := Enqueue(state, testDescriptor("turn-2"), 2, 1); err == nil {
		t.Fatal("expected overload above max pending")
	} else if code := codeOf(t, err); code != runtime.ErrorCodeRuntimeOverloaded {
		t.Fatalf("code = %q, want runtime_overloaded", code)
	}
}

func TestReplayShortCircuit(t *testing.T) {
	state := NewState()
	if _, err := Enqueue(state, testDescriptor("turn-1"), 1, 4); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	original, err := CheckReplay(state, "key-turn-1")
	if err != nil {
		t.Fatalf("check replay: %v", err)
	}
	if original != "turn-1" {
		t.Fatalf("replay original = %q, want turn-1", original)
	}
	miss, err := CheckReplay(state, "key-unknown")
	if err != nil {
		t.Fatalf("check replay miss: %v", err)
	}
	if miss != "" {
		t.Fatalf("replay miss = %q, want empty", miss)
	}
	if err := NoteReplay(state, "key-external", "turn-9"); err != nil {
		t.Fatalf("note replay: %v", err)
	}
	original, err = CheckReplay(state, "key-external")
	if err != nil {
		t.Fatalf("check noted replay: %v", err)
	}
	if original != "turn-9" {
		t.Fatalf("noted replay original = %q, want turn-9", original)
	}
}

func TestReleaseMismatchFailsLoudly(t *testing.T) {
	state := NewState()
	if _, err := Enqueue(state, testDescriptor("turn-1"), 1, 4); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if _, err := Release(state, "turn-2"); err == nil {
		t.Fatal("expected an error releasing a non-active turn")
	} else if code := codeOf(t, err); code != runtime.ErrorCodeInvalidArgument {
		t.Fatalf("code = %q, want invalid_argument", code)
	}
	if _, err := Release(state, ""); err == nil {
		t.Fatal("expected an error releasing an empty turn id")
	}
}

func TestRecoverRestartsOpenTurnsInOrder(t *testing.T) {
	state := NewState()
	for i, id := range []string{"turn-1", "turn-2", "turn-3"} {
		if _, err := Enqueue(state, testDescriptor(id), uint64(i+1), 8); err != nil {
			t.Fatalf("enqueue %s: %v", id, err)
		}
	}
	restarted, found, err := Recover(state, []string{"turn-1", "turn-2", "turn-3"}, nil)
	if err != nil {
		t.Fatalf("recover: %v", err)
	}
	if !found || restarted.Descriptor.TurnID != "turn-1" {
		t.Fatalf("expected turn-1 to restart, got %+v found=%v", restarted, found)
	}
	if len(state.Queue) != 2 || state.Queue[0].Descriptor.TurnID != "turn-2" {
		t.Fatalf("queue order not preserved: %+v", state.Queue)
	}
}

func TestRecoverDropsTerminalActive(t *testing.T) {
	state := NewState()
	for i, id := range []string{"turn-1", "turn-2"} {
		if _, err := Enqueue(state, testDescriptor(id), uint64(i+1), 8); err != nil {
			t.Fatalf("enqueue %s: %v", id, err)
		}
	}
	restarted, found, err := Recover(state, []string{"turn-2"}, []string{"turn-1"})
	if err != nil {
		t.Fatalf("recover: %v", err)
	}
	if !found || restarted.Descriptor.TurnID != "turn-2" {
		t.Fatalf("expected turn-2 to restart, got %+v found=%v", restarted, found)
	}
	if _, ok := state.Dedupe["key-turn-1"]; !ok {
		t.Fatal("completed turn dedupe entry must be retained")
	}
}

func TestRecoverPrunesUnknownEntries(t *testing.T) {
	state := NewState()
	for i, id := range []string{"turn-1", "turn-2"} {
		if _, err := Enqueue(state, testDescriptor(id), uint64(i+1), 8); err != nil {
			t.Fatalf("enqueue %s: %v", id, err)
		}
	}
	if err := NoteReplay(state, "key-ghost", "turn-ghost"); err != nil {
		t.Fatalf("note replay: %v", err)
	}
	_, found, err := Recover(state, nil, nil)
	if err != nil {
		t.Fatalf("recover: %v", err)
	}
	if found {
		t.Fatal("no turn must restart when nothing is open")
	}
	if len(state.Queue) != 0 || state.Active != nil {
		t.Fatalf("queue and active must be empty: %+v %+v", state.Queue, state.Active)
	}
	if len(state.Dedupe) != 0 {
		t.Fatalf("dedupe must be pruned: %v", state.Dedupe)
	}
}

func TestDedupeStaysBounded(t *testing.T) {
	state := NewState()
	for i := range MaxDedupeEntries + 10 {
		key := string(rune('a'+i%26)) + string(rune('0'+i/26)) + "-key"
		if err := NoteReplay(state, key, "turn-x"); err != nil {
			t.Fatalf("note replay: %v", err)
		}
	}
	if len(state.Dedupe) > MaxDedupeEntries {
		t.Fatalf("dedupe has %d entries, want at most %d", len(state.Dedupe), MaxDedupeEntries)
	}
}

func TestDecodeState(t *testing.T) {
	t.Run("empty input yields a fresh state", func(t *testing.T) {
		state, err := DecodeState(nil)
		if err != nil {
			t.Fatalf("decode empty: %v", err)
		}
		if state.SchemaVersion != SchemaVersion || state.Dedupe == nil {
			t.Fatalf("fresh state not initialized: %+v", state)
		}
	})
	t.Run("corrupt input fails loudly", func(t *testing.T) {
		if _, err := DecodeState([]byte("{nope")); err == nil {
			t.Fatal("expected a decode error")
		} else if code := codeOf(t, err); code != runtime.ErrorCodeInvalidArgument {
			t.Fatalf("code = %q, want invalid_argument", code)
		}
	})
	t.Run("unknown schema version fails loudly", func(t *testing.T) {
		if _, err := DecodeState([]byte(`{"schema_version":7,"dedupe":{}}`)); err == nil {
			t.Fatal("expected a version error")
		} else if code := codeOf(t, err); code != runtime.ErrorCodeInvalidArgument {
			t.Fatalf("code = %q, want invalid_argument", code)
		}
	})
	t.Run("validation rejects empty ids", func(t *testing.T) {
		state := NewState()
		if _, err := Enqueue(state, &Descriptor{}, 1, 4); err == nil {
			t.Fatal("expected a validation error")
		}
		if _, err := Enqueue(nil, testDescriptor("turn-1"), 1, 4); err == nil {
			t.Fatal("expected a nil-state error")
		}
	})
}

func TestAbortDropsQueueButKeepsDedupe(t *testing.T) {
	state := NewState()
	for i, id := range []string{"turn-1", "turn-2"} {
		if _, err := Enqueue(state, testDescriptor(id), uint64(i+1), 8); err != nil {
			t.Fatalf("enqueue %s: %v", id, err)
		}
	}
	dropped, err := Abort(state)
	if err != nil {
		t.Fatalf("abort: %v", err)
	}
	if len(dropped) != 2 || dropped[0].Descriptor.TurnID != "turn-1" || dropped[1].Descriptor.TurnID != "turn-2" {
		t.Fatalf("dropped turns out of order: %+v", dropped)
	}
	if len(state.Queue) != 0 || state.Active != nil {
		t.Fatalf("queue and active must be empty: %+v %+v", state.Queue, state.Active)
	}
	if _, ok := state.Dedupe["key-turn-1"]; !ok {
		t.Fatal("dedupe entries must survive abort")
	}
}

func TestEnqueueRejectsStaleSequence(t *testing.T) {
	state := NewState()
	if _, err := Enqueue(state, testDescriptor("turn-1"), 1, 4); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if _, err := Enqueue(state, testDescriptor("turn-2"), 1, 4); err == nil {
		t.Fatal("expected a stale-sequence error")
	} else if code := codeOf(t, err); code != runtime.ErrorCodeInvalidArgument {
		t.Fatalf("code = %q, want invalid_argument", code)
	}
	if _, err := Enqueue(state, testDescriptor("turn-3"), 0, 4); err == nil {
		t.Fatal("expected a zero-sequence error")
	}
}
