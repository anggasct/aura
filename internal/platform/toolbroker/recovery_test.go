package toolbroker

import (
	"context"
	"testing"

	"github.com/anggasct/aura/internal/platform/effect"
	"github.com/anggasct/aura/internal/platform/fault"
)

func noRecovery(context.Context, *ToolRequest, *effect.Intent, *RecoveryRun) error { return nil }

func TestBrokerRegistersARecoveryOnlyForAnEffectfulOperation(t *testing.T) {
	registration := func(stub operationStub) ToolRegistration {
		definition := stub.definition(t)
		built := ToolRegistration{Definition: definition, Adapter: unusedAdapter, Rule: DefaultRule(&definition), Recovery: noRecovery}
		if needsPreview(&definition) {
			built.Preview = defaultPreview
		}
		return built
	}
	cases := []struct {
		name     string
		build    ToolRegistration
		accepted bool
	}{
		{name: "a change operation", build: registration(changeStub), accepted: true},
		{name: "a destructive operation", build: registration(destructiveStub), accepted: true},
		{name: "a read operation", build: registration(readStub)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			broker := newProbeBroker(t)
			err := broker.RegisterProvider(stubOperations{tc.build})
			if tc.accepted {
				if err != nil {
					t.Fatalf("RegisterProvider = %v, want accepted", err)
				}
				if broker.recoveryFor(&tc.build.Definition) == nil {
					t.Error("the recovery was not stored")
				}
				return
			}
			if !fault.HasCode(err, ResultInvalidArgument) {
				t.Fatalf("RegisterProvider = %v, want %s", err, ResultInvalidArgument)
			}
			if broker.recoveryFor(&tc.build.Definition) != nil {
				t.Error("a rejected registration kept its recovery")
			}
		})
	}
	t.Run("a legacy tool", func(t *testing.T) {
		broker := newProbeBroker(t)
		legacy := stubOperations{{Definition: readStub.definition(t), Adapter: unusedAdapter, Recovery: noRecovery}}
		legacy[0].Definition.Operation, legacy[0].Definition.Risk = "", ""
		legacy[0].Rule = DefaultRule(&legacy[0].Definition)
		if err := broker.RegisterProvider(legacy); err == nil {
			t.Fatal("RegisterProvider accepted a recovery for a legacy tool")
		}
	})
}

func TestBrokerDropsTheRecoveryWithItsTool(t *testing.T) {
	broker := newProbeBroker(t)
	definition := changeStub.definition(t)
	registration := ToolRegistration{Definition: definition, Adapter: unusedAdapter, Rule: DefaultRule(&definition), Recovery: noRecovery}
	if err := broker.RegisterProvider(stubOperations{registration}); err != nil {
		t.Fatalf("RegisterProvider: %v", err)
	}

	broker.UnregisterTool(definition.Name, definition.Version)

	if broker.recoveryFor(&definition) != nil {
		t.Error("the recovery outlived its tool")
	}
}
