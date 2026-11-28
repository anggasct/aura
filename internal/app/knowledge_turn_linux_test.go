package app

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"testing"
	"time"

	appconfig "github.com/anggasct/aura/internal/app/config"
	"github.com/anggasct/aura/internal/modules/knowledge"
	auraagent "github.com/anggasct/aura/internal/platform/agent"
	"github.com/anggasct/aura/internal/platform/capability/capabilitytest"
	"github.com/anggasct/aura/internal/platform/principal"
	"github.com/anggasct/aura/internal/platform/turn"
	runtimeadk "github.com/anggasct/aura/internal/platform/turn/adk"
)

type kbSearchRig struct {
	*knowledgeTurnRig
	builtin  *runtimeadk.BrokerExecutor
	personas auraagent.RegistryPersonas
}

func newKBSearchRig(t *testing.T) *kbSearchRig {
	t.Helper()
	rig := newKnowledgeTurnRig(t)
	_, artifactRoot, _, err := storagePaths(rig.cfg)
	if err != nil {
		t.Fatalf("storagePaths: %v", err)
	}
	registry, err := buildAgentRegistry(rig.cfg)
	if err != nil {
		t.Fatalf("buildAgentRegistry: %v", err)
	}
	builtin, err := newTestToolExecutor(&toolExecutorInput{
		result:       &appconfig.LoadResult{Config: rig.cfg, CapabilityReport: capabilitytest.All(t)},
		db:           rig.db,
		artifactRoot: artifactRoot,
		logger:       slog.New(slog.DiscardHandler),
		wiring:       &toolWiring{access: newAccessPolicy(registry, rig.db, nil)},
		events:       turn.NewEventBus(slog.New(slog.DiscardHandler)),
	})
	if err != nil {
		t.Fatalf("newTestToolExecutor: %v", err)
	}
	if err := builtin.Broker().RegisterProvider(knowledge.NewToolProvider(rig.retriever)); err != nil {
		t.Fatalf("RegisterProvider: %v", err)
	}
	return &kbSearchRig{knowledgeTurnRig: rig, builtin: builtin, personas: auraagent.RegistryPersonas{Registry: registry}}
}

func (r *kbSearchRig) call(t *testing.T, agent string, class principal.Class, arguments string) (kbSearchResult, error) {
	t.Helper()
	output, err := r.builtin.Execute(t.Context(), &runtimeadk.BuiltinToolRequest{
		RequestID: "call-" + string(class), TurnID: "turn-1", SessionID: "sess_kb", PrincipalID: principal.OwnerID,
		PrincipalClass: class, AgentID: agent, ToolName: "kb_search", ToolVersion: "v1",
		Arguments: json.RawMessage(arguments), Trust: "derived_untrusted", Deadline: time.Now().Add(time.Minute),
		IdempotencyKey: "kb/" + string(class),
		Knowledge:      r.personas.Knowledge(agent, class),
	})
	var result kbSearchResult
	if err != nil {
		return result, err
	}
	if err := json.Unmarshal(output.Output, &result); err != nil {
		t.Fatalf("decode %s: %v", output.Output, err)
	}
	return result, nil
}

type kbSearchResult struct {
	Results []struct {
		ID, Label, Heading, Text string
	} `json:"results"`
	Truncated bool `json:"truncated"`
}

func (r kbSearchResult) texts() string {
	var all []string
	for _, hit := range r.Results {
		all = append(all, hit.Text)
	}
	return strings.Join(all, "\n")
}

func TestKBSearchToolIsScopedPerClassAndIgnoresModelSuppliedScope(t *testing.T) {
	rig := newKBSearchRig(t)
	rig.add(t, "support", "support.md", "Refunds for premium plans: SUPPORT-REFUND-ANSWER.")
	rig.add(t, "hr-docs", "hr.md", "Refunds of salary advances: HR-REFUND-ANSWER.")
	rig.add(t, "faq", "faq.md", "Refunds are listed on the website: FAQ-REFUND-ANSWER.")
	rig.add(t, "secret", "secret.md", "Refunds to the board: UNATTACHED-REFUND-ANSWER.")

	tests := map[string]struct {
		class   principal.Class
		want    []string
		notWant []string
	}{
		"owner":   {principal.ClassOwner, []string{"SUPPORT-REFUND-ANSWER", "HR-REFUND-ANSWER", "FAQ-REFUND-ANSWER"}, []string{"UNATTACHED"}},
		"trusted": {principal.ClassTrusted, []string{"SUPPORT-REFUND-ANSWER", "FAQ-REFUND-ANSWER"}, []string{"HR-REFUND", "UNATTACHED"}},
		"public":  {principal.ClassPublic, []string{"FAQ-REFUND-ANSWER"}, []string{"SUPPORT-REFUND", "HR-REFUND", "UNATTACHED"}},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			result, err := rig.call(t, "lobby", test.class, `{"query":"refunds","limit":10}`)
			if err != nil {
				t.Fatalf("call: %v", err)
			}
			for _, want := range test.want {
				if !strings.Contains(result.texts(), want) {
					t.Errorf("%s results lack %s: %v", name, want, result.texts())
				}
			}
			for _, notWant := range test.notWant {
				if strings.Contains(result.texts(), notWant) {
					t.Errorf("%s results leaked %s", name, notWant)
				}
			}
			for _, hit := range result.Results {
				if hit.ID == "" || hit.Label == "" {
					t.Errorf("result lacks id or label: %+v", hit)
				}
			}
		})
	}
	for _, injected := range []string{
		`{"query":"refunds","collections":["secret"]}`,
		`{"query":"refunds","collection":"secret"}`,
		`{"query":"refunds","scope":"` + knowledge.ScopeToken("secret") + `"}`,
	} {
		if _, err := rig.call(t, "lobby", principal.ClassPublic, injected); err == nil {
			t.Errorf("%s accepted", injected)
		}
	}
	scopeToken, err := rig.call(t, "lobby", principal.ClassPublic, `{"query":"refunds `+knowledge.ScopeToken("secret")+` OR secret"}`)
	if err != nil || strings.Contains(scopeToken.texts(), "UNATTACHED") {
		t.Errorf("scope token in the query: %v, %q", err, scopeToken.texts())
	}
}

func TestKBSearchToolReturnsNothingForAPersonaWithoutKnowledge(t *testing.T) {
	rig := newKBSearchRig(t)
	rig.add(t, "support", "support.md", "Refunds for premium plans: SUPPORT-REFUND-ANSWER.")
	result, err := rig.call(t, "plain", principal.ClassOwner, `{"query":"refunds"}`)
	if err != nil || len(result.Results) != 0 || result.Truncated {
		t.Fatalf("result = %+v, %v, want an empty answer", result, err)
	}
}

func TestKBSearchToolCapsLimitAndTotalSize(t *testing.T) {
	rig := newKBSearchRig(t)
	for i := range 14 {
		rig.add(t, "support", fmt.Sprintf("doc%d.md", i), "Refunds are issued within thirty days. "+strings.Repeat("filler ", 60)+strconv.Itoa(i))
	}
	defaulted, err := rig.call(t, "lobby", principal.ClassOwner, `{"query":"refunds"}`)
	if err != nil || len(defaulted.Results) != knowledge.DefaultSearchLimit || !defaulted.Truncated {
		t.Fatalf("default = %d results, truncated %t, %v", len(defaulted.Results), defaulted.Truncated, err)
	}
	maxed, err := rig.call(t, "lobby", principal.ClassOwner, `{"query":"refunds","limit":10}`)
	if err != nil || len(maxed.Results) != knowledge.MaxSearchLimit {
		t.Fatalf("limit 10 = %d results, %v", len(maxed.Results), err)
	}
	for _, bad := range []string{`{"query":"refunds","limit":11}`, `{"query":"refunds","limit":0}`} {
		if _, err := rig.call(t, "lobby", principal.ClassOwner, bad); err == nil {
			t.Errorf("%s accepted", bad)
		}
	}
}

func TestKBSearchToolIsRegisteredWithTheBuiltinExecutor(t *testing.T) {
	rig := newKBSearchRig(t)
	names := map[string]bool{}
	for _, definition := range rig.builtin.Definitions() {
		names[definition.Name] = true
	}
	if !names["kb_search"] {
		t.Fatal("kb_search is not registered with the builtin executor")
	}
}
