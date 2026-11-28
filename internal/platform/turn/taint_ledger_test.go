package turn

import (
	"testing"

	"github.com/anggasct/aura/internal/platform/principal"
	runtimeingress "github.com/anggasct/aura/internal/platform/turn/ingress"
)

func TestAdmissionTaintSourcesAndPrecedence(t *testing.T) {
	taintedCases := []struct {
		name       string
		req        *TurnRequest
		wantSource TaintSource
		wantDetail string
	}{
		{
			name:       "nil request",
			req:        nil,
			wantSource: TaintSourceOrigin,
		},
		{
			name:       "untrusted origin beats everything",
			req:        &TurnRequest{Origin: OriginWebhook, UntrustedContext: &UntrustedRecall{}, InputTainted: true, JobTaint: "x", WebOnly: true, Knowledge: []string{"k"}, Parts: []runtimeingress.InputPart{{Text: "r", Source: PartSourceRelayed}, {Text: "a", Source: PartSourceAttachment}}},
			wantSource: TaintSourceOrigin,
			wantDetail: "webhook",
		},
		{
			name:       "client history beats workflow input",
			req:        &TurnRequest{Origin: OriginHTTP, UntrustedContext: &UntrustedRecall{}, InputTainted: true, JobTaint: "x", WebOnly: true, Knowledge: []string{"k"}},
			wantSource: TaintSourceClientHistory,
		},
		{
			name:       "workflow input beats schedule origin",
			req:        &TurnRequest{Origin: OriginWorkflow, InputTainted: true, JobTaint: "x", WebOnly: true},
			wantSource: TaintSourceWorkflowInput,
		},
		{
			name:       "schedule origin beats web only",
			req:        &TurnRequest{Origin: OriginSchedule, JobTaint: "prior", JobID: "job-7", WebOnly: true},
			wantSource: TaintSourceScheduleOrigin,
			wantDetail: "job-7",
		},
		{
			name:       "web only beats knowledge scope",
			req:        &TurnRequest{Origin: OriginSchedule, WebOnly: true, JobID: "job-8", PrincipalClass: principal.ClassPublic, Knowledge: []string{"faq"}},
			wantSource: TaintSourceWebOnly,
			wantDetail: "job-8",
		},
		{
			name:       "non-owner knowledge scope beats relayed and attachment",
			req:        &TurnRequest{Origin: "telegram:main", PrincipalClass: principal.ClassTrusted, Knowledge: []string{"faq"}, Parts: []runtimeingress.InputPart{{Text: "fwd", Source: PartSourceRelayed, Detail: "telegram forward"}, {Text: "doc", Source: PartSourceAttachment, Detail: "document"}}},
			wantSource: TaintSourceKnowledgeScope,
		},
		{
			name:       "relayed input beats attachment",
			req:        &TurnRequest{Origin: "telegram:main", PrincipalClass: principal.ClassOwner, Knowledge: []string{"faq"}, Parts: []runtimeingress.InputPart{{Text: "doc", Source: PartSourceAttachment, Detail: "image"}, {Text: "fwd", Source: PartSourceRelayed, Detail: "telegram forward"}}},
			wantSource: TaintSourceRelayedInput,
			wantDetail: "telegram forward",
		},
		{
			name:       "attachment input taints owner turn",
			req:        &TurnRequest{Origin: "discord:main", PrincipalClass: principal.ClassOwner, Parts: []runtimeingress.InputPart{{Text: "attached", Source: PartSourceAttachment, Detail: "document"}}},
			wantSource: TaintSourceAttachment,
			wantDetail: "document",
		},
	}
	for _, tc := range taintedCases {
		t.Run(tc.name, func(t *testing.T) {
			mark, tainted := tc.req.AdmissionTaint()
			if !tainted {
				t.Fatal("AdmissionTaint() = clean, want tainted")
			}
			if mark.Source != tc.wantSource || mark.Detail != tc.wantDetail {
				t.Fatalf("AdmissionTaint() = %+v, want source %q detail %q", mark, tc.wantSource, tc.wantDetail)
			}
		})
	}

	cleanCases := []struct {
		name string
		req  *TurnRequest
	}{
		{name: "owner with knowledge scope", req: &TurnRequest{Origin: OriginTerminal, PrincipalClass: principal.ClassOwner, Knowledge: []string{"faq"}}},
		{name: "owner on email", req: &TurnRequest{Origin: "email:main", PrincipalClass: principal.ClassOwner}},
		{name: "owner on http without history", req: &TurnRequest{Origin: OriginHTTP, PrincipalClass: principal.ClassOwner}},
		{name: "clean full schedule", req: &TurnRequest{Origin: OriginSchedule, PrincipalClass: principal.ClassOwner, JobID: "job-1"}},
	}
	for _, tc := range cleanCases {
		t.Run("clean "+tc.name, func(t *testing.T) {
			if mark, tainted := tc.req.AdmissionTaint(); tainted {
				t.Fatalf("AdmissionTaint() = %+v, want clean", mark)
			}
		})
	}
}
