package workflow

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/config"
	auraagent "github.com/anggasct/aura/internal/platform/agent"
	"github.com/anggasct/aura/internal/platform/durable"
	"github.com/anggasct/aura/internal/platform/principal"
	"github.com/anggasct/aura/internal/platform/tools"
)

func registryStepAllowed(registry *auraagent.Registry, extra ...tools.Definition) func(string, principal.Class, string) (bool, bool) {
	catalog := map[string]*tools.Definition{}
	builtins := tools.Builtins()
	for i := range builtins {
		catalog[builtins[i].Name] = &builtins[i]
	}
	for i := range extra {
		catalog[extra[i].Name] = &extra[i]
	}
	return func(agentID string, class principal.Class, toolID string) (bool, bool) {
		persona, known := registry.Lookup(agentID)
		if !known {
			return false, false
		}
		tool, found := catalog[toolID]
		return found && persona.StepToolAllowed(class, tool), true
	}
}

func ceilingTestRegistry(t *testing.T) *auraagent.Registry {
	t.Helper()
	overrides := []config.AgentDefinition{
		{ID: "open", Description: "no list", Instructions: "work"},
		{ID: "lister", Description: "lists one", Instructions: "work", Tools: []string{"read_file"}},
		{ID: "gh", Description: "lists github", Instructions: "work", Tools: []string{"github_comment"}},
	}
	known := []string{"read_file", "write_file", "exec", "list_dir", "web_fetch", "web_search", "github_comment"}
	registry, err := auraagent.Build(overrides, known, []string{"primary"})
	if err != nil {
		t.Fatal(err)
	}
	return registry
}

func ceilingSpec(toolID string) *Spec {
	args := json.RawMessage(`{"path":"a","content":"b"}`)
	return &Spec{ID: "gen_ceiling", Goal: "Check the ceiling", Version: 1, Source: SourceGenerated, Steps: []StepSpec{
		{ID: "approve", Executor: ExecutorSpec{Kind: KindApproval}, Timeout: time.Minute},
		{ID: "act", DependsOn: []string{"approve"}, Executor: ExecutorSpec{Kind: KindTool, ToolID: &toolID, ToolArgs: args}, Timeout: time.Minute},
	}}
}

func TestProposalCeilingUsesTheSharedPredicate(t *testing.T) {
	registry := ceilingTestRegistry(t)
	comment := tools.Definition{Name: "github_comment", Version: tools.Version, OptIn: true, Effectful: true}
	tests := []struct {
		name     string
		persona  string
		tool     string
		noRule   bool
		wantCode ErrorCode
	}{
		{name: "an empty list allows a plain builtin", persona: "open", tool: "write_file"},
		{name: "an empty list refuses an opt-in tool", persona: "open", tool: "github_comment", wantCode: ErrorCodeCapabilityExceeded},
		{name: "a listed opt-in tool is allowed", persona: "gh", tool: "github_comment"},
		{name: "an unlisted tool is refused", persona: "lister", tool: "write_file", wantCode: ErrorCodeCapabilityExceeded},
		{name: "an unknown persona is refused", persona: "ghost", tool: "write_file", wantCode: ErrorCodePersonaMissing},
		{name: "a missing predicate refuses every tool step", persona: "open", tool: "write_file", noRule: true, wantCode: ErrorCodeCapabilityExceeded},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			deps := testValidationDeps()
			deps.KnownTools = append(deps.KnownTools, "github_comment")
			deps.EffectfulTools = append(deps.EffectfulTools, "github_comment")
			deps.Agents = registry
			deps.StepAllowed = registryStepAllowed(registry, comment)
			if tc.noRule {
				deps.StepAllowed = nil
			}
			interpreter := freshInterpreter(t)
			if err := interpreter.LoadAll(t.Context(), nil, deps); err != nil {
				t.Fatalf("LoadAll: %v", err)
			}
			_, err := interpreter.CompileProposal(ceilingSpec(tc.tool), tc.persona)
			if tc.wantCode == "" {
				if err != nil {
					t.Fatalf("CompileProposal = %v, want nil", err)
				}
				return
			}
			if !hasCode(err, tc.wantCode) {
				t.Fatalf("CompileProposal = %v, want %s", err, tc.wantCode)
			}
		})
	}
	t.Run("an unknown persona is refused even without tool steps", func(t *testing.T) {
		interpreter := freshInterpreter(t)
		if err := interpreter.LoadAll(t.Context(), nil, testValidationDeps()); err != nil {
			t.Fatalf("LoadAll: %v", err)
		}
		spec := &Spec{ID: "gen_quiet", Goal: "Only ask", Version: 1, Source: SourceGenerated, Steps: []StepSpec{
			{ID: "approve", Executor: ExecutorSpec{Kind: KindApproval}, Timeout: time.Minute},
		}}
		if _, err := interpreter.CompileProposal(spec, "ghost"); !hasCode(err, ErrorCodePersonaMissing) {
			t.Fatalf("CompileProposal = %v, want %s", err, ErrorCodePersonaMissing)
		}
	})
}

func twoStepCeilingSpec(id string) *Spec {
	read, write := "read_file", "write_file"
	return &Spec{ID: id, Goal: "Read then write", Version: 1, Source: SourceDefined, Steps: []StepSpec{
		{ID: "one", Executor: ExecutorSpec{Kind: KindTool, ToolID: &read, ToolArgs: json.RawMessage(`{"path":"a"}`)}, Timeout: 5 * time.Second},
		{ID: "gate", DependsOn: []string{"one"}, Executor: ExecutorSpec{Kind: KindApproval}, Timeout: 5 * time.Second},
		{ID: "two", DependsOn: []string{"gate"}, Executor: ExecutorSpec{Kind: KindTool, ToolID: &write, ToolArgs: json.RawMessage(`{"path":"a","content":"b"}`)}, Timeout: 5 * time.Second},
	}}
}

func countRunSessions(t *testing.T, disk *Store) int {
	t.Helper()
	var count int
	if err := disk.db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM session WHERE id GLOB 'wf-*'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

func TestEveryStartChecksTheCeilingBeforeTheRun(t *testing.T) {
	tests := []struct {
		name string
		key  string
		run  func(ctx context.Context, interpreter *Interpreter) error
	}{
		{name: "a configured start", key: "sched-ceiling", run: func(ctx context.Context, interpreter *Interpreter) error {
			_, err := interpreter.StartConfigured(ctx, "wide", nil, principal.OwnerID, principal.ClassOwner, "sched-ceiling")
			return err
		}},
		{name: "a start with an explicit origin", run: func(ctx context.Context, interpreter *Interpreter) error {
			_, err := interpreter.Start(ctx, "wide", nil, ownerOrigin())
			return err
		}},
		{name: "a child run", run: func(ctx context.Context, interpreter *Interpreter) error {
			_, err := interpreter.createChildRun(ctx, "wide", &RunInput{}, "run-parent", ownerOrigin())
			return err
		}},
		{name: "a root whose composed definition is outside the ceiling", key: "sched-composed", run: func(ctx context.Context, interpreter *Interpreter) error {
			_, err := interpreter.StartConfigured(ctx, "top", nil, principal.OwnerID, principal.ClassOwner, "sched-composed")
			return err
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			runner := &fakeToolRunner{}
			interpreter, fake, disk := driverTestSetup(t, runner, DriverConcurrent)
			deps := testValidationDeps()
			deps.StepAllowed = personaTools(map[string][]string{"main": {"read_file"}})
			if err := interpreter.Install(t.Context(), []*Spec{twoStepCeilingSpec("wide"), composingSpec("top", "wide")}, deps); err != nil {
				t.Fatalf("Install: %v", err)
			}
			err := tc.run(t.Context(), interpreter)
			if !hasCode(err, ErrorCodeCapabilityExceeded) || !strings.Contains(err.Error(), `"two"`) {
				t.Fatalf("start = %v, want %s naming the step", err, ErrorCodeCapabilityExceeded)
			}
			if got := countRuns(t, disk); got != 0 {
				t.Fatalf("%d run rows written, want none", got)
			}
			if got := countRunSessions(t, disk); got != 0 {
				t.Fatalf("%d run sessions written, want none", got)
			}
			if tc.key != "" {
				if _, err := fake.Status(t.Context(), durable.RunRef{Key: tc.key}); !errors.Is(err, durable.ErrUnknownRun) {
					t.Fatalf("durable run %s = %v, want it unknown", tc.key, err)
				}
			}
			if len(runner.called) != 0 {
				t.Fatalf("the tool runner was called %v, want never", runner.called)
			}
		})
	}
}

func installedRunAs(id, persona string) *Spec {
	spec := toolSpec(id)
	spec.RunAs = persona
	return spec
}

func TestConfiguredStartRunsAsTheDeclaredPersona(t *testing.T) {
	approveAs := func(t *testing.T, interpreter *Interpreter, disk *Store, spec *Spec, agent string) {
		t.Helper()
		if err := interpreter.LoadAll(t.Context(), []*Spec{spec}, testValidationDeps()); err != nil {
			t.Fatal(err)
		}
		proposal := proposeSpec(t, disk, "prop_"+agent, agent, false, spec)
		if err := disk.ApproveProposal(t.Context(), proposal.ID, proposalApproval(proposal)); err != nil {
			t.Fatal(err)
		}
	}
	t.Run("an installed definition runs as its run_as persona, or main", func(t *testing.T) {
		interpreter, _, _ := driverTestSetup(t, &fakeToolRunner{}, DriverConcurrent)
		installAll(t, interpreter, installedRunAs("declared", "lobby"), toolSpec("plain"))
		for id, want := range map[string]string{"declared": "lobby", "plain": "main"} {
			summary, err := interpreter.StartConfigured(t.Context(), id, nil, principal.OwnerID, principal.ClassOwner, "")
			if err != nil || summary.Origin.AgentID != want {
				t.Fatalf("StartConfigured(%s) = %+v, %v, want persona %s", id, summary, err, want)
			}
		}
	})
	t.Run("a proposal and a routine run as the persona on their record", func(t *testing.T) {
		interpreter, _, disk := driverTestSetup(t, &fakeToolRunner{}, DriverConcurrent)
		generated := NormalizeProposal(toolSpec("x"), "prop_lobby")
		approveAs(t, interpreter, disk, generated, "lobby")
		routine := &Spec{ID: "routine_one", Name: "One", Goal: "Run once", Version: 1, Source: SourceGenerated, Steps: toolSpec("x").Steps}
		if err := interpreter.load(t.Context(), []*Spec{routine}, testValidationDeps(), &Approval{Kind: ApprovalRoutine, AgentID: "lobby", ApproverID: principal.OwnerID, SourceDefinitionID: generated.ID, SourceDefinitionVersion: generated.Version}); err != nil {
			t.Fatal(err)
		}
		for _, id := range []string{generated.ID, routine.ID} {
			summary, err := interpreter.StartConfigured(t.Context(), id, nil, principal.OwnerID, principal.ClassOwner, "")
			if err != nil || summary.Origin.AgentID != "lobby" {
				t.Fatalf("StartConfigured(%s) = %+v, %v, want persona lobby", id, summary, err)
			}
		}
	})
	t.Run("a keyed start returns the run it already made", func(t *testing.T) {
		interpreter, _, disk := driverTestSetup(t, &fakeToolRunner{}, DriverConcurrent)
		installAll(t, interpreter, toolSpec("plain"))
		first, err := interpreter.StartConfigured(t.Context(), "plain", nil, principal.OwnerID, principal.ClassOwner, "sched-once")
		if err != nil {
			t.Fatal(err)
		}
		again, err := interpreter.StartConfigured(t.Context(), "plain", nil, principal.OwnerID, principal.ClassOwner, "sched-once")
		if err != nil || again.ID != first.ID || countRuns(t, disk) != 1 {
			t.Fatalf("keyed restart = %+v, %v, want run %s only", again, err, first.ID)
		}
	})
	t.Run("a class the persona does not admit is refused", func(t *testing.T) {
		interpreter, _, disk := driverTestSetup(t, &fakeToolRunner{}, DriverConcurrent)
		installAll(t, interpreter, installedRunAs("front", "lobby"), toolSpec("plain"))
		for _, class := range []principal.Class{principal.ClassTrusted, principal.ClassPublic} {
			if _, err := interpreter.StartConfigured(t.Context(), "plain", nil, "discord:111", class, ""); !hasCode(err, ErrorCodeNotPermitted) {
				t.Fatalf("%s start of an owner persona = %v, want %s", class, err, ErrorCodeNotPermitted)
			}
		}
		if got := countRuns(t, disk); got != 0 {
			t.Fatalf("%d run rows written, want none", got)
		}
		if _, err := interpreter.StartConfigured(t.Context(), "front", nil, "discord:111", principal.ClassPublic, ""); err != nil {
			t.Fatalf("public start of a public persona = %v, want nil", err)
		}
	})
	t.Run("a persona missing from the registry is refused", func(t *testing.T) {
		interpreter, _, disk := driverTestSetup(t, &fakeToolRunner{}, DriverConcurrent)
		generated := NormalizeProposal(toolSpec("x"), "prop_ghost")
		approveAs(t, interpreter, disk, generated, "ghost")
		if _, err := interpreter.StartConfigured(t.Context(), generated.ID, nil, principal.OwnerID, principal.ClassOwner, ""); !hasCode(err, ErrorCodePersonaMissing) {
			t.Fatalf("StartConfigured = %v, want %s", err, ErrorCodePersonaMissing)
		}
	})
	t.Run("Compile checks run_as", func(t *testing.T) {
		restricted := testValidationDeps()
		restricted.StepAllowed = personaTools(map[string][]string{"lobby": {"exec"}})
		withoutAgents := testValidationDeps()
		withoutAgents.Agents = nil
		tests := []struct {
			name     string
			spec     *Spec
			deps     *ValidationDeps
			wantCode ErrorCode
			wantText string
		}{
			{name: "an unknown persona", spec: installedRunAs("a", "ghost"), deps: testValidationDeps(), wantCode: ErrorCodeSpecInvalid, wantText: "run_as"},
			{name: "no resolver", spec: installedRunAs("a", "lobby"), deps: withoutAgents, wantCode: ErrorCodeSpecInvalid, wantText: "run_as"},
			{name: "a step outside the persona's ceiling", spec: installedRunAs("a", "lobby"), deps: restricted, wantCode: ErrorCodeCapabilityExceeded, wantText: `"read"`},
			{name: "a step inside the persona's ceiling", spec: installedRunAs("a", "lobby"), deps: testValidationDeps()},
		}
		for _, tc := range tests {
			t.Run(tc.name, func(t *testing.T) {
				_, err := Compile(tc.spec, tc.deps)
				if tc.wantCode == "" {
					if err != nil {
						t.Fatalf("Compile = %v, want nil", err)
					}
					return
				}
				if !hasCode(err, tc.wantCode) || !strings.Contains(err.Error(), tc.wantText) {
					t.Fatalf("Compile = %v, want %s mentioning %s", err, tc.wantCode, tc.wantText)
				}
			})
		}
		restrictedInstall := freshInterpreter(t)
		if err := restrictedInstall.Install(t.Context(), []*Spec{installedRunAs("a", "lobby")}, restricted); !hasCode(err, ErrorCodeCapabilityExceeded) {
			t.Fatalf("Install = %v, want %s", err, ErrorCodeCapabilityExceeded)
		}
	})
	t.Run("a proposal cannot set run_as", func(t *testing.T) {
		interpreter := freshInterpreter(t)
		if err := interpreter.LoadAll(t.Context(), nil, testValidationDeps()); err != nil {
			t.Fatal(err)
		}
		proposed := installedRunAs("x", "lobby")
		if _, err := interpreter.CompileProposal(NormalizeProposal(proposed, "prop_runas"), "main"); !hasCode(err, ErrorCodeSpecInvalid) {
			t.Fatalf("CompileProposal = %v, want %s", err, ErrorCodeSpecInvalid)
		}
	})
	t.Run("a child run keeps the parent's persona", func(t *testing.T) {
		interpreter, _, disk := driverTestSetup(t, &fakeToolRunner{}, DriverConcurrent)
		installAll(t, interpreter, installedRunAs("leaf", "lobby"), composingSpec("top", "leaf"))
		summary, err := interpreter.StartConfigured(t.Context(), "top", nil, principal.OwnerID, principal.ClassOwner, "")
		if err != nil {
			t.Fatal(err)
		}
		waitForRunStatus(t, disk, summary.ID, RunSucceeded, 5*time.Second)
		children := childRuns(t, disk, "leaf")
		if len(children) != 1 {
			t.Fatalf("child runs = %d, want 1", len(children))
		}
		child, err := disk.Run(t.Context(), children[0].ID)
		if err != nil || child.Origin.AgentID != "main" {
			t.Fatalf("child run = %+v, %v, want persona main", child, err)
		}
	})
	t.Run("run_as leaves the digest of a file without it unchanged", func(t *testing.T) {
		plain := toolSpec("plain")
		encoded, err := json.Marshal(plain)
		if err != nil || strings.Contains(string(encoded), "run_as") {
			t.Fatalf("spec without run_as encodes as %s, %v", encoded, err)
		}
	})
}

func TestTheCeilingChecksFailClosedWithoutTheirDependencies(t *testing.T) {
	registry := ceilingTestRegistry(t)
	t.Run("a tool step without a ceiling predicate is refused", func(t *testing.T) {
		err := CheckCeiling(ceilingSpec("read_file"), "open", principal.ClassOwner, &ValidationDeps{Agents: registry})
		if code, _ := CodeOf(err); code != ErrorCodeCapabilityExceeded {
			t.Fatalf("CheckCeiling() = %v, want %s", err, ErrorCodeCapabilityExceeded)
		}
	})
	t.Run("a start without a persona registry is refused", func(t *testing.T) {
		err := checkPersona(&ValidationDeps{StepAllowed: registryStepAllowed(registry)}, "open", principal.ClassOwner)
		if code, _ := CodeOf(err); code != ErrorCodePersonaMissing {
			t.Fatalf("checkPersona() = %v, want %s", err, ErrorCodePersonaMissing)
		}
	})
}
