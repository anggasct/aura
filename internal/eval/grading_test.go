package eval

import (
	"context"
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
