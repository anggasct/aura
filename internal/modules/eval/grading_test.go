package eval

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/anggasct/aura/internal/store"
)

func TestGradeTraceEnforcesTrajectoryAndPolicySignals(t *testing.T) {
	trace := NewTrace("exec-linux", "completed", []store.RuntimeEvent{
		{Sequence: 1, Kind: "turn.accepted"},
		{Sequence: 2, Kind: "tool.requested"},
		{Sequence: 3, Kind: "tool.completed"},
		{Sequence: 4, Kind: "turn.completed"},
	})
	grade := GradeTrace(trace, &TraceExpectation{
		Profile:        "exec-linux",
		Outcome:        "completed",
		ExpectedKinds:  []string{"turn.accepted", "tool.requested", "tool.completed", "turn.completed"},
		RequiredKinds:  []string{"tool.requested", "tool.completed"},
		ForbiddenKinds: []string{"policy.bypass"},
		MinScore:       100,
	})
	if !grade.Passed || grade.Score != 100 || grade.Version != "trace-grader.v1" {
		t.Fatalf("grade = %+v, want passing versioned grade", grade)
	}

	trace.Events[2].Sequence = 2
	failed := GradeTrace(trace, &TraceExpectation{Profile: "exec-linux", Outcome: "completed", MinScore: 100})
	if failed.Passed || len(failed.Failures) == 0 {
		t.Fatalf("failed grade = %+v, want failure evidence", failed)
	}
}

func TestGradeTraceRejectsExtraAndReorderedSteps(t *testing.T) {
	trace := NewTrace("core", "completed", []store.RuntimeEvent{
		{Sequence: 1, Kind: "turn.accepted"},
		{Sequence: 2, Kind: "tool.completed"},
		{Sequence: 3, Kind: "tool.requested"},
		{Sequence: 4, Kind: "turn.completed"},
	})
	want := []string{"turn.accepted", "tool.requested", "tool.completed", "turn.completed"}
	grade := GradeTrace(trace, &TraceExpectation{Profile: "core", Outcome: "completed", ExpectedKinds: want, MinScore: 100})
	if grade.Passed {
		t.Fatalf("grade = %+v, want reordered trajectory rejected", grade)
	}
	trace.Events = append(trace.Events, store.RuntimeEvent{Sequence: 5, Kind: "unexpected"})
	grade = GradeTrace(trace, &TraceExpectation{Profile: "core", Outcome: "completed", ExpectedKinds: []string{
		"turn.accepted", "tool.completed", "tool.requested", "turn.completed",
	}, MinScore: 100})
	if grade.Passed {
		t.Fatalf("grade = %+v, want extra trajectory step rejected", grade)
	}
}

func TestGradeTraceEnforcesIdentityAndPayloadSignals(t *testing.T) {
	event := func(sequence uint64, kind string, payload string) store.RuntimeEvent {
		return store.RuntimeEvent{
			Sequence: sequence, Kind: kind, SessionID: "session-1", TurnID: "turn-1", InvocationID: "invocation-1", Author: "owner",
			Payload: json.RawMessage(payload),
		}
	}
	trace := NewTrace("core", "completed", []store.RuntimeEvent{
		event(1, "tool.requested", `{"tool_name":"files.read","policy":"policy-1"}`),
		event(2, "approval.required", `{"grant_id":"grant-1"}`),
		event(3, "handoff.completed", `{"target":"researcher"}`),
		event(4, "continuation.started", `{"turn":"turn-2"}`),
		event(5, "turn.completed", `{"outcome":"completed"}`),
	})
	want := &TraceExpectation{
		Profile: "core", Outcome: "completed", MinScore: 100,
		Identity: &TraceIdentity{SessionID: "session-1", TurnID: "turn-1", InvocationID: "invocation-1", Author: "owner"},
		RequiredSignals: []TraceSignal{
			{Kind: "tool.requested", PayloadFields: map[string]string{"tool_name": "files.read", "policy": "policy-1"}},
			{Kind: "approval.required", PayloadFields: map[string]string{"grant_id": "grant-1"}},
			{Kind: "handoff.completed", PayloadFields: map[string]string{"target": "researcher"}},
			{Kind: "continuation.started", PayloadFields: map[string]string{"turn": "turn-2"}},
			{Kind: "turn.completed", PayloadFields: map[string]string{"outcome": "completed"}},
		},
		ForbiddenSignals: []TraceSignal{{Kind: "policy.bypass"}},
	}
	if grade := GradeTrace(trace, want); !grade.Passed {
		t.Fatalf("grade = %+v, want passing identity and signal checks", grade)
	}
	trace.Events[0].Payload = json.RawMessage(`{"tool_name":"shell.exec","policy":"policy-1"}`)
	if grade := GradeTrace(trace, want); grade.Passed {
		t.Fatalf("grade = %+v, want wrong tool choice rejected", grade)
	}
}

func TestGradeProfilesIsDeterministicAndReportsProviderErrors(t *testing.T) {
	grades := GradeProfiles(context.Background(), []string{"native-linux", "core"}, func(_ context.Context, profile string) (Trace, error) {
		if profile == "native-linux" {
			return Trace{}, errors.New("profile unavailable")
		}
		return NewTrace(profile, "completed", []store.RuntimeEvent{{Sequence: 1, Kind: "turn.completed"}}), nil
	}, &TraceExpectation{Outcome: "completed", RequiredKinds: []string{"turn.completed"}})
	if len(grades) != 2 || grades[0].Profile != "core" || grades[1].Profile != "native-linux" {
		t.Fatalf("grades = %+v, want sorted profile results", grades)
	}
	if !grades[0].Grade.Passed || grades[1].Error == "" {
		t.Fatalf("grades = %+v, want pass and provider error", grades)
	}
}
