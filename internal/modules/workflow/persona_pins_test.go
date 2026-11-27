package workflow

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/platform/sqlite/sqlitetest"
	turnstore "github.com/anggasct/aura/internal/platform/turn/store"
)

func TestProposalWithAgentStepStoresThePlanAndPins(t *testing.T) {
	interpreter, _, disk := serialTestSetup(t, &fakeToolRunner{})
	deps := testValidationDeps()
	if err := interpreter.Install(t.Context(), nil, deps); err != nil {
		t.Fatal(err)
	}
	spec := NormalizeProposal(agentSpec("x",
		agentStep("research", agentExec("Find it.\nBe brief.", "", "web_search")),
		agentStep("summarise", agentExec("Summarise.", ""), "research"),
	), "prop_pins")
	spec.Steps[1].Executor.AgentID = ptr("reviewer")
	graph, err := interpreter.CompileProposal(spec, "main")
	if err != nil {
		t.Fatal(err)
	}
	plan, err := RenderPlan(spec, graph, nil)
	if err != nil {
		t.Fatal(err)
	}
	proposalSession(t, disk, "session-prop_pins")
	digest, err := Digest(spec)
	if err != nil {
		t.Fatal(err)
	}
	if err := disk.CreateProposal(t.Context(), &Proposal{
		ID: "prop_pins", SessionID: "session-prop_pins", AgentID: "main", PrincipalID: "local-owner",
		Spec: spec, PlanText: plan, Digest: digest, CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	runID, err := ApproveStoredProposal(t.Context(), interpreter, disk, "prop_pins", nil, time.Now().UTC())
	if err != nil {
		t.Fatalf("ApproveStoredProposal: %v", err)
	}
	waitForRunStatus(t, disk, runID, RunSucceeded, 5*time.Second)

	stored, err := disk.Proposal(t.Context(), "prop_pins", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"ask agent researcher", "    tools: web_search", "    reads: none", "    budget: 50000 tokens", "    prompt:", "        Be brief."} {
		if !strings.Contains(stored.PlanText, want) {
			t.Fatalf("stored plan lacks %q:\n%s", want, stored.PlanText)
		}
	}
	h := &agentHarness{deps: deps}
	wantPins := map[string]PersonaPin{"researcher": h.currentPin(t, "researcher"), "reviewer": h.currentPin(t, "reviewer")}
	record, err := disk.Approval(t.Context(), spec.ID, spec.Version)
	if err != nil {
		t.Fatal(err)
	}
	if record.Kind != ApprovalProposal || !reflect.DeepEqual(record.PersonaPins, wantPins) {
		t.Fatalf("proposal approval = %s %+v, want pins %+v", record.Kind, record.PersonaPins, wantPins)
	}

	routine, err := interpreter.SaveRoutine(t.Context(), runID, "Weekly research")
	if err != nil {
		t.Fatalf("SaveRoutine: %v", err)
	}
	inherited, err := disk.Approval(t.Context(), routine.ID, routine.Version)
	if err != nil {
		t.Fatal(err)
	}
	if inherited.Kind != ApprovalRoutine || !reflect.DeepEqual(inherited.PersonaPins, wantPins) {
		t.Fatalf("routine approval = %s %+v, want the inherited pins %+v", inherited.Kind, inherited.PersonaPins, wantPins)
	}
}

func TestInstalledApprovalStoresNoPins(t *testing.T) {
	interpreter, _, disk := serialTestSetup(t, &fakeToolRunner{})
	spec := agentSpec("installed_agent", agentStep("s", agentExec("go", "")))
	if err := interpreter.Install(t.Context(), []*Spec{spec}, testValidationDeps()); err != nil {
		t.Fatal(err)
	}
	var stored string
	if err := disk.db.QueryRowContext(t.Context(), `SELECT persona_pins FROM workflow_approval WHERE definition_id = 'installed_agent'`).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored != "{}" {
		t.Fatalf("persona_pins = %q, want {}", stored)
	}
}

func TestApprovalPersonaPinsAreImmutable(t *testing.T) {
	ctx := t.Context()
	db := sqlitetest.OpenMigrated(t, turnstore.Schema(), Schema())
	if _, err := db.ExecContext(ctx, `INSERT INTO workflow_definition (id, version, goal, source, spec_json, spec_sha256, created_at) VALUES ('flow', 1, 'g', 'defined', '{}', 'digest', '2026-11-27T10:00:00Z')`); err != nil {
		t.Fatalf("seed definition: %v", err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO workflow_approval (definition_id, definition_version, spec_sha256, kind, approver_id, approved_at) VALUES ('flow', 1, 'digest', 'installed', 'owner-1', '2026-11-27T10:00:00Z')`); err != nil {
		t.Fatalf("seed approval: %v", err)
	}
	var pins string
	if err := db.QueryRowContext(ctx, `SELECT persona_pins FROM workflow_approval WHERE definition_id = 'flow'`).Scan(&pins); err != nil || pins != "{}" {
		t.Fatalf("default persona_pins = %q, %v, want {}", pins, err)
	}
	_, err := db.ExecContext(ctx, `UPDATE workflow_approval SET persona_pins = '{"researcher":{"soul_sha256":"a","model_route":"b"}}' WHERE definition_id = 'flow'`)
	if err == nil || !strings.Contains(err.Error(), "workflow_approval.persona_pins is immutable") {
		t.Fatalf("UPDATE persona_pins = %v, want the immutability abort", err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE workflow_approval SET tainted = 1 WHERE definition_id = 'flow'`); err != nil {
		t.Fatalf("UPDATE of another column = %v, want success", err)
	}
	if _, err := db.ExecContext(ctx, `DELETE FROM workflow_approval WHERE definition_id = 'flow'`); err != nil {
		t.Fatalf("DELETE = %v, want success", err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO workflow_approval (definition_id, definition_version, spec_sha256, kind, approver_id, persona_pins, approved_at) VALUES ('flow', 1, 'digest', 'installed', 'owner-1', 'not json', '2026-11-27T10:00:00Z')`); err == nil {
		t.Fatal("a persona_pins value that is not JSON was accepted")
	}
}
