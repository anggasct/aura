package workflow

import (
	"encoding/json"
	"github.com/anggasct/aura/internal/platform/durable/durabletest"
	"github.com/anggasct/aura/internal/platform/fault"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/platform/principal"
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
				code, ok := fault.CodeOf[ErrorCode](err)
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

func agentExecutor(agentID string) ExecutorSpec {
	return ExecutorSpec{Kind: KindAgent, AgentID: &agentID, Prompt: "Do the work."}
}

func validTestSpec() *Spec {
	return &Spec{
		ID:      "demo",
		Goal:    "Demonstrate",
		Version: 1,
		Source:  SourceDefined,
		Steps: []StepSpec{
			{ID: "implement", Executor: agentExecutor("engineer"), Timeout: 5 * time.Minute},
		},
	}
}

func acceptToolCall(string, json.RawMessage) error { return nil }

func testValidationDeps() *ValidationDeps {
	registry, err := buildTestRegistry()
	if err != nil {
		panic(err)
	}
	return &ValidationDeps{
		KnownTools:     []string{"read_file", "write_file", "exec"},
		EffectfulTools: []string{"exec", "write_file"},
		Agents:         registry,
		CheckToolCall:  acceptToolCall,
		StepAllowed:    acceptStep,
	}
}

func acceptStep(string, principal.Class, string) (allowed, personaKnown bool) { return true, true }

func personaTools(byPersona map[string][]string) func(string, principal.Class, string) (bool, bool) {
	return func(agentID string, _ principal.Class, toolID string) (bool, bool) {
		granted, known := byPersona[agentID]
		return slices.Contains(granted, toolID), known
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
			name:     "agent without agent_id",
			mutate:   func(s *Spec) { s.Steps[0].Executor.AgentID = nil },
			wantCode: ErrorCodeSpecInvalid,
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
			if code, _ := fault.CodeOf[ErrorCode](err); code != tc.wantCode {
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
	if code, _ := fault.CodeOf[ErrorCode](err); code != ErrorCodeDangerousUncovered {
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

func TestValidateWaitCorrelationFields(t *testing.T) {
	waitSpec := func() *Spec {
		event := "check_suite.completed"
		return &Spec{
			ID: "waitbind", Goal: "Wait binding", Version: 1, Source: SourceDefined,
			Steps: []StepSpec{
				{ID: "build", Executor: ExecutorSpec{Kind: KindTool, ToolID: strPtr("read_file")}, Timeout: time.Minute},
				{ID: "hold", DependsOn: []string{"build"}, Executor: ExecutorSpec{Kind: KindWait, Event: &event}, Timeout: time.Minute},
			},
		}
	}
	t.Run("accepts bound wait", func(t *testing.T) {
		spec := waitSpec()
		spec.Steps[1].Executor.ExternalRef = strPtr("build")
		if err := Validate(spec, testValidationDeps()); err != nil {
			t.Fatalf("Validate: %v", err)
		}
	})
	t.Run("accepts custom source", func(t *testing.T) {
		spec := waitSpec()
		spec.Steps[1].Executor.ExternalRef = strPtr("build")
		spec.Steps[1].Executor.Source = strPtr("github")
		if err := Validate(spec, testValidationDeps()); err != nil {
			t.Fatalf("Validate: %v", err)
		}
	})
	cases := []struct {
		name   string
		mutate func(*Spec)
	}{
		{"empty external ref", func(s *Spec) { s.Steps[1].Executor.ExternalRef = strPtr("") }},
		{"unknown external ref", func(s *Spec) { s.Steps[1].Executor.ExternalRef = strPtr("ghost") }},
		{"non-dependency external ref", func(s *Spec) {
			s.Steps = append(s.Steps, StepSpec{ID: "other", Executor: ExecutorSpec{Kind: KindTool, ToolID: strPtr("read_file")}, Timeout: time.Minute})
			s.Steps[1].Executor.ExternalRef = strPtr("other")
		}},
		{"empty source", func(s *Spec) {
			s.Steps[1].Executor.ExternalRef = strPtr("build")
			s.Steps[1].Executor.Source = strPtr("")
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			spec := waitSpec()
			tc.mutate(spec)
			err := Validate(spec, testValidationDeps())
			if err == nil {
				t.Fatal("Validate unexpectedly accepted the spec")
			}
			if code, _ := fault.CodeOf[ErrorCode](err); code != ErrorCodeExecutorInvalid {
				t.Fatalf("code = %s (%v), want %s", code, err, ErrorCodeExecutorInvalid)
			}
		})
	}
}

func TestValidateToolArgumentsMustBeJSONObject(t *testing.T) {
	toolArgsSpec := func() *Spec {
		return &Spec{
			ID: "toolargs", Goal: "Tool args", Version: 1, Source: SourceDefined,
			Steps: []StepSpec{
				{ID: "run", Executor: ExecutorSpec{Kind: KindTool, ToolID: strPtr("read_file")}, Timeout: time.Minute},
			},
		}
	}
	t.Run("accepts object args", func(t *testing.T) {
		spec := toolArgsSpec()
		spec.Steps[0].Executor.ToolArgs = json.RawMessage(`{"path":"notes.txt"}`)
		if err := Validate(spec, testValidationDeps()); err != nil {
			t.Fatalf("Validate: %v", err)
		}
	})
	for _, args := range []string{`[1,2]`, `"text"`, `oops`} {
		t.Run("rejects "+args, func(t *testing.T) {
			spec := toolArgsSpec()
			spec.Steps[0].Executor.ToolArgs = json.RawMessage(args)
			err := Validate(spec, testValidationDeps())
			if err == nil {
				t.Fatal("Validate unexpectedly accepted non-object args")
			}
			if code, _ := fault.CodeOf[ErrorCode](err); code != ErrorCodeExecutorInvalid {
				t.Fatalf("code = %s (%v), want %s", code, err, ErrorCodeExecutorInvalid)
			}
		})
	}
}

func agentStepWith(id string, mutate func(*ExecutorSpec), dependsOn ...string) StepSpec {
	executor := agentExecutor("engineer")
	if mutate != nil {
		mutate(&executor)
	}
	return StepSpec{ID: id, DependsOn: dependsOn, Executor: executor, Timeout: time.Minute}
}

func agentToolsStep(id string, tools []string, dependsOn ...string) StepSpec {
	return agentStepWith(id, func(e *ExecutorSpec) { e.Tools = tools }, dependsOn...)
}

func toolStepOf(id, toolID string, dependsOn ...string) StepSpec {
	return StepSpec{ID: id, DependsOn: dependsOn, Executor: ExecutorSpec{Kind: KindTool, ToolID: &toolID, ToolArgs: json.RawMessage(`{"path":"a"}`)}, Timeout: time.Minute}
}

func workflowStepOf(id string, dependsOn ...string) StepSpec {
	return StepSpec{ID: id, DependsOn: dependsOn, Executor: ExecutorSpec{Kind: KindWorkflow, WorkflowID: ptr("leaf")}, Timeout: time.Minute}
}

func waitStepOf(id string, dependsOn ...string) StepSpec {
	return StepSpec{ID: id, DependsOn: dependsOn, Executor: ExecutorSpec{Kind: KindWait, Event: ptr("done")}, Timeout: time.Minute}
}

func approvalStepOf(id string, dependsOn ...string) StepSpec {
	return StepSpec{ID: id, DependsOn: dependsOn, Executor: ExecutorSpec{Kind: KindApproval}, Timeout: time.Minute}
}

func withCondition(step StepSpec, source string) StepSpec {
	step.Condition = &source
	return step
}

func agentGraphDeps() *ValidationDeps {
	deps := testValidationDeps()
	deps.KnownTools = []string{"read_file", "write_file", "exec", "code_execute", "kb_search", "web_search", "web_fetch", "github_comment"}
	deps.EffectfulTools = nil
	return deps
}

func specOfSteps(steps ...StepSpec) *Spec {
	return &Spec{ID: "demo", Goal: "Demonstrate", Version: 1, Source: SourceDefined, Steps: steps}
}

func TestValidateAgentStep(t *testing.T) {
	foreign := func(mutate func(*ExecutorSpec)) func(*Spec) {
		return func(s *Spec) { mutate(&s.Steps[0].Executor) }
	}
	onOtherKind := func(step StepSpec, mutate func(*ExecutorSpec)) func(*Spec) {
		return func(s *Spec) {
			mutate(&step.Executor)
			s.Steps = append(s.Steps, step)
		}
	}
	agentFields := map[string]func(*ExecutorSpec){
		"agent_id":   func(e *ExecutorSpec) { e.AgentID = ptr("engineer") },
		"prompt":     func(e *ExecutorSpec) { e.Prompt = "x" },
		"tools":      func(e *ExecutorSpec) { e.Tools = []string{"read_file"} },
		"output":     func(e *ExecutorSpec) { e.Output = OutputText },
		"max_tokens": func(e *ExecutorSpec) { e.MaxTokens = intPtr(10) },
	}
	otherKinds := map[string]StepSpec{
		"tool":     toolStepOf("other", "read_file"),
		"wait":     waitStepOf("other"),
		"approval": approvalStepOf("other"),
		"workflow": workflowStepOf("other"),
	}
	cases := []struct {
		name     string
		mutate   func(*Spec)
		wantCode ErrorCode
	}{
		{name: "missing agent_id", mutate: foreign(func(e *ExecutorSpec) { e.AgentID = nil }), wantCode: ErrorCodeSpecInvalid},
		{name: "empty agent_id", mutate: foreign(func(e *ExecutorSpec) { e.AgentID = ptr("") }), wantCode: ErrorCodeSpecInvalid},
		{name: "empty prompt", mutate: foreign(func(e *ExecutorSpec) { e.Prompt = "" }), wantCode: ErrorCodeSpecInvalid},
		{name: "blank prompt", mutate: foreign(func(e *ExecutorSpec) { e.Prompt = " \n " }), wantCode: ErrorCodeSpecInvalid},
		{name: "prompt over 2000 runes", mutate: foreign(func(e *ExecutorSpec) { e.Prompt = strings.Repeat("é", 2001) }), wantCode: ErrorCodeSpecInvalid},
		{name: "prompt with a control character", mutate: foreign(func(e *ExecutorSpec) { e.Prompt = "a\x1b[2Jb" }), wantCode: ErrorCodeSpecInvalid},
		{name: "prompt with a carriage return", mutate: foreign(func(e *ExecutorSpec) { e.Prompt = "a\r\nb" }), wantCode: ErrorCodeSpecInvalid},
		{name: "prompt with a bidirectional override", mutate: foreign(func(e *ExecutorSpec) { e.Prompt = "a" + string(rune(0x202e)) + "b" }), wantCode: ErrorCodeSpecInvalid},
		{name: "max_tokens zero", mutate: foreign(func(e *ExecutorSpec) { e.MaxTokens = intPtr(0) }), wantCode: ErrorCodeSpecInvalid},
		{name: "max_tokens negative", mutate: foreign(func(e *ExecutorSpec) { e.MaxTokens = intPtr(-5) }), wantCode: ErrorCodeSpecInvalid},
		{name: "max_tokens above the limit", mutate: foreign(func(e *ExecutorSpec) { e.MaxTokens = intPtr(MaxAgentMaxTokens + 1) }), wantCode: ErrorCodeSpecInvalid},
		{name: "unknown output", mutate: foreign(func(e *ExecutorSpec) { e.Output = "yaml" }), wantCode: ErrorCodeExecutorInvalid},
		{name: "unknown agent", mutate: foreign(func(e *ExecutorSpec) { e.AgentID = ptr("ghost") }), wantCode: ErrorCodeExecutorInvalid},
		{name: "agent step with tool", mutate: foreign(func(e *ExecutorSpec) { e.ToolID = ptr("read_file") }), wantCode: ErrorCodeExecutorInvalid},
		{name: "agent step with args", mutate: foreign(func(e *ExecutorSpec) { e.ToolArgs = json.RawMessage(`{}`) }), wantCode: ErrorCodeExecutorInvalid},
		{name: "agent step with event", mutate: foreign(func(e *ExecutorSpec) { e.Event = ptr("done") }), wantCode: ErrorCodeExecutorInvalid},
		{name: "agent step with source", mutate: foreign(func(e *ExecutorSpec) { e.Source = ptr("github") }), wantCode: ErrorCodeExecutorInvalid},
		{name: "agent step with external_ref", mutate: foreign(func(e *ExecutorSpec) { e.ExternalRef = ptr("x") }), wantCode: ErrorCodeExecutorInvalid},
		{name: "agent step with workflow", mutate: foreign(func(e *ExecutorSpec) { e.WorkflowID = ptr("leaf") }), wantCode: ErrorCodeExecutorInvalid},
		{
			name: "wait external_ref naming an agent step",
			mutate: func(s *Spec) {
				ref := "implement"
				s.Steps = append(s.Steps, StepSpec{ID: "hold", DependsOn: []string{"implement"}, Executor: ExecutorSpec{Kind: KindWait, Event: ptr("ci"), ExternalRef: &ref}, Timeout: time.Minute})
			},
			wantCode: ErrorCodeExecutorInvalid,
		},
		{name: "wait external_ref naming a tool step", mutate: func(s *Spec) {
			ref := "read"
			s.Steps = append(s.Steps, toolStepOf("read", "read_file"), StepSpec{ID: "hold", DependsOn: []string{"read"}, Executor: ExecutorSpec{Kind: KindWait, Event: ptr("ci"), ExternalRef: &ref}, Timeout: time.Minute})
		}},
		{name: "default max_tokens and output", mutate: func(*Spec) {}},
		{name: "empty tools", mutate: foreign(func(e *ExecutorSpec) { e.Tools = []string{} })},
		{name: "workspace tools alone", mutate: foreign(func(e *ExecutorSpec) { e.Tools = []string{"read_file", "list_dir"} })},
		{name: "web tools alone", mutate: foreign(func(e *ExecutorSpec) { e.Tools = []string{"web_search", "web_fetch"} })},
		{name: "json output with the largest budget", mutate: foreign(func(e *ExecutorSpec) { e.Output = OutputJSON; e.MaxTokens = intPtr(MaxAgentMaxTokens) })},
		{name: "prompt of 2000 runes with newlines", mutate: foreign(func(e *ExecutorSpec) { e.Prompt = strings.Repeat("é\n", 1000) })},
	}
	for _, forbidden := range []string{"write_file", "exec", "code_execute", "email_send", "handoff_to_owner", "github_create_pr", "mcp_files_read", "plugin_weather_lookup", "workflow_start", "kb_search"} {
		cases = append(cases, struct {
			name     string
			mutate   func(*Spec)
			wantCode ErrorCode
		}{name: "forbidden tool " + forbidden, mutate: foreign(func(e *ExecutorSpec) { e.Tools = []string{forbidden} }), wantCode: ErrorCodeExecutorInvalid})
	}
	for kind, step := range otherKinds {
		for field, set := range agentFields {
			cases = append(cases, struct {
				name     string
				mutate   func(*Spec)
				wantCode ErrorCode
			}{name: kind + " step with " + field, mutate: onOtherKind(step, set), wantCode: ErrorCodeExecutorInvalid})
		}
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			spec := validTestSpec()
			tc.mutate(spec)
			err := Validate(spec, agentGraphDeps())
			if tc.wantCode == "" {
				if err != nil {
					t.Fatalf("Validate = %v, want nil", err)
				}
				return
			}
			if !hasCode(err, tc.wantCode) {
				t.Fatalf("Validate = %v, want %s", err, tc.wantCode)
			}
		})
	}
	t.Run("defaults are written into the validated step", func(t *testing.T) {
		spec := validTestSpec()
		if err := Validate(spec, agentGraphDeps()); err != nil {
			t.Fatal(err)
		}
		executor := &spec.Steps[0].Executor
		if executor.MaxTokens == nil || *executor.MaxTokens != DefaultAgentMaxTokens || executor.Output != OutputText {
			t.Fatalf("executor = %+v, want the default budget and text output written", executor)
		}
	})
}

func TestParseSpecRefusesRequires(t *testing.T) {
	_, err := parseSpec([]byte("id: demo\nversion: 1\ngoal: g\nsteps:\n  - id: a\n    timeout: 1m\n    executor:\n      kind: agent\n      requires: [repository.read]\n      prompt: x\n"))
	if !hasCode(err, ErrorCodeSpecInvalid) {
		t.Fatalf("parseSpec = %v, want %s", err, ErrorCodeSpecInvalid)
	}
	var spec Spec
	decoder := json.NewDecoder(strings.NewReader(`{"id":"demo","goal":"g","version":1,"source":"defined","steps":[{"id":"a","timeout":60000000000,"executor":{"kind":"agent","agent_id":"engineer","requires":["repository.read"],"prompt":"x"}}]}`))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&spec); err == nil {
		t.Fatal("a proposal spec with requires decoded")
	}
}

func TestParseSpecLoadsAgentFields(t *testing.T) {
	spec, err := parseSpec([]byte(`id: demo
version: 1
goal: Research
steps:
  - id: research
    timeout: 1m
    executor:
      kind: agent
      agent_id: researcher
      prompt: |
        Find the release.
        Be brief.
      tools: [web_search, web_fetch]
      output: json
      max_tokens: 30000
`))
	if err != nil {
		t.Fatal(err)
	}
	executor := spec.Steps[0].Executor
	if deref(executor.AgentID) != "researcher" || executor.Prompt != "Find the release.\nBe brief.\n" || !slices.Equal(executor.Tools, []string{"web_search", "web_fetch"}) || executor.Output != OutputJSON || executor.MaxTokens == nil || *executor.MaxTokens != 30000 {
		t.Fatalf("executor = %+v, want every agent field loaded", executor)
	}
	if err := Validate(spec, agentGraphDeps()); err != nil {
		t.Fatalf("Validate = %v", err)
	}
}

func TestValidateAgentStepFamilies(t *testing.T) {
	workspace := []string{"read_file"}
	web := []string{"web_fetch"}
	cases := []struct {
		name     string
		steps    []StepSpec
		wantCode ErrorCode
	}{
		{name: "read_file with web_fetch", steps: []StepSpec{agentToolsStep("a", []string{"read_file", "web_fetch"})}, wantCode: ErrorCodeExecutorInvalid},
		{name: "list_dir with web_search", steps: []StepSpec{agentToolsStep("a", []string{"list_dir", "web_search"})}, wantCode: ErrorCodeExecutorInvalid},
		{name: "web step after a read_file tool step", steps: []StepSpec{toolStepOf("r", "read_file"), agentToolsStep("w", web, "r")}, wantCode: ErrorCodeExecutorInvalid},
		{name: "web step after a kb_search tool step", steps: []StepSpec{toolStepOf("r", "kb_search"), agentToolsStep("w", web, "r")}, wantCode: ErrorCodeExecutorInvalid},
		{name: "web step after an exec tool step", steps: []StepSpec{toolStepOf("r", "exec"), agentToolsStep("w", web, "r")}, wantCode: ErrorCodeExecutorInvalid},
		{name: "web step after a code_execute tool step", steps: []StepSpec{toolStepOf("r", "code_execute"), agentToolsStep("w", web, "r")}, wantCode: ErrorCodeExecutorInvalid},
		{name: "web step after a workspace agent step", steps: []StepSpec{agentToolsStep("r", workspace), agentToolsStep("w", web, "r")}, wantCode: ErrorCodeExecutorInvalid},
		{name: "web step after a tool-free agent step that read a workspace agent step", steps: []StepSpec{agentToolsStep("r", workspace), agentToolsStep("mid", nil, "r"), agentToolsStep("w", web, "mid")}, wantCode: ErrorCodeExecutorInvalid},
		{name: "web step whose condition reads the output of a private step", steps: []StepSpec{toolStepOf("r", "read_file"), withCondition(agentToolsStep("w", web), `steps.r.output.ok == true`)}, wantCode: ErrorCodeExecutorInvalid},
		{name: "web step whose condition reads the status of a private step", steps: []StepSpec{toolStepOf("r", "read_file"), withCondition(agentToolsStep("w", web), `steps.r.status == "succeeded"`)}, wantCode: ErrorCodeExecutorInvalid},
		{
			name: "web step whose condition reads the status of a private-influenced step",
			steps: []StepSpec{
				toolStepOf("r", "read_file"),
				withCondition(agentToolsStep("q", nil), `steps.r.output.ok == true`),
				withCondition(agentToolsStep("w", web), `steps.q.status == "succeeded"`),
			},
			wantCode: ErrorCodeExecutorInvalid,
		},
		{
			name: "web step whose condition reads the output of a private-influenced step",
			steps: []StepSpec{
				toolStepOf("r", "read_file"),
				withCondition(agentToolsStep("q", nil), `steps.r.output.ok == true`),
				withCondition(agentToolsStep("w", web), `steps.q.output.ok == true`),
			},
			wantCode: ErrorCodeExecutorInvalid,
		},
		{
			name: "web step depending on a step whose condition reads a private output",
			steps: []StepSpec{
				toolStepOf("r", "read_file"),
				withCondition(approvalStepOf("q"), `steps.r.output.ok == true`),
				agentToolsStep("w", web, "q"),
			},
			wantCode: ErrorCodeExecutorInvalid,
		},
		{
			name: "web step depending on a step that depends on a private-influenced step",
			steps: []StepSpec{
				toolStepOf("r", "read_file"),
				withCondition(approvalStepOf("q"), `steps.r.output.ok == true`),
				waitStepOf("hold", "q"),
				agentToolsStep("w", web, "hold"),
			},
			wantCode: ErrorCodeExecutorInvalid,
		},
		{name: "web step after a web step", steps: []StepSpec{agentToolsStep("a", web), agentToolsStep("w", web, "a")}},
		{name: "web step after a web_search tool step", steps: []StepSpec{toolStepOf("s", "web_search"), agentToolsStep("w", web, "s")}},
		{name: "web step after a web_fetch tool step", steps: []StepSpec{toolStepOf("s", "web_fetch"), agentToolsStep("w", web, "s")}},
		{name: "web step after a workflow step", steps: []StepSpec{workflowStepOf("child"), agentToolsStep("w", web, "child")}},
		{name: "web step after a wait step", steps: []StepSpec{waitStepOf("hold"), agentToolsStep("w", web, "hold")}},
		{name: "web step after an approval step", steps: []StepSpec{approvalStepOf("gate"), agentToolsStep("w", web, "gate")}},
		{name: "web step after an approval step that follows a private step", steps: []StepSpec{toolStepOf("r", "read_file"), approvalStepOf("gate", "r"), agentToolsStep("w", web, "gate")}},
		{name: "workspace step after a web step", steps: []StepSpec{agentToolsStep("a", web), agentToolsStep("r", workspace, "a")}},
		{name: "tool-free step after a workspace step", steps: []StepSpec{agentToolsStep("r", workspace), agentToolsStep("mid", nil, "r")}},
		{
			name: "web step beside a private step it does not read",
			steps: []StepSpec{
				toolStepOf("r", "read_file"),
				withCondition(approvalStepOf("q"), `steps.r.output.ok == true`),
				agentToolsStep("w", web),
				withCondition(agentToolsStep("w2", web, "w"), `steps.w.status == "succeeded"`),
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := Validate(specOfSteps(tc.steps...), agentGraphDeps())
			if tc.wantCode == "" {
				if err != nil {
					t.Fatalf("Validate = %v, want nil", err)
				}
				return
			}
			if !hasCode(err, tc.wantCode) {
				t.Fatalf("Validate = %v, want %s", err, tc.wantCode)
			}
		})
	}
}

func intPtr(value int) *int { return &value }

func rawAgentDefaultsSpec() *Spec {
	return &Spec{ID: "demo", Goal: "Demonstrate", Version: 1, Source: SourceDefined, Steps: []StepSpec{
		{ID: "think", Executor: agentExecutor("engineer"), Timeout: time.Minute},
		{ID: "again", DependsOn: []string{"think"}, Executor: agentExecutor("engineer"), Timeout: time.Minute},
	}}
}

func TestAgentStepDefaultsDigestIdenticallyOnEveryPath(t *testing.T) {
	digestOf := func(t *testing.T, spec *Spec) string {
		t.Helper()
		digest, err := Digest(spec)
		if err != nil {
			t.Fatal(err)
		}
		return digest
	}
	want := digestOf(t, rawAgentDefaultsSpec())
	t.Run("a raw spec and its validated copy", func(t *testing.T) {
		validated := rawAgentDefaultsSpec()
		if err := Validate(validated, agentGraphDeps()); err != nil {
			t.Fatal(err)
		}
		if got := digestOf(t, validated); got != want {
			t.Fatalf("validated digest = %s, want %s", got, want)
		}
	})
	t.Run("a spec that writes the defaults out", func(t *testing.T) {
		explicit := rawAgentDefaultsSpec()
		for index := range explicit.Steps {
			explicit.Steps[index].Executor.Output = OutputText
			explicit.Steps[index].Executor.MaxTokens = intPtr(DefaultAgentMaxTokens)
		}
		if got := digestOf(t, explicit); got != want {
			t.Fatalf("explicit digest = %s, want %s", got, want)
		}
	})
	t.Run("a different budget changes the digest", func(t *testing.T) {
		other := rawAgentDefaultsSpec()
		other.Steps[0].Executor.MaxTokens = intPtr(DefaultAgentMaxTokens + 1)
		if got := digestOf(t, other); got == want {
			t.Fatal("a different max_tokens left the digest unchanged")
		}
	})
	t.Run("the effective budget is in the stored definition", func(t *testing.T) {
		encoded, err := specJSON(rawAgentDefaultsSpec())
		if err != nil || !strings.Contains(string(encoded), `"max_tokens":50000`) || !strings.Contains(string(encoded), `"output":"text"`) {
			t.Fatalf("specJSON = %s, %v, want the effective budget and output", encoded, err)
		}
	})
	t.Run("loading the same definition twice", func(t *testing.T) {
		interpreter := freshInterpreter(t)
		for range 2 {
			if err := interpreter.Install(t.Context(), []*Spec{rawAgentDefaultsSpec()}, agentGraphDeps()); err != nil {
				t.Fatalf("Install = %v", err)
			}
		}
	})
	t.Run("saving a raw definition before and after a load", func(t *testing.T) {
		for _, saveFirst := range []bool{true, false} {
			disk := newTestStore(t)
			interpreter := mustNewInterpreter(t, disk, durabletest.NewFake(), &Options{})
			if saveFirst {
				if err := disk.SaveDefinition(t.Context(), rawAgentDefaultsSpec()); err != nil {
					t.Fatalf("SaveDefinition = %v", err)
				}
			}
			if err := interpreter.Install(t.Context(), []*Spec{rawAgentDefaultsSpec()}, agentGraphDeps()); err != nil {
				t.Fatalf("Install after SaveDefinition=%t = %v", saveFirst, err)
			}
			if !saveFirst {
				if err := disk.SaveDefinition(t.Context(), rawAgentDefaultsSpec()); err != nil {
					t.Fatalf("SaveDefinition after Install = %v", err)
				}
			}
		}
	})
	t.Run("a proposal before and after compilation", func(t *testing.T) {
		interpreter := freshInterpreter(t)
		if err := interpreter.LoadAll(t.Context(), nil, agentGraphDeps()); err != nil {
			t.Fatal(err)
		}
		proposal := func() *Spec { return NormalizeProposal(rawAgentDefaultsSpec(), "prop_defaults") }
		before := digestOf(t, proposal())
		compiled := proposal()
		if _, err := interpreter.CompileProposal(compiled, "main"); err != nil {
			t.Fatalf("CompileProposal = %v", err)
		}
		if after := digestOf(t, compiled); after != before {
			t.Fatalf("digest after compile = %s, want %s", after, before)
		}
	})
}
