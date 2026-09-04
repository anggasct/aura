package workflow

import (
	"encoding/json"
	"slices"
	"testing"
	"time"
)

func TestParseConditionGrammar(t *testing.T) {
	cases := []struct {
		name     string
		source   string
		wantErr  bool
		wantRefs []string
	}{
		{name: "status equality", source: `steps.approval.status == "succeeded"`, wantRefs: []string{"approval"}},
		{name: "output path comparison", source: `steps.wait_ci.output.exit_code != 1`, wantRefs: []string{"wait_ci"}},
		{name: "conjunction", source: `steps.a.status == "succeeded" && steps.b.output.ok == true`, wantRefs: []string{"a", "b"}},
		{name: "boolean literal", source: `steps.a.output.enabled == false`, wantRefs: []string{"a"}},
		{name: "null literal", source: `steps.a.output.maybe == null`, wantRefs: []string{"a"}},
		{name: "array index path", source: `steps.a.output.items[0].name == "x"`, wantRefs: []string{"a"}},
		{name: "escaped quote literal", source: `steps.a.output.q == "say \"hi\""`, wantRefs: []string{"a"}},
		{name: "or is outside the grammar", source: `steps.a.status == "x" || steps.b.status == "y"`, wantErr: true},
		{name: "lone exclamation", source: `steps.a.status != "x"`, wantRefs: []string{"a"}},
		{name: "unknown field", source: `steps.a.duration == 5`, wantErr: true},
		{name: "bare identifier", source: `enabled == true`, wantErr: true},
		{name: "trailing conjunction", source: `steps.a.status == "x" &&`, wantErr: true},
		{name: "unterminated string", source: `steps.a.status == "x`, wantErr: true},
		{name: "empty", source: ``, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			parsed, err := parseCondition(tc.source)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("parseCondition(%q) unexpectedly succeeded", tc.source)
				}
				code, ok := CodeOf(err)
				if !ok || code != ErrorCodeConditionInvalid {
					t.Fatalf("code = %s, want workflow_condition_invalid", code)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseCondition(%q): %v", tc.source, err)
			}
			if tc.wantRefs != nil {
				got := parsed.referencedSteps()
				for _, want := range tc.wantRefs {
					if !slices.Contains(got, want) {
						t.Errorf("references = %v, want %q", got, want)
					}
				}
			}
		})
	}
}

func TestConditionComparesLargeIntegerOutputsNumerically(t *testing.T) {
	for _, tc := range []struct {
		name  string
		value string
	}{
		{name: "one million", value: "1000000"},
		{name: "nine digits", value: "123456789"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			execution := &stepExecution{
				outputs:  map[string]json.RawMessage{"a": json.RawMessage(`{"n":` + tc.value + `}`)},
				statuses: map[string]string{},
			}
			equal, err := parseCondition("steps.a.output.n == " + tc.value)
			if err != nil {
				t.Fatalf("parseCondition: %v", err)
			}
			if !execution.evaluate(equal) {
				t.Fatalf("steps.a.output.n == %s evaluated false for output %s", tc.value, tc.value)
			}
			notEqual, err := parseCondition("steps.a.output.n != " + tc.value)
			if err != nil {
				t.Fatalf("parseCondition: %v", err)
			}
			if execution.evaluate(notEqual) {
				t.Fatalf("steps.a.output.n != %s evaluated true for output %s", tc.value, tc.value)
			}
		})
	}
}

func validTestSpec() *Spec {
	agentID := "engineer"
	return &Spec{
		ID:      "demo",
		Goal:    "Demonstrate",
		Version: 1,
		Source:  SourceDefined,
		Steps: []StepSpec{
			{ID: "implement", Executor: ExecutorSpec{Kind: KindAgent, AgentID: &agentID}, Timeout: 5 * time.Minute},
		},
	}
}

func testValidationDeps() ValidationDeps {
	registry, err := buildTestRegistry()
	if err != nil {
		panic(err)
	}
	return ValidationDeps{
		KnownTools:     []string{"read_file", "write_file", "exec"},
		EffectfulTools: []string{"exec", "write_file"},
		Agents:         registry,
	}
}

func TestValidateRejectsInvalidClasses(t *testing.T) {
	cases := []struct {
		name     string
		mutate   func(*Spec)
		wantCode ErrorCode
	}{
		{
			name:     "empty id",
			mutate:   func(s *Spec) { s.ID = "" },
			wantCode: ErrorCodeSpecInvalid,
		},
		{
			name:     "empty goal",
			mutate:   func(s *Spec) { s.Goal = " " },
			wantCode: ErrorCodeSpecInvalid,
		},
		{
			name:     "zero version",
			mutate:   func(s *Spec) { s.Version = 0 },
			wantCode: ErrorCodeSpecInvalid,
		},
		{
			name:     "no steps",
			mutate:   func(s *Spec) { s.Steps = nil },
			wantCode: ErrorCodeSpecInvalid,
		},
		{
			name: "duplicate step id",
			mutate: func(s *Spec) {
				s.Steps = append(s.Steps, s.Steps[0])
			},
			wantCode: ErrorCodeDuplicateStep,
		},
		{
			name:     "empty step id",
			mutate:   func(s *Spec) { s.Steps[0].ID = "" },
			wantCode: ErrorCodeSpecInvalid,
		},
		{
			name:     "self dependency",
			mutate:   func(s *Spec) { s.Steps[0].DependsOn = []string{"implement"} },
			wantCode: ErrorCodeUnknownDependency,
		},
		{
			name:     "unknown dependency",
			mutate:   func(s *Spec) { s.Steps[0].DependsOn = []string{"ghost"} },
			wantCode: ErrorCodeUnknownDependency,
		},
		{
			name: "cycle",
			mutate: func(s *Spec) {
				second := StepSpec{ID: "test", DependsOn: []string{"implement"}, Executor: ExecutorSpec{Kind: KindApproval}, Timeout: time.Minute}
				s.Steps = append(s.Steps, second)
				s.Steps[0].DependsOn = []string{"test"}
			},
			wantCode: ErrorCodeCycleDetected,
		},
		{
			name:     "invalid executor kind",
			mutate:   func(s *Spec) { s.Steps[0].Executor.Kind = Kind("shell") },
			wantCode: ErrorCodeExecutorInvalid,
		},
		{
			name:     "agent without id or requirements",
			mutate:   func(s *Spec) { s.Steps[0].Executor.AgentID = nil },
			wantCode: ErrorCodeExecutorInvalid,
		},
		{
			name: "unknown agent id",
			mutate: func(s *Spec) {
				ghost := "ghost"
				s.Steps[0].Executor.AgentID = &ghost
			},
			wantCode: ErrorCodeExecutorInvalid,
		},
		{
			name: "unknown tool",
			mutate: func(s *Spec) {
				tool := "deploy_prod"
				s.Steps[0].Executor = ExecutorSpec{Kind: KindTool, ToolID: &tool}
			},
			wantCode: ErrorCodeExecutorInvalid,
		},
		{
			name: "wait without event",
			mutate: func(s *Spec) {
				s.Steps[0].Executor = ExecutorSpec{Kind: KindWait}
			},
			wantCode: ErrorCodeExecutorInvalid,
		},
		{
			name: "approval with extras",
			mutate: func(s *Spec) {
				event := "x"
				s.Steps[0].Executor = ExecutorSpec{Kind: KindApproval, Event: &event}
			},
			wantCode: ErrorCodeExecutorInvalid,
		},
		{
			name: "unresolvable agent requirements",
			mutate: func(s *Spec) {
				s.Steps[0].Executor.AgentID = nil
				s.Steps[0].Executor.RequiredCapabilities = []string{"kernel.root"}
			},
			wantCode: ErrorCodeExecutorInvalid,
		},
		{
			name: "condition references unknown step",
			mutate: func(s *Spec) {
				condition := `steps.ghost.status == "succeeded"`
				s.Steps[0].Condition = &condition
			},
			wantCode: ErrorCodeConditionInvalid,
		},
		{
			name: "condition outside grammar",
			mutate: func(s *Spec) {
				condition := `steps.implement.status != "succeeded" || true`
				s.Steps[0].Condition = &condition
			},
			wantCode: ErrorCodeConditionInvalid,
		},
		{
			name:     "missing timeout",
			mutate:   func(s *Spec) { s.Steps[0].Timeout = 0 },
			wantCode: ErrorCodeSpecInvalid,
		},
		{
			name:     "retry attempts out of range",
			mutate:   func(s *Spec) { s.Steps[0].Retry.Attempts = 9 },
			wantCode: ErrorCodeSpecInvalid,
		},
		{
			name: "retry backoff unbounded",
			mutate: func(s *Spec) {
				s.Steps[0].Retry.Backoff = 11 * time.Minute
			},
			wantCode: ErrorCodeSpecInvalid,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			spec := validTestSpec()
			tc.mutate(spec)
			err := Validate(spec, testValidationDeps())
			if err == nil {
				t.Fatal("Validate unexpectedly accepted the spec")
			}
			if code, _ := CodeOf(err); code != tc.wantCode {
				t.Fatalf("code = %s (%v), want %s", code, err, tc.wantCode)
			}
		})
	}
}

func TestValidateAppliesConfiguredDefaultStepTimeout(t *testing.T) {
	spec := validTestSpec()
	spec.Steps[0].Timeout = 0
	deps := testValidationDeps()
	if err := Validate(spec, deps); err == nil {
		t.Fatal("omitted timeout without a configured default was accepted")
	}
	deps.DefaultStepTimeout = 15 * time.Minute
	if err := Validate(spec, deps); err != nil {
		t.Fatalf("omitted timeout with configured default rejected: %v", err)
	}
	if spec.Steps[0].Timeout != 15*time.Minute {
		t.Fatalf("timeout = %s, want the configured default", spec.Steps[0].Timeout)
	}
}

func TestValidateRequiresApprovalCoverageForEffectfulTools(t *testing.T) {
	spec := validTestSpec()
	tool := "exec"
	spec.Steps = []StepSpec{
		{ID: "deploy", Executor: ExecutorSpec{Kind: KindTool, ToolID: &tool}, Timeout: time.Minute},
	}
	err := Validate(spec, testValidationDeps())
	if err == nil {
		t.Fatal("uncovered effectful tool was accepted")
	}
	if code, _ := CodeOf(err); code != ErrorCodeDangerousUncovered {
		t.Fatalf("code = %s, want workflow_dangerous_operation_uncovered", code)
	}

	spec.Steps = []StepSpec{
		{ID: "approve", Executor: ExecutorSpec{Kind: KindApproval}, Timeout: time.Minute},
		{ID: "deploy", DependsOn: []string{"approve"}, Executor: ExecutorSpec{Kind: KindTool, ToolID: &tool}, Timeout: time.Minute},
	}
	if err := Validate(spec, testValidationDeps()); err != nil {
		t.Fatalf("covered effectful tool rejected: %v", err)
	}
}

func TestCompileProducesDeterministicTopologicalOrder(t *testing.T) {
	spec := &Spec{
		ID: "order", Goal: "g", Version: 1, Source: SourceDefined,
		Steps: []StepSpec{
			{ID: "c", DependsOn: []string{"b"}, Executor: ExecutorSpec{Kind: KindApproval}, Timeout: time.Minute},
			{ID: "b", DependsOn: []string{"a"}, Executor: ExecutorSpec{Kind: KindApproval}, Timeout: time.Minute},
			{ID: "a", Executor: ExecutorSpec{Kind: KindApproval}, Timeout: time.Minute},
		},
	}
	graph, err := Compile(spec, testValidationDeps())
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	want := []string{"a", "b", "c"}
	for i := range want {
		if graph.Order[i] != want[i] {
			t.Fatalf("order = %v, want %v", graph.Order, want)
		}
	}
}
