package toolbroker

import (
	"testing"

	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/tools"
	"github.com/anggasct/aura/internal/platform/turn"
)

func dispositionDefinition(t *testing.T, risk tools.Risk, mutate func(*tools.Definition)) *tools.Definition {
	t.Helper()
	definition := &tools.Definition{Name: "aura_probe", Version: tools.Version, Operation: "probe.run", Risk: risk}
	if mutate != nil {
		mutate(definition)
	}
	if err := definition.ApplyOperationFlags(); err != nil {
		t.Fatalf("ApplyOperationFlags: %v", err)
	}
	return definition
}

func TestOperationDispositionFollowsTheBrokerRules(t *testing.T) {
	attended := OperationTurn{Origin: turn.OriginTerminal}
	unattended := OperationTurn{Origin: turn.OriginTerminal, Unattended: true}
	cases := []struct {
		name       string
		definition *tools.Definition
		turn       OperationTurn
		want       Disposition
	}{
		{name: "a read runs", definition: dispositionDefinition(t, tools.RiskRead, nil), turn: attended, want: DispositionRuns},
		{name: "a read runs unattended", definition: dispositionDefinition(t, tools.RiskRead, nil), turn: unattended, want: DispositionRuns},
		{name: "a change asks while content taint is not tracked", definition: dispositionDefinition(t, tools.RiskChange, nil), turn: attended, want: DispositionAsks},
		{name: "a change runs once content taint is tracked and the turn is untainted", definition: dispositionDefinition(t, tools.RiskChange, nil), turn: OperationTurn{Origin: turn.OriginTerminal, ContentTaintTracked: true}, want: DispositionRuns},
		{name: "a tainted change asks even when content taint is tracked", definition: dispositionDefinition(t, tools.RiskChange, nil), turn: OperationTurn{Origin: turn.OriginTerminal, ContentTaintTracked: true, Tainted: true}, want: DispositionAsks},
		{name: "a change is refused unattended", definition: dispositionDefinition(t, tools.RiskChange, nil), turn: unattended, want: DispositionRefused},
		{name: "a notice runs unattended", definition: dispositionDefinition(t, tools.RiskChange, func(d *tools.Definition) { d.OwnerNotice = true }), turn: unattended, want: DispositionRuns},
		{name: "a change with fresh approval always asks", definition: dispositionDefinition(t, tools.RiskChange, func(d *tools.Definition) { d.FreshApproval = true }), turn: attended, want: DispositionAlways},
		{name: "a trust grant always asks", definition: dispositionDefinition(t, tools.RiskTrustGrant, nil), turn: attended, want: DispositionAlways},
		{name: "a tainted trust grant is refused", definition: dispositionDefinition(t, tools.RiskTrustGrant, nil), turn: OperationTurn{Origin: turn.OriginTerminal, Tainted: true}, want: DispositionRefused},
		{name: "a destructive call is refused unattended", definition: dispositionDefinition(t, tools.RiskDestructive, nil), turn: unattended, want: DispositionRefused},
		{name: "a scheduled origin is unattended without the flag", definition: dispositionDefinition(t, tools.RiskDestructive, nil), turn: OperationTurn{Origin: turn.OriginSchedule}, want: DispositionRefused},
		{name: "an http origin is unattended without the flag", definition: dispositionDefinition(t, tools.RiskChange, nil), turn: OperationTurn{Origin: turn.OriginHTTP}, want: DispositionRefused},
		{name: "a child origin is refused", definition: dispositionDefinition(t, tools.RiskRead, nil), turn: OperationTurn{Origin: turn.OriginChild}, want: DispositionRefused},
		{name: "a workflow origin is refused", definition: dispositionDefinition(t, tools.RiskRead, nil), turn: OperationTurn{Origin: turn.OriginWorkflow}, want: DispositionRefused},
		{name: "no origin is refused", definition: dispositionDefinition(t, tools.RiskRead, nil), turn: OperationTurn{}, want: DispositionRefused},
		{name: "an unknown risk is refused", definition: &tools.Definition{Name: "aura_probe", Version: tools.Version, Operation: "probe.run", Risk: "dangerous"}, turn: attended, want: DispositionRefused},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := OperationDisposition(tc.definition, &tc.turn)
			if err != nil || got != tc.want {
				t.Fatalf("OperationDisposition = %q, %v; want %q", got, err, tc.want)
			}
		})
	}
	t.Run("nil arguments are refused with a coded error", func(t *testing.T) {
		if got, err := OperationDisposition(nil, &attended); !fault.HasCode(err, ResultInvalidArgument) || got != DispositionRefused {
			t.Fatalf("OperationDisposition(nil definition) = %q, %v", got, err)
		}
		if got, err := OperationDisposition(dispositionDefinition(t, tools.RiskRead, nil), nil); !fault.HasCode(err, ResultInvalidArgument) || got != DispositionRefused {
			t.Fatalf("OperationDisposition(nil turn) = %q, %v", got, err)
		}
	})
}
