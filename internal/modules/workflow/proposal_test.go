package workflow

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/platform/durable"
	"github.com/anggasct/aura/internal/platform/principal"
)

func proposalSession(t *testing.T, disk *Store, id string) {
	t.Helper()
	if _, err := disk.db.ExecContext(context.Background(), `INSERT INTO session (id, owner_id, metadata_json, created_at, updated_at) VALUES (?, 'owner', '{}', ?, ?)`,
		id, time.Now().UTC().Format(time.RFC3339Nano), time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
}

func TestNormalizedProposalsCannotShadowDefinitionsAndHaveStableDigests(t *testing.T) {
	proposed := toolSpec("deploy")
	proposed.Source = SourceDefined
	proposed.Version = 9
	normalized := NormalizeProposal(proposed, "prop_1")
	if normalized.ID != "gen_prop_1" || normalized.Version != 1 || normalized.Source != SourceGenerated || proposed.ID != "deploy" {
		t.Fatalf("normalized = %+v, original = %+v", normalized, proposed)
	}
	first, err := Digest(normalized)
	if err != nil {
		t.Fatal(err)
	}
	again, _ := Digest(NormalizeProposal(proposed, "prop_1"))
	changed := NormalizeProposal(proposed, "prop_1")
	changed.Steps[0].Executor.ToolArgs = json.RawMessage(`{"path":"b"}`)
	other, _ := Digest(changed)
	if first != again || first == other || len(first) != 64 {
		t.Fatalf("digests = %s %s %s", first, again, other)
	}
}

func TestCeilingAndPlanRendering(t *testing.T) {
	approve := StepSpec{ID: "approve", Executor: ExecutorSpec{Kind: KindApproval}, Timeout: time.Minute}
	event := "ci.done"
	spec := &Spec{ID: "gen_x", Goal: "Ship the fix", Version: 1, Source: SourceGenerated, Steps: []StepSpec{
		approve,
		{ID: "write", DependsOn: []string{"approve"}, Executor: ExecutorSpec{Kind: KindTool, ToolID: ptr("write_file"), ToolArgs: json.RawMessage(`{"path":"a","content":"b"}`)}, Timeout: time.Minute},
		{ID: "ci", DependsOn: []string{"write"}, Executor: ExecutorSpec{Kind: KindWait, Event: &event}, Timeout: time.Minute},
	}}
	deps := testValidationDeps()
	deps.StepAllowed = personaTools(map[string][]string{"narrow": {"read_file"}, "wide": {"write_file"}})
	if err := CheckCeiling(spec, "narrow", principal.ClassOwner, deps); !hasCode(err, ErrorCodeCapabilityExceeded) || !strings.Contains(err.Error(), "write_file") {
		t.Fatalf("ceiling without write_file = %v", err)
	}
	if err := CheckCeiling(spec, "wide", principal.ClassOwner, deps); err != nil {
		t.Fatalf("ceiling with write_file = %v", err)
	}
	graph, err := Compile(spec, testValidationDeps())
	if err != nil {
		t.Fatal(err)
	}
	plan, err := RenderPlan(spec, graph, func(toolID string, args json.RawMessage) (string, error) { return toolID + " " + string(args), nil })
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Plan: Ship the fix", "1. approve — owner approval", "2. write — run tool write_file (after approve)", "3. ci — wait for ci.done (after write)"} {
		if !strings.Contains(plan, want) {
			t.Fatalf("plan lacks %q:\n%s", want, plan)
		}
	}
}

func TestProposalStoreDecidesOnceAndExpiresAfterSevenDays(t *testing.T) {
	disk := newTestStore(t)
	proposalSession(t, disk, "s1")
	created := time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)
	for _, id := range []string{"prop_a", "prop_b"} {
		spec := NormalizeProposal(toolSpec("x"), id)
		digest, _ := Digest(spec)
		if err := disk.CreateProposal(context.Background(), &Proposal{ID: id, SessionID: "s1", AgentID: "main", PrincipalID: "owner", Spec: spec, Digest: digest, CreatedAt: created}); err != nil {
			t.Fatal(err)
		}
	}
	got, err := disk.Proposal(context.Background(), "prop_a", created.Add(time.Hour))
	if err != nil || got.Status != ProposalProposed || got.Spec.ID != "gen_prop_a" {
		t.Fatalf("proposal = %+v, %v", got, err)
	}
	if err := disk.DecideProposal(context.Background(), "prop_a", ProposalRejected, "", created.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := disk.DecideProposal(context.Background(), "prop_a", ProposalApproved, "", created.Add(time.Hour)); !hasCode(err, ErrorCodeProposalDecided) {
		t.Fatalf("second decision = %v", err)
	}
	expired, err := disk.Proposal(context.Background(), "prop_b", created.Add(ProposalTTL+time.Minute))
	if err != nil || expired.Status != ProposalExpired {
		t.Fatalf("week-old proposal = %+v, %v", expired, err)
	}
	if err := disk.DecideProposal(context.Background(), "prop_b", ProposalApproved, "", created.Add(ProposalTTL+time.Minute)); !hasCode(err, ErrorCodeProposalDecided) {
		t.Fatalf("approving an expired proposal = %v", err)
	}
	if _, err := disk.Proposal(context.Background(), "prop_missing", created); !hasCode(err, ErrorCodeProposalNotFound) {
		t.Fatalf("missing proposal = %v", err)
	}
	rejected, err := disk.Proposals(context.Background(), ProposalRejected, created.Add(ProposalTTL+time.Minute))
	if err != nil || len(rejected) != 1 || rejected[0].ID != "prop_a" {
		t.Fatalf("rejected list = %+v, %v", rejected, err)
	}
}

func TestInterpreterResolvesStoredDefinitionsItDidNotLoad(t *testing.T) {
	tools := &fakeToolRunner{}
	author, fake, disk := serialTestSetup(t, tools)
	generated := NormalizeProposal(toolSpec("x"), "prop_c")
	installAll(t, author, toolSpec("leaf"), generated)
	run, err := disk.newRun(context.Background(), generated, &RunInput{}, ownerOrigin())
	if err != nil {
		t.Fatal(err)
	}
	driver := NewInterpreter(disk, durable.NewFake(), &Options{MaxConcurrentSteps: 2, Tools: tools})
	if err := driver.LoadAll(context.Background(), []*Spec{toolSpec("leaf")}, testValidationDeps()); err != nil {
		t.Fatal(err)
	}
	for _, spec := range driver.Definitions() {
		if spec.Source == SourceGenerated {
			t.Fatalf("a generated definition is listed: %s", spec.ID)
		}
	}
	inv, _ := holdInvocation(t, fake, "holder-stored")
	terminal, err := driver.DriveSerial(context.Background(), inv, run.ID)
	if err != nil || terminal != RunSucceeded {
		t.Fatalf("driving a stored generated definition = %s, %v", terminal, err)
	}
}
