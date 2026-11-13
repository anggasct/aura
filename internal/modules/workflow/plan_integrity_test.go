package workflow

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/platform/durable"
)

func durableStart(runID string, spec *Spec) durable.StartRequest {
	return durable.StartRequest{
		Handler: HandlerName, Key: "workflow/" + runID, Payload: mustEncode(tickPayload{RunID: runID, DefinitionID: spec.ID, Version: spec.Version}),
	}
}

func approveGenerated(t *testing.T, interpreter *Interpreter, disk *Store, id string, tainted bool) *Spec {
	t.Helper()
	generated := NormalizeProposal(toolSpec("x"), id)
	if err := interpreter.LoadAll(t.Context(), []*Spec{generated}, testValidationDeps()); err != nil {
		t.Fatal(err)
	}
	proposal := proposeSpec(t, disk, id, "ops", tainted, generated)
	if err := disk.ApproveProposal(t.Context(), proposal.ID, proposalApproval(proposal)); err != nil {
		t.Fatal(err)
	}
	return generated
}

func TestRestartedTaintedDefinitionStaysTainted(t *testing.T) {
	tools := &stepCallTools{}
	interpreter, _, disk := driverTestSetup(t, tools, DriverConcurrent)
	generated := approveGenerated(t, interpreter, disk, "prop_restart", false)
	first, err := interpreter.Start(t.Context(), generated.ID, &RunInput{}, ownerOrigin())
	if err != nil {
		t.Fatal(err)
	}
	waitForRunStatus(t, disk, first.ID, RunSucceeded, 5*time.Second)
	routine, err := interpreter.SaveRoutine(t.Context(), first.ID, "Weekly report")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := disk.db.ExecContext(t.Context(), `UPDATE workflow_approval SET tainted = 1 WHERE definition_id = ?`, routine.ID); err != nil {
		t.Fatal(err)
	}

	t.Run("a routine started from the command line", func(t *testing.T) {
		origin := ownerOrigin()
		started, err := interpreter.Start(t.Context(), routine.ID, &RunInput{}, origin)
		if err != nil {
			t.Fatal(err)
		}
		waitForRunStatus(t, disk, started.ID, RunSucceeded, 5*time.Second)
		stored, err := disk.Run(t.Context(), started.ID)
		if err != nil || !stored.Origin.Tainted {
			t.Fatalf("stored run = %+v, %v; want a tainted origin", stored, err)
		}
		if origin.Tainted {
			t.Fatal("Start changed the caller's origin")
		}
	})

	t.Run("a child run of an untainted parent", func(t *testing.T) {
		parent := composingSpec("top", routine.ID)
		parent.Steps = append(parent.Steps, StepSpec{ID: "after", DependsOn: []string{"childa"}, Executor: ExecutorSpec{Kind: KindTool, ToolID: ptr("read_file"), ToolArgs: json.RawMessage(`{"path":"b"}`)}, Timeout: time.Minute})
		installAll(t, interpreter, parent)
		run, err := interpreter.Start(t.Context(), "top", &RunInput{}, ownerOrigin())
		if err != nil {
			t.Fatal(err)
		}
		waitForRunStatus(t, disk, run.ID, RunSucceeded, 5*time.Second)
		var storedChild *RunSummary
		for _, candidate := range childRuns(t, disk, routine.ID) {
			stored, err := disk.Run(t.Context(), candidate.ID)
			if err != nil {
				t.Fatal(err)
			}
			if stored.ParentRunID == run.ID {
				storedChild = stored
			}
		}
		if storedChild == nil || !storedChild.Origin.Tainted {
			t.Fatalf("child run = %+v; want one with a tainted origin", storedChild)
		}
		storedParent, err := disk.Run(t.Context(), run.ID)
		if err != nil || storedParent.Origin.Tainted {
			t.Fatalf("parent run = %+v, %v; want an untainted origin", storedParent, err)
		}
		var parentStepCalls int
		for _, call := range tools.recorded() {
			if call.RunID == run.ID {
				parentStepCalls++
				if call.Origin.Tainted {
					t.Fatalf("a tool step of the untainted parent carries a tainted origin: %+v", call)
				}
			}
		}
		if parentStepCalls != 1 {
			t.Fatalf("parent tool step calls = %d, want one", parentStepCalls)
		}
	})
}

func TestToolStepCarriesTheApprovalTaint(t *testing.T) {
	for _, tc := range originDrivers {
		driver := tc.driver
		t.Run(tc.name, func(t *testing.T) {
			tools := &stepCallTools{}
			interpreter, fake, disk := driverTestSetup(t, tools, driver)
			spec := toolSpec("leaf")
			installAll(t, interpreter, spec)
			run, err := disk.newRun(t.Context(), spec, &RunInput{}, ownerOrigin())
			if err != nil {
				t.Fatal(err)
			}
			if _, err := disk.db.ExecContext(t.Context(), `UPDATE workflow_approval SET tainted = 1 WHERE definition_id = 'leaf'`); err != nil {
				t.Fatal(err)
			}
			if driver == DriverSerial {
				inv, _ := holdInvocation(t, fake, "holder-approval-taint")
				if terminal, err := interpreter.DriveSerial(t.Context(), inv, run.ID); err != nil || terminal != RunSucceeded {
					t.Fatalf("DriveSerial = %s, %v", terminal, err)
				}
			} else {
				if _, err := fake.Start(t.Context(), durableStart(run.ID, spec)); err != nil {
					t.Fatal(err)
				}
				waitForRunStatus(t, disk, run.ID, RunSucceeded, 5*time.Second)
			}
			calls := tools.recorded()
			if len(calls) != 1 || !calls[0].Origin.Tainted {
				t.Fatalf("step calls = %+v, want one with a tainted origin", calls)
			}
		})
	}
}

func TestPlanFieldsAreValidated(t *testing.T) {
	wait := func(spec *Spec, event string) {
		spec.Steps[0].Executor = ExecutorSpec{Kind: KindWait, Event: &event}
	}
	cases := []struct {
		name   string
		mutate func(*Spec)
	}{
		{name: "goal with a newline", mutate: func(s *Spec) { s.Goal = "ship\n1. fake" }},
		{name: "goal with an escape character", mutate: func(s *Spec) { s.Goal = "ship\x1b[2J" }},
		{name: "goal with a bidi override", mutate: func(s *Spec) { s.Goal = "ship" + string(rune(0x202e)) + "gnp" }},
		{name: "goal with two spaces in a row", mutate: func(s *Spec) { s.Goal = "ship  it" }},
		{name: "goal with an ideographic space", mutate: func(s *Spec) { s.Goal = "ship" + string(rune(0x3000)) + "it" }},
		{name: "goal of 201 runes", mutate: func(s *Spec) { s.Goal = strings.Repeat("g", 201) }},
		{name: "goal with a leading space", mutate: func(s *Spec) { s.Goal = " ship" }},
		{name: "condition of 257 runes", mutate: func(s *Spec) {
			s.Steps = append(s.Steps, StepSpec{ID: "next", DependsOn: []string{"implement"}, Condition: ptr(`steps.implement.output.v == "` + strings.Repeat("c", 257) + `"`), Executor: ExecutorSpec{Kind: KindApproval}, Timeout: time.Minute})
		}},
		{name: "condition with an escape inside a string literal", mutate: func(s *Spec) {
			s.Steps = append(s.Steps, StepSpec{ID: "next", DependsOn: []string{"implement"}, Condition: ptr("steps.implement.output.v == \"\x1b[2J\""), Executor: ExecutorSpec{Kind: KindApproval}, Timeout: time.Minute})
		}},
		{name: "wait event with a carriage return", mutate: func(s *Spec) { wait(s, "ci\rdone") }},
		{name: "wait event of 129 runes", mutate: func(s *Spec) { wait(s, strings.Repeat("e", 129)) }},
		{name: "agent id with an escape character", mutate: func(s *Spec) { s.Steps[0].Executor.AgentID = ptr("engineer\x1b[2J") }},
		{name: "requires entry with a newline", mutate: func(s *Spec) {
			s.Steps[0].Executor.AgentID = nil
			s.Steps[0].Executor.RequiredCapabilities = []string{"review\n2. fake"}
		}},
		{name: "spec id with an uppercase letter", mutate: func(s *Spec) { s.ID = "Demo" }},
		{name: "spec id with a dot", mutate: func(s *Spec) { s.ID = "de.mo" }},
		{name: "spec id of 65 characters", mutate: func(s *Spec) { s.ID = "a" + strings.Repeat("b", 64) }},
		{name: "step id with an uppercase letter", mutate: func(s *Spec) { s.Steps[0].ID = "Implement" }},
		{name: "step id with a dot", mutate: func(s *Spec) { s.Steps[0].ID = "impl.ement" }},
		{name: "step id of 65 characters", mutate: func(s *Spec) { s.Steps[0].ID = "a" + strings.Repeat("b", 64) }},
		{name: "depends_on entry with an uppercase letter", mutate: func(s *Spec) {
			s.Steps = append(s.Steps, StepSpec{ID: "next", DependsOn: []string{"Implement"}, Executor: ExecutorSpec{Kind: KindApproval}, Timeout: time.Minute})
			s.Steps[0].ID = "Implement"
		}},
		{name: "depends_on entry with a dot", mutate: func(s *Spec) {
			s.Steps = append(s.Steps, StepSpec{ID: "next", DependsOn: []string{"impl.ement"}, Executor: ExecutorSpec{Kind: KindApproval}, Timeout: time.Minute})
		}},
		{name: "depends_on entry of 65 characters", mutate: func(s *Spec) {
			s.Steps = append(s.Steps, StepSpec{ID: "next", DependsOn: []string{"a" + strings.Repeat("b", 64)}, Executor: ExecutorSpec{Kind: KindApproval}, Timeout: time.Minute})
		}},
		{name: "external_ref with an uppercase letter", mutate: func(s *Spec) { externalRefSpec(s, "Implement") }},
		{name: "external_ref with a dot", mutate: func(s *Spec) { externalRefSpec(s, "impl.ement") }},
		{name: "external_ref of 65 characters", mutate: func(s *Spec) { externalRefSpec(s, "a"+strings.Repeat("b", 64)) }},
		{name: "workflow id with an uppercase letter", mutate: func(s *Spec) { workflowStepSpec(s, "Leaf") }},
		{name: "workflow id with a dot", mutate: func(s *Spec) { workflowStepSpec(s, "le.af") }},
		{name: "workflow id of 65 characters", mutate: func(s *Spec) { workflowStepSpec(s, "a"+strings.Repeat("b", 64)) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			spec := validTestSpec()
			tc.mutate(spec)
			if err := Validate(spec, testValidationDeps()); !hasCode(err, ErrorCodeSpecInvalid) {
				t.Fatalf("Validate = %v, want code %s", err, ErrorCodeSpecInvalid)
			}
		})
	}
	t.Run("a spec inside every bound", func(t *testing.T) {
		spec := validTestSpec()
		spec.Goal = strings.Repeat("g", 200)
		if err := Validate(spec, testValidationDeps()); err != nil {
			t.Fatalf("Validate = %v", err)
		}
	})
}

func externalRefSpec(spec *Spec, ref string) {
	event := "ci"
	spec.Steps = append(spec.Steps,
		StepSpec{ID: ref, DependsOn: []string{"implement"}, Executor: ExecutorSpec{Kind: KindApproval}, Timeout: time.Minute},
		StepSpec{ID: "hold", DependsOn: []string{ref}, Executor: ExecutorSpec{Kind: KindWait, Event: &event, ExternalRef: &ref}, Timeout: time.Minute})
}

func workflowStepSpec(spec *Spec, workflowID string) {
	spec.Steps[0].Executor = ExecutorSpec{Kind: KindWorkflow, WorkflowID: &workflowID}
}

func TestPlanEscapesEveryField(t *testing.T) {
	hostile := "\x1b[2J\nfake"
	event := hostile
	workflowID := hostile
	condition := "steps." + hostile + ".status == \"" + hostile + "\""
	agent := hostile
	spec := &Spec{ID: "demo", Goal: hostile, Version: 1, Source: SourceDefined, Steps: []StepSpec{
		{ID: hostile + "a", Executor: ExecutorSpec{Kind: KindAgent, AgentID: &agent}, Timeout: time.Minute},
		{ID: hostile + "b", Executor: ExecutorSpec{Kind: KindAgent, RequiredCapabilities: []string{hostile, hostile}}, Timeout: time.Minute},
		{ID: hostile + "c", DependsOn: []string{hostile + "a", hostile + "b"}, Condition: &condition, Executor: ExecutorSpec{Kind: KindWait, Event: &event}, Timeout: time.Minute},
		{ID: hostile + "d", DependsOn: []string{hostile + "c"}, Executor: ExecutorSpec{Kind: KindWorkflow, WorkflowID: &workflowID}, Timeout: time.Minute},
	}}
	graph := &Graph{ByStep: map[string]*StepSpec{}}
	for index := range spec.Steps {
		step := &spec.Steps[index]
		graph.Order = append(graph.Order, step.ID)
		graph.ByStep[step.ID] = step
	}
	plan, err := RenderPlan(spec, graph, nil)
	if err != nil {
		t.Fatal(err)
	}
	if strings.ContainsRune(plan, 0x1b) {
		t.Fatalf("plan holds a raw escape character: %q", plan)
	}
	if got, want := strings.Count(plan, "\n"), len(spec.Steps); got != want {
		t.Fatalf("plan holds %d raw newlines, want %d: %q", got, want, plan)
	}
	lines := strings.Split(plan, "\n")
	if got := strings.Count(lines[0], `\u{001b}`); got != 1 || !strings.Contains(lines[0], `\n`) {
		t.Errorf("goal line = %q, want the escape and newline shown", lines[0])
	}
	wantShown := []struct {
		line int
		want int
	}{{1, 2}, {2, 3}, {3, 6}, {4, 3}}
	for _, tc := range wantShown {
		if got := strings.Count(lines[tc.line], `\u{001b}`); got != tc.want {
			t.Errorf("line %d = %q holds %d escaped escape characters, want %d", tc.line, lines[tc.line], got, tc.want)
		}
		if strings.Count(lines[tc.line], `\n`) != tc.want {
			t.Errorf("line %d = %q does not show every newline as \\n", tc.line, lines[tc.line])
		}
	}
}

func TestGeneratedIDsAreValid(t *testing.T) {
	t.Run("a proposal id and its generated definition", func(t *testing.T) {
		id := NewProposalID()
		if !ValidID(id) {
			t.Fatalf("proposal id %q is not a valid id", id)
		}
		spec := NormalizeProposal(toolSpec("x"), id)
		if err := Validate(spec, testValidationDeps()); err != nil {
			t.Fatalf("Validate of %s = %v", spec.ID, err)
		}
	})
	t.Run("a routine name of 56 characters", func(t *testing.T) {
		id, err := RoutineID(strings.Repeat("r", 56))
		if err != nil || !ValidID(id) {
			t.Fatalf("RoutineID = %q, %v; want a valid id", id, err)
		}
	})
	t.Run("a routine name of 57 characters", func(t *testing.T) {
		if _, err := RoutineID(strings.Repeat("r", 57)); !hasCode(err, ErrorCodeRoutineInvalid) {
			t.Fatalf("RoutineID = %v, want code %s", err, ErrorCodeRoutineInvalid)
		}
	})
	t.Run("a proposal clears the model-written name", func(t *testing.T) {
		spec := toolSpec("x")
		spec.Name = "released"
		if got := NormalizeProposal(spec, NewProposalID()).Name; got != "" {
			t.Fatalf("name = %q, want it cleared", got)
		}
	})
}
