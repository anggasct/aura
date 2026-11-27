package workflow

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/anggasct/aura/internal/platform/fault"
	"strings"
	"testing"
	"time"

	auraagent "github.com/anggasct/aura/internal/platform/agent"
	"github.com/anggasct/aura/internal/platform/durable"
	"github.com/anggasct/aura/internal/platform/principal"
	"github.com/anggasct/aura/internal/platform/tools"
)

func ceilingTestCatalog() []tools.Definition {
	return []tools.Definition{
		{Name: "read_file", Version: tools.Version},
		{Name: "write_file", Version: tools.Version, RequiresApproval: true, Effectful: true},
		{Name: "exec", Version: tools.Version, RequiresApproval: true, Effectful: true},
		{Name: "list_dir", Version: tools.Version},
		{Name: "web_fetch", Version: tools.Version, ExternalSink: true},
		{Name: "web_search", Version: tools.Version, ExternalSink: true},
		{Name: "github_comment", Version: tools.Version, OptIn: true, Effectful: true},
	}
}

func registryStepAllowed(registry *auraagent.Registry, extra ...tools.Definition) func(string, principal.Class, string) (bool, bool) {
	catalog := map[string]*tools.Definition{}
	builtins := ceilingTestCatalog()
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
	overrides := []auraagent.Persona{
		{ID: "open", Description: "no list", Instructions: "work"},
		{ID: "lister", Description: "lists one", Instructions: "work", Tools: []string{"read_file"}},
		{ID: "gh", Description: "lists github", Instructions: "work", Tools: []string{"github_comment"}},
	}
	known := []string{"read_file", "write_file", "exec", "list_dir", "web_fetch", "web_search", "github_comment"}
	registry, err := auraagent.Build(overrides, known, ceilingTestCatalog(), []string{"primary"}, "en")
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
		if code, _ := fault.CodeOf[ErrorCode](err); code != ErrorCodeCapabilityExceeded {
			t.Fatalf("CheckCeiling() = %v, want %s", err, ErrorCodeCapabilityExceeded)
		}
	})
	t.Run("a start without a persona registry is refused", func(t *testing.T) {
		err := checkPersona(&ValidationDeps{StepAllowed: registryStepAllowed(registry)}, "open", principal.ClassOwner)
		if code, _ := fault.CodeOf[ErrorCode](err); code != ErrorCodePersonaMissing {
			t.Fatalf("checkPersona() = %v, want %s", err, ErrorCodePersonaMissing)
		}
	})
}

func agentCeilingRegistry(t *testing.T) *auraagent.Registry {
	t.Helper()
	listing := []string{"read_file", "web_fetch"}
	overrides := []auraagent.Persona{
		{ID: "gate", Description: "lists both", Instructions: "work", Audience: auraagent.AudiencePublic, Tools: listing, PrincipalTools: listing, PublicTools: listing},
		{ID: "reader", Description: "lists one", Instructions: "work", Audience: auraagent.AudiencePublic, Tools: []string{"read_file"}, PrincipalTools: []string{"read_file"}, PublicTools: []string{"read_file"}},
		{ID: "inner", Description: "owner only", Instructions: "work", Audience: auraagent.AudienceOwner, Tools: listing},
	}
	known := []string{"read_file", "write_file", "exec", "list_dir", "web_fetch", "web_search", "github_comment"}
	registry, err := auraagent.Build(overrides, known, ceilingTestCatalog(), []string{"primary"}, "en")
	if err != nil {
		t.Fatal(err)
	}
	return registry
}

func agentCeilingSpec(stepPersona string, toolIDs ...string) *Spec {
	return &Spec{ID: "gen_agent_ceiling", Goal: "Check the ceiling", Version: 1, Source: SourceGenerated, Steps: []StepSpec{
		{ID: "think", Executor: ExecutorSpec{Kind: KindAgent, AgentID: &stepPersona, Prompt: "Think.", Tools: toolIDs}, Timeout: time.Minute},
	}}
}

func TestCheckCeilingAgentSteps(t *testing.T) {
	registry := agentCeilingRegistry(t)
	deps := &ValidationDeps{Agents: registry, StepAllowed: registryStepAllowed(registry)}
	cases := []struct {
		name        string
		runPersona  string
		stepPersona string
		tools       []string
		classes     []principal.Class
		wantCode    ErrorCode
	}{
		{name: "both personas allow the tool", runPersona: "gate", stepPersona: "gate", tools: []string{"web_fetch"}, classes: allClasses()},
		{name: "a tool-free step needs only the persona", runPersona: "reader", stepPersona: "gate", classes: allClasses()},
		{name: "the run persona does not allow the tool", runPersona: "reader", stepPersona: "gate", tools: []string{"web_fetch"}, classes: allClasses(), wantCode: ErrorCodeCapabilityExceeded},
		{name: "the step persona does not allow the tool", runPersona: "gate", stepPersona: "reader", tools: []string{"web_fetch"}, classes: allClasses(), wantCode: ErrorCodeCapabilityExceeded},
		{name: "the step persona does not admit the class", runPersona: "gate", stepPersona: "inner", tools: []string{"web_fetch"}, classes: []principal.Class{principal.ClassTrusted, principal.ClassPublic}, wantCode: ErrorCodeNotPermitted},
		{name: "a tool-free step still needs a persona that admits the class", runPersona: "gate", stepPersona: "inner", classes: []principal.Class{principal.ClassTrusted, principal.ClassPublic}, wantCode: ErrorCodeNotPermitted},
		{name: "an owner-only step persona serves the owner", runPersona: "gate", stepPersona: "inner", tools: []string{"web_fetch"}, classes: []principal.Class{principal.ClassOwner}},
		{name: "an unknown step persona", runPersona: "gate", stepPersona: "ghost", tools: []string{"web_fetch"}, classes: allClasses(), wantCode: ErrorCodePersonaMissing},
		{name: "an unknown run persona", runPersona: "ghost", stepPersona: "gate", tools: []string{"web_fetch"}, classes: allClasses(), wantCode: ErrorCodePersonaMissing},
	}
	for _, tc := range cases {
		for _, class := range tc.classes {
			t.Run(tc.name+" for "+string(class), func(t *testing.T) {
				err := CheckCeiling(agentCeilingSpec(tc.stepPersona, tc.tools...), tc.runPersona, class, deps)
				if tc.wantCode == "" {
					if err != nil {
						t.Fatalf("CheckCeiling = %v, want nil", err)
					}
					return
				}
				if !hasCode(err, tc.wantCode) {
					t.Fatalf("CheckCeiling = %v, want %s", err, tc.wantCode)
				}
			})
		}
	}
	t.Run("a step with tools and no ceiling predicate is refused", func(t *testing.T) {
		err := CheckCeiling(agentCeilingSpec("gate", "web_fetch"), "gate", principal.ClassOwner, &ValidationDeps{Agents: registry})
		if !hasCode(err, ErrorCodeCapabilityExceeded) {
			t.Fatalf("CheckCeiling = %v, want %s", err, ErrorCodeCapabilityExceeded)
		}
	})
	t.Run("a step without a persona registry is refused", func(t *testing.T) {
		err := CheckCeiling(agentCeilingSpec("gate"), "gate", principal.ClassOwner, &ValidationDeps{StepAllowed: registryStepAllowed(registry)})
		if !hasCode(err, ErrorCodePersonaMissing) {
			t.Fatalf("CheckCeiling = %v, want %s", err, ErrorCodePersonaMissing)
		}
	})
}

func allClasses() []principal.Class {
	return []principal.Class{principal.ClassOwner, principal.ClassTrusted, principal.ClassPublic}
}

func agentLeafSpec(id, persona string, toolIDs ...string) *Spec {
	spec := agentCeilingSpec(persona, toolIDs...)
	spec.ID = id
	spec.Source = SourceDefined
	return spec
}

func TestAdmitRunChecksComposedAgentSteps(t *testing.T) {
	runner := &fakeToolRunner{}
	interpreter, _, disk := driverTestSetup(t, runner, DriverConcurrent)
	deps := testValidationDeps()
	deps.StepAllowed = personaTools(map[string][]string{"main": {"web_fetch"}, "engineer": {"read_file"}})
	if err := interpreter.Install(t.Context(), []*Spec{agentLeafSpec("leaf", "engineer", "web_fetch"), composingSpec("top", "leaf")}, deps); err != nil {
		t.Fatalf("Install: %v", err)
	}
	for _, id := range []string{"leaf", "top"} {
		_, err := interpreter.Start(t.Context(), id, nil, ownerOrigin())
		if !hasCode(err, ErrorCodeCapabilityExceeded) || !strings.Contains(err.Error(), `"think"`) {
			t.Fatalf("Start(%s) = %v, want %s naming the agent step", id, err, ErrorCodeCapabilityExceeded)
		}
	}
	if got := countRuns(t, disk); got != 0 {
		t.Fatalf("%d run rows written, want none", got)
	}
	if got := countRunSessions(t, disk); got != 0 {
		t.Fatalf("%d run sessions written, want none", got)
	}
}

func TestCompileProposalChecksAgentSteps(t *testing.T) {
	registry := agentCeilingRegistry(t)
	cases := []struct {
		name        string
		persona     string
		stepPersona string
		tools       []string
		wantCode    ErrorCode
	}{
		{name: "both personas allow the tool", persona: "gate", stepPersona: "gate", tools: []string{"web_fetch"}},
		{name: "the proposing persona does not allow the tool", persona: "reader", stepPersona: "gate", tools: []string{"web_fetch"}, wantCode: ErrorCodeCapabilityExceeded},
		{name: "the step persona does not allow the tool", persona: "gate", stepPersona: "reader", tools: []string{"web_fetch"}, wantCode: ErrorCodeCapabilityExceeded},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			deps := testValidationDeps()
			deps.Agents = registry
			deps.StepAllowed = registryStepAllowed(registry)
			interpreter := freshInterpreter(t)
			if err := interpreter.LoadAll(t.Context(), nil, deps); err != nil {
				t.Fatalf("LoadAll: %v", err)
			}
			spec := NormalizeProposal(agentCeilingSpec(tc.stepPersona, tc.tools...), "prop_agent")
			_, err := interpreter.CompileProposal(spec, tc.persona)
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
}

func TestRunAsLoadChecksAgentSteps(t *testing.T) {
	registry := agentCeilingRegistry(t)
	cases := []struct {
		name        string
		runAs       string
		stepPersona string
		wantCode    ErrorCode
	}{
		{name: "both personas allow the tool", runAs: "gate", stepPersona: "gate"},
		{name: "the run_as persona does not allow the tool", runAs: "reader", stepPersona: "gate", wantCode: ErrorCodeCapabilityExceeded},
		{name: "the step persona does not allow the tool", runAs: "gate", stepPersona: "reader", wantCode: ErrorCodeCapabilityExceeded},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			deps := testValidationDeps()
			deps.Agents = registry
			deps.StepAllowed = registryStepAllowed(registry)
			spec := agentLeafSpec("declared", tc.stepPersona, "web_fetch")
			spec.RunAs = tc.runAs
			_, err := Compile(spec, deps)
			installErr := freshInterpreter(t).Install(t.Context(), []*Spec{agentLeafSpec("declared", tc.stepPersona, "web_fetch")}, deps)
			runAsInstall := agentLeafSpec("declared", tc.stepPersona, "web_fetch")
			runAsInstall.RunAs = tc.runAs
			loadErr := freshInterpreter(t).Install(t.Context(), []*Spec{runAsInstall}, deps)
			if tc.wantCode == "" {
				if err != nil || installErr != nil || loadErr != nil {
					t.Fatalf("Compile = %v, Install = %v, Install with run_as = %v, want nil", err, installErr, loadErr)
				}
				return
			}
			if !hasCode(err, tc.wantCode) || !hasCode(loadErr, tc.wantCode) {
				t.Fatalf("Compile = %v, Install with run_as = %v, want %s", err, loadErr, tc.wantCode)
			}
			if installErr != nil {
				t.Fatalf("Install without run_as = %v, want it unchecked until a start", installErr)
			}
		})
	}
}

func TestTrustedOriginRunRunsAgentStepWithinBothCeilings(t *testing.T) {
	interpreter, _, disk := driverTestSetup(t, &fakeToolRunner{}, DriverConcurrent)
	deps := testValidationDeps()
	deps.StepAllowed = personaTools(map[string][]string{"lobby": {"read_file"}, "engineer": {"read_file"}})
	spec := agentLeafSpec("lookup", "lobby", "read_file")
	if err := interpreter.Install(t.Context(), []*Spec{spec}, deps); err != nil {
		t.Fatalf("Install: %v", err)
	}
	origin := &RunOrigin{PrincipalID: "discord:111", PrincipalClass: "trusted", AgentID: "lobby"}
	summary, err := interpreter.Start(t.Context(), "lookup", nil, origin)
	if err != nil {
		t.Fatalf("Start = %v, want a trusted run within both ceilings to start", err)
	}
	waitForRunStatus(t, disk, summary.ID, RunSucceeded, 5*time.Second)
	if err := interpreter.Install(t.Context(), []*Spec{agentLeafSpec("wide", "lobby", "web_fetch")}, deps); err != nil {
		t.Fatalf("Install wide: %v", err)
	}
	if _, err := interpreter.Start(t.Context(), "wide", nil, origin); !hasCode(err, ErrorCodeCapabilityExceeded) {
		t.Fatalf("Start with a tool outside both ceilings = %v, want %s", err, ErrorCodeCapabilityExceeded)
	}
	if err := interpreter.Install(t.Context(), []*Spec{agentLeafSpec("owner_only", "engineer", "read_file")}, deps); err != nil {
		t.Fatalf("Install owner_only: %v", err)
	}
	if _, err := interpreter.Start(t.Context(), "owner_only", nil, origin); !hasCode(err, ErrorCodeNotPermitted) {
		t.Fatalf("Start with a step persona that does not serve the class = %v, want %s", err, ErrorCodeNotPermitted)
	}
}
