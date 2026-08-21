package eval

import (
	"context"
	"slices"

	"github.com/anggasct/aura/internal/store"
)

type Trace struct {
	Profile string
	Outcome string
	Events  []store.RuntimeEvent
}

type TraceExpectation struct {
	Profile        string
	Outcome        string
	ExpectedKinds  []string
	RequiredKinds  []string
	ForbiddenKinds []string
	MinScore       int
}

type TraceGrade struct {
	Version  string
	Score    int
	Passed   bool
	Failures []string
}

type ProfileGrade struct {
	Profile string
	Grade   TraceGrade
	Error   string
}

func NewTrace(profile, outcome string, events []store.RuntimeEvent) Trace {
	return Trace{Profile: profile, Outcome: outcome, Events: slices.Clone(events)}
}

func GradeTrace(trace Trace, expectation *TraceExpectation) TraceGrade {
	grade := TraceGrade{Version: "trace-grader.v1", Score: 100}
	if expectation == nil {
		grade.Passed = false
		grade.Failures = []string{"trace expectation is missing"}
		return grade
	}
	want := *expectation
	if want.MinScore <= 0 {
		want.MinScore = 100
	}
	if trace.Profile != want.Profile {
		grade.Score -= 25
		grade.Failures = append(grade.Failures, "profile mismatch")
	}
	if trace.Outcome != want.Outcome {
		grade.Score -= 25
		grade.Failures = append(grade.Failures, "outcome mismatch")
	}
	if !monotonic(trace.Events) {
		grade.Score -= 25
		grade.Failures = append(grade.Failures, "event sequence is not monotonic")
	}
	if want.ExpectedKinds != nil && !slices.Equal(eventKinds(trace.Events), want.ExpectedKinds) {
		grade.Score -= 25
		grade.Failures = append(grade.Failures, "event trajectory does not match expected order")
	}
	for _, kind := range want.RequiredKinds {
		if !containsKind(trace.Events, kind) {
			grade.Score -= 10
			grade.Failures = append(grade.Failures, "missing required event: "+kind)
		}
	}
	for _, kind := range want.ForbiddenKinds {
		if containsKind(trace.Events, kind) {
			grade.Score -= 25
			grade.Failures = append(grade.Failures, "forbidden event: "+kind)
		}
	}
	if grade.Score < 0 {
		grade.Score = 0
	}
	grade.Passed = len(grade.Failures) == 0 && grade.Score >= want.MinScore
	return grade
}

func GradeProfiles(ctx context.Context, profiles []string, run func(context.Context, string) (Trace, error), expectation *TraceExpectation) []ProfileGrade {
	ordered := slices.Clone(profiles)
	slices.Sort(ordered)
	grades := make([]ProfileGrade, 0, len(ordered))
	for _, profile := range ordered {
		trace, err := run(ctx, profile)
		if err != nil {
			grades = append(grades, ProfileGrade{Profile: profile, Error: err.Error()})
			continue
		}
		if expectation == nil {
			grades = append(grades, ProfileGrade{Profile: profile, Error: "trace expectation is missing"})
			continue
		}
		profileExpectation := *expectation
		profileExpectation.Profile = profile
		grades = append(grades, ProfileGrade{Profile: profile, Grade: GradeTrace(trace, &profileExpectation)})
	}
	return grades
}

func monotonic(events []store.RuntimeEvent) bool {
	for i := 1; i < len(events); i++ {
		if events[i].Sequence <= events[i-1].Sequence {
			return false
		}
	}
	return true
}

func containsKind(events []store.RuntimeEvent, kind string) bool {
	for i := range events {
		if events[i].Kind == kind {
			return true
		}
	}
	return false
}

func eventKinds(events []store.RuntimeEvent) []string {
	kinds := make([]string, len(events))
	for i := range events {
		kinds[i] = events[i].Kind
	}
	return kinds
}
