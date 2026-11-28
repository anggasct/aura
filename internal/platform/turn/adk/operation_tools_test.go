package runtimeadk

import (
	"encoding/json"
	"slices"
	"testing"

	auraagent "github.com/anggasct/aura/internal/platform/agent"
	"github.com/anggasct/aura/internal/platform/operation/operationtest"
	"github.com/anggasct/aura/internal/platform/principal"
	"github.com/anggasct/aura/internal/platform/turn"
)

func executorWithOperationTool(t *testing.T) *ADKExecutor {
	t.Helper()
	schema := json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`)
	builtin := &fakeBuiltinExecutor{definitions: []BuiltinToolDefinition{
		{Name: "read_file", Version: "v1", Description: "read", Schema: schema},
		{Name: "aura_cron_list", Version: "v1", Description: "list jobs", Schema: schema, Operation: "cron.list"},
		{Name: "aura_cron_add", Version: "v1", Description: "add a job", Schema: schema, Operation: "cron.add", OptIn: true, OwnerOptIn: true},
	}}
	executor := &ADKExecutor{}
	if err := WithBuiltinToolExecutor(builtin)(executor); err != nil {
		t.Fatalf("WithBuiltinToolExecutor: %v", err)
	}
	return executor
}

func TestToolsForOffersNoOperationTool(t *testing.T) {
	executor := executorWithOperationTool(t)
	cases := []struct {
		name       string
		definition *auraagent.Definition
		class      principal.Class
		origin     turn.Origin
	}{
		{name: "an owner persona without a tool list", definition: &auraagent.Definition{}, class: principal.ClassOwner, origin: turn.OriginTerminal},
		{name: "an owner persona listing the operation tools", definition: &auraagent.Definition{Tools: []string{"read_file", "aura_cron_list", "aura_cron_add"}}, class: principal.ClassOwner, origin: "discord:main"},
		{name: "a turn with no principal class", definition: &auraagent.Definition{Tools: []string{"read_file", "aura_cron_list"}}, origin: turn.OriginTerminal},
		{name: "a trusted principal on an owner-listed persona", definition: &auraagent.Definition{Tools: []string{"read_file", "aura_cron_list"}, PrincipalTools: []string{"read_file", "aura_cron_list"}}, class: principal.ClassTrusted, origin: "discord:main"},
		{name: "a child turn", definition: &auraagent.Definition{Tools: []string{"read_file", "aura_cron_list"}}, class: principal.ClassOwner, origin: turn.OriginChild},
		{name: "a workflow turn", definition: &auraagent.Definition{Tools: []string{"read_file", "aura_cron_list"}}, class: principal.ClassOwner, origin: turn.OriginWorkflow},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for _, name := range toolNames(executor.toolsFor(tc.definition, tc.class, tc.origin, turnContext{})) {
				if name == "aura_cron_list" || name == "aura_cron_add" {
					t.Fatalf("the operation tool %q is offered", name)
				}
			}
		})
	}
	t.Run("the other tools stay offered", func(t *testing.T) {
		got := toolNames(executor.toolsFor(&auraagent.Definition{}, principal.ClassOwner, turn.OriginTerminal, turnContext{}))
		if !slices.Contains(got, "read_file") {
			t.Fatalf("tools = %v, want read_file", got)
		}
	})
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
