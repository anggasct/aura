package workflow

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/platform/principal"
)

func sinkDeps() *ValidationDeps {
	return &ValidationDeps{SinkTools: []string{"web_fetch", "web_search"}, EffectfulTools: []string{"write_file", "email_send"}}
}

func sinkWebAgent(id string, tools ...string) StepSpec {
	return agentStep(id, agentExec("Look it up.", "text", tools...))
}

func after(step StepSpec, ids ...string) StepSpec {
	step.DependsOn = ids
	return step
}

func sinkPlainAgent(id string, dependsOn ...string) StepSpec {
	return agentStep(id, agentExec("Write it up.", "text"), dependsOn...)
}

func sinkToolStep(id, tool string, dependsOn ...string) StepSpec {
	return StepSpec{ID: id, Executor: ExecutorSpec{Kind: KindTool, ToolID: &tool, ToolArgs: json.RawMessage(`{}`)}, DependsOn: dependsOn, Timeout: time.Minute}
}

func sinkKindStep(id string, kind Kind, dependsOn ...string) StepSpec {
	executor := ExecutorSpec{Kind: kind}
	switch kind {
	case KindWait:
		executor.Event = ptr("done")
	case KindWorkflow:
		executor.WorkflowID = ptr("child")
	case KindAgent, KindTool, KindApproval:
	}
	return StepSpec{ID: id, Executor: executor, DependsOn: dependsOn, Timeout: time.Minute}
}

func TestScheduledWorkflowRefusesASinkAfterAWebStep(t *testing.T) {
	onResearch := "steps.research.status == \"succeeded\""
	conditional := sinkToolStep("notify", "write_file")
	conditional.Condition = &onResearch
	cases := map[string][]StepSpec{
		"an effectful tool step":              {sinkWebAgent("research", "web_search"), sinkKindStep("gate", KindApproval, "research"), sinkToolStep("send", "email_send", "gate")},
		"an external sink tool step":          {sinkWebAgent("research", "web_search"), sinkToolStep("fetch", "web_fetch", "research")},
		"a second web agent step":             {sinkWebAgent("research", "web_search"), after(sinkWebAgent("again", "web_fetch"), "research")},
		"a child workflow":                    {sinkWebAgent("research", "web_fetch"), sinkKindStep("child", KindWorkflow, "research")},
		"a step that only reads the status":   {sinkWebAgent("research", "web_search"), sinkPlainAgent("quiet"), conditional},
		"a sink behind a clean step":          {sinkWebAgent("research", "web_search"), sinkPlainAgent("summarize", "research"), sinkToolStep("send", "write_file", "summarize")},
		"a web tool step then a sink":         {sinkToolStep("search", "web_search"), sinkToolStep("send", "write_file", "search")},
		"a sink behind two levels of waiting": {sinkWebAgent("research", "web_fetch"), sinkKindStep("pause", KindWait, "research"), sinkKindStep("gate", KindApproval, "pause"), sinkToolStep("send", "write_file", "gate")},
	}
	for name, steps := range cases {
		t.Run(name, func(t *testing.T) {
			err := CheckUnattendedSinks(agentSpec("job", steps...), sinkDeps(), nil)
			if !hasCode(err, ErrorCodeUnattendedSink) {
				t.Errorf("CheckUnattendedSinks = %v, want %s", err, ErrorCodeUnattendedSink)
			}
		})
	}
}

func catalogOf(specs ...*Spec) ChildLookup {
	byID := map[string]*Spec{}
	for _, spec := range specs {
		byID[spec.ID] = spec
	}
	return func(id string) (*Spec, error) {
		if spec, ok := byID[id]; ok {
			return spec, nil
		}
		return nil, codedError(ErrorCodeDefinitionNotFound, "definition "+id+" is not loaded")
	}
}

func childStep(id, child string, dependsOn ...string) StepSpec {
	step := sinkKindStep(id, KindWorkflow, dependsOn...)
	step.Executor.WorkflowID = ptr(child)
	return step
}

func namedSpec(id string, steps ...StepSpec) *Spec {
	spec := agentSpec(id, steps...)
	spec.ID = id
	return spec
}

func TestScheduledWorkflowCheckSeesThroughChildWorkflows(t *testing.T) {
	webChild := namedSpec("web-child", sinkWebAgent("research", "web_search"))
	leakyChild := namedSpec("leaky-child", sinkWebAgent("research", "web_fetch"), after(sinkToolStep("send", "email_send"), "research"))
	cleanChild := namedSpec("clean-child", sinkPlainAgent("write"))
	cases := []struct {
		name    string
		root    *Spec
		others  []*Spec
		refused bool
	}{
		{"a sink after a child that holds a web step", namedSpec("p", childStep("child", "web-child"), after(sinkToolStep("send", "email_send"), "child")), []*Spec{webChild}, true},
		{"a child whose own steps send after a web step", namedSpec("p", childStep("child", "leaky-child")), []*Spec{leakyChild}, true},
		{"a child two levels down that sends after a web step", namedSpec("p", childStep("child", "mid")), []*Spec{namedSpec("mid", childStep("inner", "leaky-child")), leakyChild}, true},
		{"a web step followed by a child that sends", namedSpec("p", sinkWebAgent("research", "web_search"), childStep("child", "sender", "research")), []*Spec{namedSpec("sender", sinkToolStep("send", "email_send"))}, true},
		{"a web step followed by a child that only writes text", namedSpec("p", sinkWebAgent("research", "web_search"), childStep("child", "clean-child", "research")), []*Spec{cleanChild}, false},
		{"a web child followed by a plain agent step", namedSpec("p", childStep("child", "web-child"), after(sinkPlainAgent("write"), "child")), []*Spec{webChild}, false},
		{"a clean child followed by a sink", namedSpec("p", childStep("child", "clean-child"), after(sinkToolStep("send", "email_send"), "child")), []*Spec{cleanChild}, false},
		{"a missing child", namedSpec("p", childStep("child", "ghost")), nil, true},
		{"a workflow that starts itself", namedSpec("p", childStep("child", "p")), nil, true},
		{"two workflows that start each other", namedSpec("p", childStep("child", "q")), []*Spec{namedSpec("q", childStep("back", "p"))}, true},
		{"a deep chain that ends in a missing child", namedSpec("p", childStep("child", "mid")), []*Spec{namedSpec("mid", childStep("inner", "ghost"))}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := CheckUnattendedSinks(tc.root, sinkDeps(), catalogOf(append([]*Spec{tc.root}, tc.others...)...))
			if tc.refused && !hasCode(err, ErrorCodeUnattendedSink) {
				t.Errorf("CheckUnattendedSinks = %v, want an unattended sink refusal", err)
			}
			if !tc.refused && err != nil {
				t.Errorf("CheckUnattendedSinks = %v, want accepted", err)
			}
		})
	}
	t.Run("a child that cannot be looked up at all", func(t *testing.T) {
		root := namedSpec("p", childStep("child", "clean-child"))
		if err := CheckUnattendedSinks(root, sinkDeps(), nil); !hasCode(err, ErrorCodeUnattendedSink) {
			t.Errorf("CheckUnattendedSinks = %v, want an unattended sink refusal", err)
		}
	})
}

func TestTwoStepJobUsesPrivateContextWithoutSinks(t *testing.T) {
	cases := map[string][]StepSpec{
		"a web step then an agent step without web tools": {sinkWebAgent("research", "web_search", "web_fetch"), sinkPlainAgent("write", "research")},
		"a tool search then an agent step":                {sinkToolStep("search", "web_search"), sinkPlainAgent("write", "search")},
		"an approval and a wait after the web step":       {sinkWebAgent("research", "web_fetch"), sinkKindStep("gate", KindApproval, "research"), sinkKindStep("pause", KindWait, "gate")},
		"a read tool after the web step":                  {sinkWebAgent("research", "web_search"), sinkToolStep("read", "read_file", "research")},
		"an effectful step that does not follow the web":  {sinkWebAgent("research", "web_search"), sinkPlainAgent("write", "research"), sinkKindStep("gate", KindApproval), sinkToolStep("save", "write_file", "gate")},
		"no web step at all":                              {sinkToolStep("read", "read_file"), sinkKindStep("gate", KindApproval, "read"), sinkToolStep("save", "write_file", "gate")},
	}
	for name, steps := range cases {
		t.Run(name, func(t *testing.T) {
			if err := CheckUnattendedSinks(agentSpec("job", steps...), sinkDeps(), nil); err != nil {
				t.Errorf("CheckUnattendedSinks = %v, want the workflow allowed", err)
			}
		})
	}
	t.Run("the check needs a spec and the tool sets", func(t *testing.T) {
		if err := CheckUnattendedSinks(nil, sinkDeps(), nil); !hasCode(err, ErrorCodeInvalidArgument) {
			t.Errorf("nil spec = %v", err)
		}
		if err := CheckUnattendedSinks(agentSpec("job"), nil, nil); !hasCode(err, ErrorCodeInvalidArgument) {
			t.Errorf("nil deps = %v", err)
		}
	})
}

func TestScheduledFireRechecksTheWorkflow(t *testing.T) {
	interpreter, _, disk := driverTestSetup(t, &fakeToolRunner{}, DriverConcurrent)
	deps := testValidationDeps()
	if err := interpreter.Install(t.Context(), []*Spec{twoStepCeilingSpec("later")}, deps); err != nil {
		t.Fatalf("Install: %v", err)
	}
	owner := func(scheduled bool) *ConfiguredStart {
		return &ConfiguredStart{DefinitionID: "later", PrincipalID: principal.OwnerID, Class: principal.ClassOwner, Scheduled: scheduled}
	}
	if _, err := interpreter.StartConfigured(t.Context(), owner(true)); err != nil {
		t.Fatalf("a scheduled start of a workflow with no web step: %v", err)
	}

	changed := testValidationDeps()
	changed.SinkTools = []string{"read_file"}
	if err := interpreter.Install(t.Context(), []*Spec{twoStepCeilingSpec("later")}, changed); err != nil {
		t.Fatalf("Install again: %v", err)
	}
	before := countRuns(t, disk)
	if _, err := interpreter.StartConfigured(t.Context(), owner(true)); !hasCode(err, ErrorCodeUnattendedSink) {
		t.Fatalf("a scheduled start after the tool sets changed = %v, want %s", err, ErrorCodeUnattendedSink)
	}
	if countRuns(t, disk) != before {
		t.Error("a refused scheduled start created a run")
	}
	if _, err := interpreter.StartConfigured(t.Context(), owner(false)); err != nil {
		t.Errorf("an unscheduled start is refused for a rule that only binds scheduled runs: %v", err)
	}
}

func TestScheduledWorkflowOfATaintedJobRunsTainted(t *testing.T) {
	interpreter, _, _ := driverTestSetup(t, &fakeToolRunner{}, DriverConcurrent)
	installAll(t, interpreter, toolSpec("plain"))
	for name, tainted := range map[string]bool{"a clean job": false, "a tainted job": true} {
		t.Run(name, func(t *testing.T) {
			summary, err := interpreter.StartConfigured(t.Context(), &ConfiguredStart{
				DefinitionID: "plain", PrincipalID: principal.OwnerID, Class: principal.ClassOwner, Scheduled: true, Tainted: tainted,
			})
			if err != nil || summary.Origin.Tainted != tainted {
				t.Errorf("run = %+v, %v, want tainted=%t", summary, err, tainted)
			}
		})
	}
}

func TestScheduledStartChecksComposedChildren(t *testing.T) {
	changed := testValidationDeps()
	changed.SinkTools = []string{"read_file"}
	cases := map[string]*Spec{
		"a parent whose only step is the child":   composingSpec("top", "leaf"),
		"a parent that sends after the child":     composedThenWrite("top", "leaf"),
		"a parent that holds the child in a pair": composingSpec("top", "leaf", "leaf"),
	}
	for name, parent := range cases {
		t.Run(name, func(t *testing.T) {
			interpreter, _, disk := driverTestSetup(t, &fakeToolRunner{}, DriverConcurrent)
			if err := interpreter.Install(t.Context(), []*Spec{twoStepCeilingSpec("leaf")}, testValidationDeps()); err != nil {
				t.Fatalf("Install the child: %v", err)
			}
			if err := interpreter.Install(t.Context(), []*Spec{parent}, testValidationDeps()); err != nil {
				t.Fatalf("Install the parent: %v", err)
			}
			start := &ConfiguredStart{DefinitionID: "top", PrincipalID: principal.OwnerID, Class: principal.ClassOwner, Scheduled: true}
			if _, err := interpreter.StartConfigured(t.Context(), start); err != nil {
				t.Fatalf("a scheduled start while the child has no web step: %v", err)
			}
			interpreter.setDeps(changed)
			before := countRuns(t, disk)
			if _, err := interpreter.StartConfigured(t.Context(), start); !hasCode(err, ErrorCodeUnattendedSink) {
				t.Fatalf("a scheduled start once the child has a web step = %v, want %s", err, ErrorCodeUnattendedSink)
			}
			if countRuns(t, disk) != before {
				t.Error("a refused scheduled start created a run")
			}
			unscheduled := *start
			unscheduled.Scheduled = false
			if _, err := interpreter.StartConfigured(t.Context(), &unscheduled); err != nil {
				t.Errorf("an unscheduled start is refused for a rule that only binds scheduled runs: %v", err)
			}
		})
	}
}

func composedThenWrite(id, child string) *Spec {
	spec := composingSpec(id, child)
	write := "write_file"
	spec.Steps = append(spec.Steps,
		StepSpec{ID: "gate", DependsOn: []string{"childa"}, Executor: ExecutorSpec{Kind: KindApproval}, Timeout: 5 * time.Second},
		StepSpec{ID: "after", DependsOn: []string{"gate"}, Executor: ExecutorSpec{Kind: KindTool, ToolID: &write, ToolArgs: json.RawMessage(`{"path":"a","content":"b"}`)}, Timeout: 5 * time.Second})
	return spec
}

func TestChildOfAScheduledRunIsCheckedWhenItIsCreated(t *testing.T) {
	interpreter, _, disk := driverTestSetup(t, &fakeToolRunner{}, DriverConcurrent)
	installAll(t, interpreter, toolSpec("leaf"), composingSpec("top", "leaf"))
	start := func(scheduled bool) *RunSummary {
		t.Helper()
		summary, err := interpreter.StartConfigured(t.Context(), &ConfiguredStart{DefinitionID: "top", PrincipalID: principal.OwnerID, Class: principal.ClassOwner, Scheduled: scheduled})
		if err != nil {
			t.Fatalf("StartConfigured: %v", err)
		}
		return summary
	}
	scheduled, plain := start(true), start(false)

	changed := testValidationDeps()
	changed.SinkTools = []string{"read_file"}
	if err := interpreter.Install(t.Context(), []*Spec{twoStepCeilingSpec("leaf2")}, changed); err != nil {
		t.Fatalf("Install: %v", err)
	}
	if _, err := interpreter.createChildRun(t.Context(), "leaf2", nil, scheduled.ID, &scheduled.Origin); !hasCode(err, ErrorCodeUnattendedSink) {
		t.Errorf("a child of a scheduled run that reads then sends = %v, want %s", err, ErrorCodeUnattendedSink)
	}
	if _, err := interpreter.createChildRun(t.Context(), "leaf2", nil, plain.ID, &plain.Origin); err != nil {
		t.Errorf("a child of an unscheduled run = %v, want it started", err)
	}
	if got := countLeafTwoRuns(t, disk, scheduled.ID); got != 0 {
		t.Errorf("the refused child left %d runs", got)
	}
}

func countLeafTwoRuns(t *testing.T, disk *Store, parentID string) int {
	t.Helper()
	var count int
	if err := disk.db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM workflow_run WHERE parent_run_id = ? AND definition_id = 'leaf2'`, parentID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

func TestDurableKeyPrefixOfScheduledRunsIsReserved(t *testing.T) {
	interpreter, _, _ := driverTestSetup(t, &fakeToolRunner{}, DriverConcurrent)
	installAll(t, interpreter, toolSpec("plain"))
	_, err := interpreter.StartConfigured(t.Context(), &ConfiguredStart{DefinitionID: "plain", PrincipalID: principal.OwnerID, Class: principal.ClassOwner, DurableKey: ScheduledKeyPrefix + "forged"})
	if !hasCode(err, ErrorCodeInvalidArgument) {
		t.Errorf("an unscheduled start under the scheduled prefix = %v, want %s", err, ErrorCodeInvalidArgument)
	}
}
