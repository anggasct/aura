package runtimeadk

import (
	"encoding/json"
	"slices"
	"testing"

	auraagent "github.com/anggasct/aura/internal/platform/agent"
	"github.com/anggasct/aura/internal/platform/fault"
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

func executorWithOperationTool(t *testing.T) (*ADKExecutor, *OperationPort) {
	t.Helper()
	port := NewOperationPort()
	executor := &ADKExecutor{}
	if err := WithBuiltinToolExecutor(executorBuiltins())(executor); err != nil {
		t.Fatalf("WithBuiltinToolExecutor: %v", err)
	}
	if err := WithOperations(port)(executor); err != nil {
		t.Fatalf("WithOperations: %v", err)
	}
	return executor, port
}

type quietSurface struct{}

func (quietSurface) Describe(*operation.DescriptionInput) (string, error) { return "", nil }

func ownerAudience(tools ...string) *auraagent.Definition {
	return &auraagent.Definition{Audience: auraagent.AudienceOwner, Tools: tools}
}

func TestToolsForOffersOperationToolsOnlyToOwnerAudiencePersonasThatListThem(t *testing.T) {
	executor, port := executorWithOperationTool(t)
	if err := port.Bind(quietSurface{}, executorBuiltins()); err != nil {
		t.Fatalf("Bind: %v", err)
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
			for _, name := range toolNames(executor.toolsFor(tc.definition, tc.class, tc.origin, &turnContext{})) {
				if name == "aura_cron_list" || name == "aura_cron_add" {
					t.Fatalf("the operation tool %q is offered", name)
				}
			}
		})
	}
	t.Run("an owner persona is offered the operation tools it lists by name", func(t *testing.T) {
		got := toolNames(executor.toolsFor(ownerAudience("read_file", "aura_cron_list", "aura_cron_add"), principal.ClassOwner, "discord:main", &turnContext{}))
		want := []string{"aura_cron_add", "aura_cron_list", "read_file"}
		if !slices.Equal(sortedCopy(got), want) {
			t.Fatalf("tools = %v, want %v", got, want)
		}
	})
	t.Run("a tool restriction removes them", func(t *testing.T) {
		got := toolNames(executor.toolsFor(ownerAudience("read_file", "aura_cron_list"), principal.ClassOwner, turn.OriginTerminal, &turnContext{restriction: &turn.ToolRestriction{Allowed: []string{"read_file"}}}))
		if !slices.Equal(got, []string{"read_file"}) {
			t.Fatalf("tools = %v, want only read_file", got)
		}
	})
	t.Run("an executor whose operations were never bound offers none", func(t *testing.T) {
		unbound, _ := executorWithOperationTool(t)
		got := toolNames(unbound.toolsFor(ownerAudience("read_file", "aura_cron_list"), principal.ClassOwner, turn.OriginTerminal, &turnContext{}))
		if !slices.Equal(got, []string{"read_file"}) {
			t.Fatalf("tools = %v, want only read_file", got)
		}
	})
	t.Run("the other tools stay offered", func(t *testing.T) {
		got := toolNames(executor.toolsFor(&auraagent.Definition{}, principal.ClassOwner, turn.OriginTerminal, &turnContext{}))
		if !slices.Contains(got, "read_file") {
			t.Fatalf("tools = %v, want read_file", got)
		}
	})
}

func TestOperationToolsComeOnlyFromTheBoundPort(t *testing.T) {
	executor, port := executorWithOperationTool(t)
	if got := namesOf(executor.tools); !slices.Equal(got, []string{"read_file"}) {
		t.Fatalf("tools = %v, want the operation definitions kept out of the construction snapshot", got)
	}
	if executor.isOperationTool("aura_cron_list") {
		t.Fatal("an operation tool is known before the port is bound")
	}
	if err := port.Bind(quietSurface{}, executorBuiltins()); err != nil {
		t.Fatalf("Bind: %v", err)
	}
	if !executor.isOperationTool("aura_cron_list") || !executor.generatesOperation("aura_cron_add") {
		t.Fatal("the bound operation tools are not known to the executor")
	}
	if got := namesOf(executor.tools); !slices.Equal(got, []string{"read_file"}) {
		t.Fatalf("tools = %v, want the snapshot unchanged by the binding", got)
	}
}

func TestOperationPortIsFilledOnce(t *testing.T) {
	port := NewOperationPort()
	if err := port.Bind(nil, executorBuiltins()); err == nil {
		t.Error("Bind without a surface succeeded")
	}
	if err := port.Bind(quietSurface{}, nil); err == nil {
		t.Error("Bind without a builtin tool executor succeeded")
	}
	schema := json.RawMessage(`{"type":"object"}`)
	legacyOnly := &fakeBuiltinExecutor{definitions: []BuiltinToolDefinition{{Name: "read_file", Version: "v1", Schema: schema}}}
	if err := port.Bind(quietSurface{}, legacyOnly); err == nil {
		t.Error("Bind over an executor with no operation tool succeeded")
	}
	if err := port.Bind(quietSurface{}, executorBuiltins()); err != nil {
		t.Fatalf("the first Bind: %v", err)
	}
	if err := port.Bind(quietSurface{}, executorBuiltins()); !fault.HasCode(err, turn.ErrorCodeInvalidArgument) {
		t.Fatalf("the second Bind = %v, want invalid_argument", err)
	}
	if err := (*OperationPort)(nil).Bind(quietSurface{}, executorBuiltins()); err == nil {
		t.Error("Bind on a nil port succeeded")
	}
	if err := WithOperations(nil)(&ADKExecutor{}); err == nil {
		t.Error("WithOperations(nil) succeeded")
	}
}

func TestAnExecutorWithoutLegacyToolsStillStartsAndServesOperations(t *testing.T) {
	port := NewOperationPort()
	executor := &ADKExecutor{}
	only := &fakeBuiltinExecutor{definitions: []BuiltinToolDefinition{{Name: "aura_cron_list", Version: "v1", Schema: json.RawMessage(`{"type":"object"}`), Operation: "cron.list"}}}
	if err := WithBuiltinToolExecutor(only)(executor); err != nil {
		t.Fatalf("WithBuiltinToolExecutor over operation tools only: %v", err)
	}
	if err := WithOperations(port)(executor); err != nil {
		t.Fatalf("WithOperations: %v", err)
	}
	if err := port.Bind(quietSurface{}, only); err != nil {
		t.Fatalf("Bind: %v", err)
	}
	if executor.builtinsFor("aura_cron_list") == nil {
		t.Fatal("the bound operation tool has no executor")
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
