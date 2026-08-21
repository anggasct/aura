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
