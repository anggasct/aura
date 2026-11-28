package runtimeengine

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/platform/principal"
	"github.com/anggasct/aura/internal/platform/turn"
	runtimechannelhost "github.com/anggasct/aura/internal/platform/turn/channelhost"
	runtimeingress "github.com/anggasct/aura/internal/platform/turn/ingress"
	turnstore "github.com/anggasct/aura/internal/platform/turn/store"
	"github.com/anggasct/aura/internal/platform/turn/turntest"
)

func TestPlanDeliveryCarriesSessionLedgerTaint(t *testing.T) {
	t.Run("descriptor keeps relayed, attachment, and narrowed knowledge taint", func(t *testing.T) {
		cases := []struct {
			name       string
			req        *turn.TurnRequest
			wantSource turn.TaintSource
			wantDetail string
		}{
			{
				name: "relayed part",
				req: &turn.TurnRequest{
					TurnID: "turn-relayed", SessionID: "sess-1", PrincipalID: principal.OwnerID,
					PrincipalClass: principal.ClassOwner, Origin: "telegram:main",
					Parts: []runtimeingress.InputPart{{Text: "forwarded text", Source: turn.PartSourceRelayed, Detail: "telegram forward"}},
				},
				wantSource: turn.TaintSourceRelayedInput, wantDetail: "telegram forward",
			},
			{
				name: "attachment part",
				req: &turn.TurnRequest{
					TurnID: "turn-attach", SessionID: "sess-1", PrincipalID: principal.OwnerID,
					PrincipalClass: principal.ClassOwner, Origin: "discord:main",
					Parts: []runtimeingress.InputPart{{Text: "doc", Source: turn.PartSourceAttachment, Detail: "document"}},
				},
				wantSource: turn.TaintSourceAttachment, wantDetail: "document",
			},
			{
				name: "non-owner knowledge scope",
				req: &turn.TurnRequest{
					TurnID: "turn-kb", SessionID: "sess-1", PrincipalID: "public-1",
					PrincipalClass: principal.ClassPublic, Origin: "telegram:main",
					Knowledge: []string{"faq"},
					Parts:     []runtimeingress.InputPart{{Text: "question"}},
				},
				wantSource: turn.TaintSourceKnowledgeScope,
			},
			{
				name: "owner with knowledge stays clean",
				req: &turn.TurnRequest{
					TurnID: "turn-owner-clean", SessionID: "sess-1", PrincipalID: principal.OwnerID,
					PrincipalClass: principal.ClassOwner, Origin: turn.OriginTerminal,
					Knowledge: []string{"faq"},
					Parts:     []runtimeingress.InputPart{{Text: "question"}},
				},
			},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				stored, _ := descriptorThroughJSON(t, tc.req)
				restored := turnRequestFromDescriptor(&stored)
				wantMark, wantTainted := tc.req.AdmissionTaint()
				gotMark, gotTainted := restored.AdmissionTaint()
				if gotTainted != wantTainted || gotMark != wantMark {
					t.Fatalf("restored AdmissionTaint = %+v %v, want %+v %v", gotMark, gotTainted, wantMark, wantTainted)
				}
				if tc.wantSource != "" && (gotMark.Source != tc.wantSource || gotMark.Detail != tc.wantDetail) {
					t.Fatalf("restored mark = %+v, want %q %q", gotMark, tc.wantSource, tc.wantDetail)
				}
			})
		}
	})

	t.Run("planDelivery reflects session ledger taint written during the turn", func(t *testing.T) {
		executor := turntest.NewFakeExecutor(nil)
		capture := &capturingAdapter{}
		engine, db, _ := newTestRuntime(t, Deps{Config: testLimits(2, 4), Channels: []runtimechannelhost.ChannelPort{capture}}, executor)
		engine.taintReader = turnstore.NewTaintReader(db)

		cleanReq := channelTurnRequest("channel:333", "turn-mid")
		cleanReq.PrincipalClass = principal.ClassOwner
		markPayload, err := turn.EncodeTaintMark(turn.TaintMark{Source: turn.TaintSourceToolResult, Detail: "web_fetch"})
		if err != nil {
			t.Fatalf("EncodeTaintMark: %v", err)
		}
		events := []turn.Event{
			{ID: "ev-mark", SessionID: cleanReq.SessionID, TurnID: cleanReq.TurnID, Author: "system", Kind: turn.EventKindSessionTainted, SchemaVersion: 1, Payload: markPayload, CreatedAt: time.Now().UTC()},
			{Kind: turn.EventKindMessageCompleted, Payload: json.RawMessage(`{"text":"summary"}`)},
		}
		final := engine.planFinalMessage(context.Background(), cleanReq, events, turnEnd{})
		planned, err := engine.planDelivery(cleanReq, &final)
		if err != nil {
			t.Fatalf("planDelivery: %v", err)
		}
		if !planned.request.Tainted {
			t.Fatal("planned delivery is clean after a session.tainted.v1 mark was emitted in the turn")
		}
	})
}
