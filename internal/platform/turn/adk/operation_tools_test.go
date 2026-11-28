package runtimeadk

import (
	"encoding/json"
	"slices"
	"testing"

	auraagent "github.com/anggasct/aura/internal/platform/agent"
	"github.com/anggasct/aura/internal/platform/operation"
	"github.com/anggasct/aura/internal/platform/operation/operationtest"
	"github.com/anggasct/aura/internal/platform/principal"
	"github.com/anggasct/aura/internal/platform/turn"
)

func executorBuiltins() *fakeBuiltinExecutor {
	schema := json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`)
	return &fakeBuiltinExecutor{definitions: []BuiltinToolDefinition{
		{Name: "read_file", Version: "v1", Description: "read", Schema: schema},
		{Name: "aura_cron_list", Version: "v1", Description: "list jobs", Schema: schema, Operation: "cron.list", OptIn: true, OwnerOptIn: true},
		{Name: "aura_cron_add", Version: "v1", Description: "add a job", Schema: schema, Operation: "cron.add", OptIn: true, OwnerOptIn: true},
	}}
}

func executorWithOperationTool(t *testing.T) *ADKExecutor {
	t.Helper()
	executor := &ADKExecutor{}
	if err := WithBuiltinToolExecutor(executorBuiltins())(executor); err != nil {
		t.Fatalf("WithBuiltinToolExecutor: %v", err)
	}
	return executor
}

type quietSurface struct{}

func (quietSurface) Describe(*operation.DescriptionInput) (string, error) { return "", nil }

func ownerAudience(tools ...string) *auraagent.Definition {
	return &auraagent.Definition{Audience: auraagent.AudienceOwner, Tools: tools}
}

func TestToolsForOffersOperationToolsOnlyToOwnerAudiencePersonasThatListThem(t *testing.T) {
	executor := executorWithOperationTool(t)
	if err := executor.BindOperations(quietSurface{}); err != nil {
		t.Fatalf("BindOperations: %v", err)
	}
	trustedPersona := ownerAudience("read_file", "aura_cron_list")
	trustedPersona.Audience = auraagent.AudienceTrusted
	none := []struct {
		name       string
		definition *auraagent.Definition
		class      principal.Class
		origin     turn.Origin
	}{
		{name: "an owner persona without a tool list", definition: ownerAudience(), class: principal.ClassOwner, origin: turn.OriginTerminal},
		{name: "an owner persona that lists no operation tool", definition: ownerAudience("read_file"), class: principal.ClassOwner, origin: turn.OriginTerminal},
		{name: "a turn with no principal class", definition: ownerAudience("read_file", "aura_cron_list"), origin: turn.OriginTerminal},
		{name: "a trusted principal on an owner-audience persona", definition: &auraagent.Definition{Audience: auraagent.AudienceOwner, Tools: []string{"read_file", "aura_cron_list"}, PrincipalTools: []string{"read_file", "aura_cron_list"}}, class: principal.ClassTrusted, origin: "discord:main"},
		{name: "a public principal on an owner-audience persona", definition: &auraagent.Definition{Audience: auraagent.AudienceOwner, Tools: []string{"read_file", "aura_cron_list"}, PublicTools: []string{"read_file", "aura_cron_list"}}, class: principal.ClassPublic, origin: "discord:main"},
		{name: "a persona whose audience is not owner", definition: trustedPersona, class: principal.ClassOwner, origin: turn.OriginTerminal},
		{name: "a child turn", definition: ownerAudience("read_file", "aura_cron_list"), class: principal.ClassOwner, origin: turn.OriginChild},
		{name: "a workflow turn", definition: ownerAudience("read_file", "aura_cron_list"), class: principal.ClassOwner, origin: turn.OriginWorkflow},
		{name: "a turn with no origin", definition: ownerAudience("read_file", "aura_cron_list"), class: principal.ClassOwner},
	}
	for _, tc := range none {
		t.Run(tc.name+" is offered no operation tool", func(t *testing.T) {
			for _, name := range toolNames(executor.toolsFor(tc.definition, tc.class, tc.origin, turnContext{})) {
				if name == "aura_cron_list" || name == "aura_cron_add" {
					t.Fatalf("the operation tool %q is offered", name)
				}
			}
		})
	}
	t.Run("an owner persona is offered the operation tools it lists by name", func(t *testing.T) {
		got := toolNames(executor.toolsFor(ownerAudience("read_file", "aura_cron_list", "aura_cron_add"), principal.ClassOwner, "discord:main", turnContext{}))
		want := []string{"aura_cron_add", "aura_cron_list", "read_file"}
		if !slices.Equal(sortedCopy(got), want) {
			t.Fatalf("tools = %v, want %v", got, want)
		}
	})
	t.Run("a tool restriction removes them", func(t *testing.T) {
		got := toolNames(executor.toolsFor(ownerAudience("read_file", "aura_cron_list"), principal.ClassOwner, turn.OriginTerminal, turnContext{restriction: &turn.ToolRestriction{Allowed: []string{"read_file"}}}))
		if !slices.Equal(got, []string{"read_file"}) {
			t.Fatalf("tools = %v, want only read_file", got)
		}
	})
	t.Run("an executor whose operations were never bound offers none", func(t *testing.T) {
		unbound := executorWithOperationTool(t)
		got := toolNames(unbound.toolsFor(ownerAudience("read_file", "aura_cron_list"), principal.ClassOwner, turn.OriginTerminal, turnContext{}))
		if !slices.Equal(got, []string{"read_file"}) {
			t.Fatalf("tools = %v, want only read_file", got)
		}
	})
	t.Run("the other tools stay offered", func(t *testing.T) {
		got := toolNames(executor.toolsFor(&auraagent.Definition{}, principal.ClassOwner, turn.OriginTerminal, turnContext{}))
		if !slices.Contains(got, "read_file") {
			t.Fatalf("tools = %v, want read_file", got)
		}
	})
}

type growingBuiltins struct {
	fakeBuiltinExecutor
	grown bool
}

func (g *growingBuiltins) Definitions() []BuiltinToolDefinition {
	definitions := g.fakeBuiltinExecutor.Definitions()
	if !g.grown {
		return definitions[:1]
	}
	return definitions
}

func TestBindOperationsRebuildsTheToolSnapshot(t *testing.T) {
	builtin := &growingBuiltins{fakeBuiltinExecutor: *executorBuiltins()}
	executor := &ADKExecutor{}
	if err := WithBuiltinToolExecutor(builtin)(executor); err != nil {
		t.Fatalf("WithBuiltinToolExecutor: %v", err)
	}
	if got := toolNames(executor.tools); slices.Contains(got, "aura_cron_list") {
		t.Fatalf("tools = %v, want the snapshot taken before the operations exist", got)
	}
	builtin.grown = true
	if err := executor.BindOperations(quietSurface{}); err != nil {
		t.Fatalf("BindOperations: %v", err)
	}
	if got := toolNames(executor.tools); !slices.Contains(got, "aura_cron_list") || !executor.operationTools["aura_cron_list"] {
		t.Fatalf("tools = %v, operations = %v, want the operation tools after the rebuild", got, executor.operationTools)
	}
}

func TestBrokerExecutorDefinitionsCarryTheOperation(t *testing.T) {
	rig := operationtest.NewRig(t, nil)
	executor, err := NewBrokerExecutor(rig.Broker, nil)
	if err != nil {
		t.Fatalf("NewBrokerExecutor: %v", err)
	}
	operations := map[string]string{}
	for _, definition := range executor.Definitions() {
		operations[definition.Name] = definition.Operation
	}
	if operations[operationtest.ToolOf(operationtest.Change)] != operationtest.Change {
		t.Fatalf("operations by tool = %v, want %s on %s", operations, operationtest.Change, operationtest.ToolOf(operationtest.Change))
	}
}
