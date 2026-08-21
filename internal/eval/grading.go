package eval

import (
	"context"
	"encoding/json"
	"slices"

	"github.com/anggasct/aura/internal/store"
)

type Trace struct {
	Profile string
	Outcome string
	Events  []store.RuntimeEvent
}

type TraceExpectation struct {
	Profile          string
	Outcome          string
	ExpectedKinds    []string
	RequiredKinds    []string
	ForbiddenKinds   []string
	Identity         *TraceIdentity
	RequiredSignals  []TraceSignal
	ForbiddenSignals []TraceSignal
	MinScore         int
}

type TraceIdentity struct {
	SessionID    string
	TurnID       string
	InvocationID string
	Author       string
}

type TraceSignal struct {
	Kind          string
	PayloadFields map[string]string
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
	if want.Identity != nil && !matchesIdentity(trace.Events, *want.Identity) {
		grade.Score -= 25
		grade.Failures = append(grade.Failures, "event identity does not match expectation")
	}
	for _, signal := range want.RequiredSignals {
		if !containsSignal(trace.Events, signal) {
			grade.Score -= 10
			grade.Failures = append(grade.Failures, "missing required trace signal: "+signal.Kind)
		}
	}
	for _, signal := range want.ForbiddenSignals {
		if containsSignal(trace.Events, signal) {
			grade.Score -= 25
			grade.Failures = append(grade.Failures, "forbidden trace signal: "+signal.Kind)
		}
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

func matchesIdentity(events []store.RuntimeEvent, identity TraceIdentity) bool {
	for i := range events {
		event := &events[i]
		if identity.SessionID != "" && event.SessionID != identity.SessionID {
			return false
		}
		if identity.TurnID != "" && event.TurnID != identity.TurnID {
			return false
		}
		if identity.InvocationID != "" && event.InvocationID != identity.InvocationID {
			return false
		}
		if identity.Author != "" && event.Author != identity.Author {
			return false
		}
	}
	return len(events) > 0
}

func containsSignal(events []store.RuntimeEvent, signal TraceSignal) bool {
	for i := range events {
		if signalMatches(&events[i], signal) {
			return true
		}
	}
	return false
}

func signalMatches(event *store.RuntimeEvent, signal TraceSignal) bool {
	if event.Kind != signal.Kind {
		return false
	}
	if len(signal.PayloadFields) == 0 {
		return true
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(event.Payload, &fields); err != nil {
		return false
	}
	for name, want := range signal.PayloadFields {
		raw, ok := fields[name]
		if !ok {
			return false
		}
		var value string
		if err := json.Unmarshal(raw, &value); err != nil {
			value = string(raw)
		}
		if value != want {
			return false
		}
	}
	return true
}
