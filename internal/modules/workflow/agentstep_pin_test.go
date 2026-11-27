package workflow

import (
	"testing"
	"time"

	auraagent "github.com/anggasct/aura/internal/platform/agent"
	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/turn"
)

type editedPersona struct {
	AgentResolver
	personaID    string
	instructions string
	modelRoute   string
}

func (e editedPersona) Resolve(required []string, preferID *string) (auraagent.Definition, error) {
	definition, err := e.AgentResolver.Resolve(required, preferID)
	if err != nil || definition.ID != e.personaID {
		return definition, err
	}
	if e.instructions != "" {
		definition.Instructions = e.instructions
	}
	if e.modelRoute != "" {
		definition.ModelRoute = e.modelRoute
	}
	return definition, nil
}

func (h *agentHarness) currentPin(t *testing.T, personaID string) PersonaPin {
	t.Helper()
	definition, err := h.deps.Agents.Resolve(nil, &personaID)
	if err != nil {
		t.Fatalf("Resolve %s: %v", personaID, err)
	}
	fingerprint := definition.Fingerprint()
	return PersonaPin{SoulSHA256: fingerprint.SoulSHA256, ModelRoute: fingerprint.ModelRoute}
}

func (h *agentHarness) reapprove(t *testing.T, kind ApprovalKind, pins map[string]PersonaPin) {
	t.Helper()
	if _, err := h.db.ExecContext(t.Context(), `DELETE FROM workflow_approval WHERE definition_id = ?`, h.spec.ID); err != nil {
		t.Fatalf("delete approval: %v", err)
	}
	digest, err := Digest(h.spec)
	if err != nil {
		t.Fatalf("Digest: %v", err)
	}
	approval := &Approval{
		DefinitionID: h.spec.ID, DefinitionVersion: h.spec.Version, Digest: digest, Kind: kind,
		AgentID: "main", ApproverID: "local-owner", PersonaPins: pins, ApprovedAt: time.Now().UTC(),
	}
	switch kind {
	case ApprovalProposal:
		approval.ProposalID = proposeSpec(t, h.disk, "pins-"+h.spec.ID, "main", false, h.spec).ID
	case ApprovalRoutine:
		approval.SourceDefinitionID, approval.SourceDefinitionVersion = "source", 1
	case ApprovalInstalled:
	}
	if err := insertApproval(t.Context(), h.db, approval); err != nil {
		t.Fatalf("insertApproval: %v", err)
	}
}

func TestAgentStepPersonaPins(t *testing.T) {
	cases := []struct {
		name     string
		kind     ApprovalKind
		pinned   []string
		edit     *editedPersona
		wantCode ErrorCode
		wantPin  bool
	}{
		{name: "a proposal approval whose pin matches runs and carries the pin", kind: ApprovalProposal, wantPin: true, pinned: []string{"researcher"}},
		{name: "a routine approval whose pin matches runs and carries the pin", kind: ApprovalRoutine, wantPin: true, pinned: []string{"researcher"}},
		{name: "an installed approval with no pins runs without a pin", kind: ApprovalInstalled},
		{name: "a proposal approval with no pins at all", kind: ApprovalProposal, wantCode: ErrorCodeApprovalMissing},
		{name: "a proposal approval with a pin for another persona only", kind: ApprovalProposal, wantCode: ErrorCodeApprovalMissing, pinned: []string{"engineer"}},
		{name: "a routine approval with no pin for the step persona", kind: ApprovalRoutine, wantCode: ErrorCodeApprovalMissing},
		{name: "a pinned persona whose instructions changed", kind: ApprovalProposal, wantCode: ErrorCodeApprovalMissing,
			edit: &editedPersona{personaID: "researcher", instructions: "Read carelessly."}, pinned: []string{"researcher"}},
		{name: "a pinned persona whose model route changed", kind: ApprovalRoutine, wantCode: ErrorCodeApprovalMissing,
			edit: &editedPersona{personaID: "researcher", modelRoute: "premium"}, pinned: []string{"researcher"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newAgentHarness(t, &Options{})
			h.turns.replies = finalAnswer("done")
			h.prepare(t, agentSpec("pinned", agentStep("s", agentExec("go", ""))))
			pins := map[string]PersonaPin{}
			for _, personaID := range tc.pinned {
				pins[personaID] = h.currentPin(t, personaID)
			}
			h.reapprove(t, tc.kind, pins)
			if tc.edit != nil {
				edit := *tc.edit
				edit.AgentResolver = h.deps.Agents
				h.deps.Agents = edit
			}
			_, err := h.runner.RunStep(t.Context(), h.call(t, 0))
			requests, models, _ := h.turns.snapshot()
			if tc.wantCode != "" {
				if !hasCode(err, tc.wantCode) {
					t.Fatalf("RunStep = %v, want %s", err, tc.wantCode)
				}
				if !finalFailure(string(tc.wantCode)) {
					t.Fatalf("%s is retried", tc.wantCode)
				}
				if len(requests) != 0 || models != 0 {
					t.Fatalf("the turn ran %d requests and %d model calls before the refusal", len(requests), models)
				}
				return
			}
			if err != nil {
				t.Fatalf("RunStep: %v", err)
			}
			got := requests[0].Persona
			if !tc.wantPin {
				if got != nil {
					t.Fatalf("request persona = %+v, want none", got)
				}
				return
			}
			want := h.currentPin(t, "researcher")
			if got == nil || got.AgentID != "researcher" || got.SoulSHA256 != want.SoulSHA256 || got.ModelRoute != want.ModelRoute {
				t.Fatalf("request persona = %+v, want the pin %+v", got, want)
			}
		})
	}
}

func TestAgentStepPersonaEditedAfterTheCheckIsRefusedByTheExecutor(t *testing.T) {
	h := newAgentHarness(t, &Options{})
	h.turns.replies = finalAnswer("never reached")
	h.prepare(t, agentSpec("pinned", agentStep("s", agentExec("go", ""))))
	h.reapprove(t, ApprovalProposal, map[string]PersonaPin{"researcher": h.currentPin(t, "researcher")})
	h.turns.beforeRun = func(req *turn.TurnRequest) error {
		h.deps.Agents = editedPersona{AgentResolver: h.deps.Agents, personaID: "researcher", instructions: "Read carelessly."}
		current := h.currentPin(t, "researcher")
		if req.Persona == nil || req.Persona.SoulSHA256 != current.SoulSHA256 {
			return fault.New(turn.ErrorCodePersonaMismatch, "persona changed since it was pinned")
		}
		return nil
	}
	_, err := h.runner.RunStep(t.Context(), h.call(t, 0))
	if !hasCode(err, ErrorCodeApprovalMissing) {
		t.Fatalf("RunStep = %v, want %s", err, ErrorCodeApprovalMissing)
	}
	if requests, models, _ := h.turns.snapshot(); len(requests) != 1 || models != 0 {
		t.Fatalf("requests = %d, model calls = %d, want the refused turn and no model call", len(requests), models)
	}
}
