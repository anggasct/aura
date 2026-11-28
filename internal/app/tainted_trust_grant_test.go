package app

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	kitapproval "github.com/anggasct/aura/internal/channels/kit/approval"
	"github.com/anggasct/aura/internal/platform/approval"
	"github.com/anggasct/aura/internal/platform/operation"
	"github.com/anggasct/aura/internal/platform/operation/operationtest"
	"github.com/anggasct/aura/internal/platform/toolbroker"
	"github.com/anggasct/aura/internal/platform/tools"
	"github.com/anggasct/aura/internal/platform/turn"
	turnstore "github.com/anggasct/aura/internal/platform/turn/store"
)

func TestTaintedTrustGrantAsksOneWarningCard(t *testing.T) {
	rig := newRegistryRig(t, func(context.Context, *toolbroker.ApprovalPrompt) (bool, error) { return true, nil })
	registrations := registeredTools(t, rig)
	checked := 0
	for i := range registrations {
		def := &registrations[i].Definition
		if def.Risk != tools.RiskTrustGrant || def.TaintedGrant == "" {
			continue
		}
		checked++
		t.Run(def.Operation, func(t *testing.T) {
			req := rig.request(def.Name, rig.sampleInput(t, def.Name))
			req.Tainted = true
			req.TrustGrantOrdinal = 1
			req.TaintMark = turn.TaintMark{Source: turn.TaintSourceToolResult, Detail: "web_fetch bücher.example"}
			req.TaintSource = req.TaintMark.Label()
			before := len(rig.Prompts())
			got := rig.Execute(t.Context(), req)
			prompts := rig.Prompts()[before:]
			if len(prompts) != 1 {
				t.Fatalf("prompts = %d (outcome=%+v), want 1 warning card", len(prompts), got)
			}
			if prompts[0].ReasonCode != approval.ReasonUntrustedTrustGrant {
				t.Errorf("ReasonCode = %q, want %q", prompts[0].ReasonCode, approval.ReasonUntrustedTrustGrant)
			}
			if prompts[0].StandingGrantAllowed {
				t.Error("StandingGrantAllowed = true, want false")
			}
			if prompts[0].TaintSource != "a web_fetch result from xn--bcher-kva.example" {
				t.Errorf("TaintSource = %q, want punycode source", prompts[0].TaintSource)
			}
		})
	}
	if checked < 2 {
		t.Fatalf("checked %d declared trust grants, want at least cron.add and kb.embed", checked)
	}
}

func TestSecondTaintedTrustGrantInATurnIsRefused(t *testing.T) {
	rig := newRegistryRig(t, func(context.Context, *toolbroker.ApprovalPrompt) (bool, error) { return true, nil })
	model := &operationtest.ScriptedModel{}
	sess := &surfaceSession{t: t, rig: rig, executor: operationSurfaceExecutor(t, rig, model), model: model, events: turnstore.NewEventStore(rig.DB)}
	add := operationtest.CallOf("aura_cron_add", map[string]any{"name": "nightly", "schedule": "0 21 * * *", "timezone": "UTC", "prompt": "summarize", "context": "full"})
	sess.turn(turn.OriginTerminal, nil, operationtest.CallOf(tools.OperationSearchToolName, map[string]any{"group": "cron"}))

	before := len(rig.Prompts())
	calls := sess.turn(turn.OriginTerminal, func(req *turn.TurnRequest) {
		req.Knowledge = []string{"notes"}
	}, add, add)
	prompts := rig.Prompts()[before:]
	if len(prompts) != 1 || prompts[0].ReasonCode != approval.ReasonUntrustedTrustGrant {
		t.Fatalf("prompts = %+v, want exactly 1 warning card for the first grant", prompts)
	}
	last := calls[len(calls)-1]
	if answersMentioning(last, string(toolbroker.ResultUntrustedInputTrustGrant)) == 0 {
		t.Errorf("second grant answers = %q, want %s", last.Answers, toolbroker.ResultUntrustedInputTrustGrant)
	}
	if states := jobStates(t, rig); len(states) != 1 {
		t.Errorf("jobs created = %d, want 1", len(states))
	}
}

func TestTaintedTrustGrantIsRefusedUnattended(t *testing.T) {
	rig := newRegistryRig(t, func(context.Context, *toolbroker.ApprovalPrompt) (bool, error) { return true, nil })
	registrations := registeredTools(t, rig)
	for i := range registrations {
		def := &registrations[i].Definition
		if def.Risk != tools.RiskTrustGrant {
			continue
		}
		t.Run(def.Operation, func(t *testing.T) {
			req := rig.request(def.Name, rig.sampleInput(t, def.Name))
			req.Tainted = true
			req.TrustGrantOrdinal = 1
			req.Unattended = true
			before := len(rig.Prompts())
			got := rig.Execute(t.Context(), req)
			if got.Class != toolbroker.ResultOwnerTurnRequired {
				t.Errorf("unattended tainted %s = %+v, want %s", def.Operation, got, toolbroker.ResultOwnerTurnRequired)
			}
			if len(rig.Prompts()) != before {
				t.Errorf("unattended tainted %s raised a card", def.Operation)
			}
		})
	}
}

func TestTaintedPluginOrMCPApprovalIsRefused(t *testing.T) {
	for _, opID := range []string{"plugin.approve", "mcp.trust.approve"} {
		t.Run(opID, func(t *testing.T) {
			group, _, _ := strings.Cut(opID, ".")
			catalog := operation.Catalog{
				Group: operation.Group{Name: group, Summary: "trust approvals"},
				Descriptors: []operation.Descriptor{{
					ID:           opID,
					Summary:      "Approve external tool trust",
					Exposure:     operation.ExposureAgent,
					Risk:         operation.RiskTrustGrant,
					Commit:       operation.CommitTransactional,
					Preview:      true,
					Confirmation: operation.ConfirmPreviewUntilApply,
					Params:       []operation.Param{{Name: "id", Kind: operation.KindString, Required: true, MinLength: 1, MaxLength: 64}},
					Output:       operation.Output{Kind: operation.OutputText},
				}},
			}
			previewed := 0
			rig := operationtest.NewRig(t, &operationtest.Options{
				Catalogs: []operation.Catalog{catalog},
				Bindings: func(*operationtest.Stubs) []operation.Binding {
					return []operation.Binding{{
						ID: opID,
						Preview: func(context.Context, *operation.Call) (operation.Preview, error) {
							previewed++
							return operation.Preview{Lines: []string{"Approve: x"}, Digest: "d"}, nil
						},
						Handle: func(context.Context, *operation.Call) (operation.Result, error) {
							return operation.Result{Data: json.RawMessage(`{"ok":true}`)}, nil
						},
					}}
				},
			})
			req := operationtest.RequestFor(operation.ToolName(opID), json.RawMessage(`{"id":"srv"}`), "tainted-"+opID)
			req.Tainted = true
			req.TrustGrantOrdinal = 1
			got := rig.Execute(t.Context(), req)
			if got.Class != toolbroker.ResultUntrustedInputTrustGrant {
				t.Fatalf("tainted %s = %+v, want %s", opID, got, toolbroker.ResultUntrustedInputTrustGrant)
			}
			if previewed != 0 || len(rig.Prompts()) != 0 {
				t.Errorf("tainted %s previewed=%d prompts=%d, want 0", opID, previewed, len(rig.Prompts()))
			}
		})
	}
}

func TestTaintedCronAddCardShowsTheWholePrompt(t *testing.T) {
	rig := newRegistryRig(t, func(context.Context, *toolbroker.ApprovalPrompt) (bool, error) { return true, nil })
	promptText := "Check the daily deployment metrics and report any anomaly in exact detail"
	input, err := json.Marshal(map[string]any{
		"name":     "metrics",
		"schedule": "0 9 * * *",
		"timezone": "UTC",
		"prompt":   promptText,
		"context":  "full",
	})
	if err != nil {
		t.Fatalf("marshal input: %v", err)
	}
	req := rig.request("aura_cron_add", input)
	req.Tainted = true
	req.TrustGrantOrdinal = 1
	req.TaintMark = turn.TaintMark{Source: turn.TaintSourceRelayedInput, Detail: "telegram forward"}
	req.TaintSource = req.TaintMark.Label()

	got := rig.Execute(t.Context(), req)
	if got.Class != toolbroker.ResultOK {
		t.Fatalf("tainted cron.add = %+v, want ok", got)
	}
	prompts := rig.Prompts()
	if len(prompts) != 1 {
		t.Fatalf("prompts = %d, want 1", len(prompts))
	}
	card := kitapproval.RenderCard(&kitapproval.Prompt{
		ToolName:    prompts[0].ToolName,
		ToolVersion: prompts[0].ToolVersion,
		SessionID:   prompts[0].SessionID,
		PrincipalID: prompts[0].PrincipalID,
		Arguments:   prompts[0].Arguments,
		ReasonCode:  prompts[0].ReasonCode,
		TaintSource: prompts[0].TaintSource,
	}, "Main (main)", nil)
	for _, want := range []string{
		approval.UntrustedTrustGrantNotice,
		"came in through: a forwarded or quoted message",
		"Runs prompt: " + promptText,
	} {
		if !strings.Contains(card, want) {
			t.Errorf("card lacks %q:\n%s", want, card)
		}
	}

	oversized, err := json.Marshal(map[string]any{
		"name":     "huge",
		"schedule": "0 9 * * *",
		"timezone": "UTC",
		"prompt":   strings.Repeat("word ", 260),
		"context":  "full",
	})
	if err != nil {
		t.Fatalf("marshal oversized input: %v", err)
	}
	bigReq := rig.request("aura_cron_add", oversized)
	bigReq.Tainted = true
	bigReq.TrustGrantOrdinal = 1
	bigReq.TaintSource = req.TaintSource
	if bigGot := rig.Execute(t.Context(), bigReq); bigGot.Class != toolbroker.ResultApprovalDisplayTooLarge {
		t.Errorf("oversized prompt outcome = %+v, want %s", bigGot, toolbroker.ResultApprovalDisplayTooLarge)
	}
}
