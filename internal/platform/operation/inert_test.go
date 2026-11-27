package operation_test

import (
	"context"
	"encoding/json"
	"slices"
	"testing"

	"github.com/anggasct/aura/internal/platform/durable"
	"github.com/anggasct/aura/internal/platform/effect"
	"github.com/anggasct/aura/internal/platform/operation"
	"github.com/anggasct/aura/internal/platform/operation/operationtest"
	"github.com/anggasct/aura/internal/platform/toolbroker"
	"github.com/anggasct/aura/internal/platform/tools"
)

const legacyTool = "legacy_echo"

func legacyDefinition() tools.Definition {
	return tools.Definition{
		Name: legacyTool, Version: tools.Version, Effectful: true,
		Validator: func(raw json.RawMessage) (json.RawMessage, error) { return raw, nil },
	}
}

type legacyRun struct {
	toolNames []string
	actions   string
	providers []string
	outcome   operationtest.Outcome
}

func runLegacyCall(t *testing.T, options *operationtest.Options) legacyRun {
	t.Helper()
	db := operationtest.OpenDatabase(t)
	hosted := operationtest.StartHosted(t, db)
	options.DB = db
	definition := legacyDefinition()
	options.Tools = operationtest.LegacyTools(&definition)
	rig := operationtest.NewRig(t, options)
	request := operationtest.Request(t, legacyTool, "a")
	var run legacyRun
	key := hosted.Start(t, func(ctx context.Context, inv durable.Invocation) error {
		run.outcome = rig.Execute(operationtest.InTurn(ctx, inv), request)
		return nil
	})
	hosted.AwaitEnded(t, key, 1)
	definitions := rig.Broker.Definitions()
	for i := range definitions {
		run.toolNames = append(run.toolNames, definitions[i].Name)
	}
	slices.Sort(run.toolNames)
	run.actions = actionKeys(hosted.Actions(t, key))
	for _, state := range []effect.State{effect.StateSucceeded, effect.StateFailed, effect.StateUnknown, effect.StateStarted} {
		intents := intentsIn(t, rig, state)
		for i := range intents {
			run.providers = append(run.providers, intents[i].Provider+"/"+intents[i].Operation+"/"+string(state))
		}
	}
	return run
}

func TestEmptyRegistryLeavesLegacyToolsUnchanged(t *testing.T) {
	baseline := runLegacyCall(t, &operationtest.Options{WithoutRegistry: true})
	withRegistry := runLegacyCall(t, &operationtest.Options{Catalogs: []operation.Catalog{}, Bindings: func(*operationtest.Stubs) []operation.Binding { return nil }})

	if baseline.outcome.Class != toolbroker.ResultOK {
		t.Fatalf("baseline outcome = %+v, want ok", baseline.outcome)
	}
	if withRegistry.outcome != baseline.outcome {
		t.Errorf("outcome = %+v, want %+v", withRegistry.outcome, baseline.outcome)
	}
	if !slices.Equal(withRegistry.toolNames, baseline.toolNames) || !slices.Equal(baseline.toolNames, []string{legacyTool}) {
		t.Errorf("tools = %v, want %v and only the legacy tool", withRegistry.toolNames, baseline.toolNames)
	}
	if withRegistry.actions != baseline.actions {
		t.Errorf("journal actions = %s, want %s", withRegistry.actions, baseline.actions)
	}
	if !slices.Equal(withRegistry.providers, baseline.providers) {
		t.Errorf("effect intents = %v, want %v", withRegistry.providers, baseline.providers)
	}
}

func TestEmptyRegistryExposesNothing(t *testing.T) {
	rig := operationtest.NewRig(t, &operationtest.Options{Catalogs: []operation.Catalog{}, Bindings: func(*operationtest.Stubs) []operation.Binding { return nil }})

	if descriptors := rig.Registry.Descriptors(); len(descriptors) != 0 {
		t.Errorf("descriptors = %d, want none", len(descriptors))
	}
	if groups := rig.Registry.Groups(); len(groups) != 0 {
		t.Errorf("groups = %d, want none", len(groups))
	}
	if registrations := rig.Registry.Tools(); len(registrations) != 0 {
		t.Errorf("tools = %d, want none", len(registrations))
	}
	if definitions := rig.Broker.Definitions(); len(definitions) != 0 {
		t.Errorf("broker definitions = %d, want none", len(definitions))
	}
}
