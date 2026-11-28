package toolbroker

import (
	"encoding/json"
	"testing"

	"github.com/anggasct/aura/internal/platform/approval"
	"github.com/anggasct/aura/internal/platform/tools"
)

func TestTheApprovalCardReportsNetworkAccessFromWhatTheOperationDeclares(t *testing.T) {
	cases := []struct {
		name       string
		definition tools.Definition
		allowed    bool
		want       bool
	}{
		{name: "an operation that reaches the network", definition: tools.Definition{Operation: "kb.add", ReachesNetwork: true}, want: true},
		{name: "an operation that stays on the host", definition: tools.Definition{Operation: "cron.add"}},
		{name: "an operation that is a sink without reaching the network", definition: tools.Definition{Operation: "cron.add", ExternalSink: true}},
		{name: "a legacy sink", definition: tools.Definition{ExternalSink: true}},
		{name: "a sandbox constraint", definition: tools.Definition{Operation: "kb.add"}, allowed: true, want: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			broker := newFixtureBroker(t, &Options{})
			tc.definition.Name, tc.definition.Version, tc.definition.Risk = "aura_probe", tools.Version, tools.RiskChange
			call := &authorization{
				canonical:  &ToolRequest{ToolName: "aura_probe", ToolVersion: tools.Version, Arguments: json.RawMessage(`{}`)},
				definition: &tc.definition,
				decision:   approval.PolicyDecision{Constraints: approval.Constraints{AllowNetwork: tc.allowed}},
			}

			prompt, err := broker.buildApprovalPrompt(call)

			if err != nil {
				t.Fatalf("buildApprovalPrompt: %v", err)
			}
			if prompt.Network != tc.want {
				t.Errorf("Network = %v, want %v", prompt.Network, tc.want)
			}
		})
	}
}
