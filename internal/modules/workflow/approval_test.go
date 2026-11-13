package workflow

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/platform/durable"
)

const staleDigest = "ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"

func approvalCount(t *testing.T, disk *Store) int {
	t.Helper()
	var count int
	if err := disk.db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM workflow_approval`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

func proposeSpec(t *testing.T, disk *Store, id, agent string, tainted bool, spec *Spec) *Proposal {
	t.Helper()
	proposalSession(t, disk, "session-"+id)
	digest, err := Digest(spec)
	if err != nil {
		t.Fatal(err)
	}
	proposal := &Proposal{ID: id, SessionID: "session-" + id, AgentID: agent, PrincipalID: "local-owner", Tainted: tainted, Spec: spec, Digest: digest, CreatedAt: time.Now().UTC()}
	if err := disk.CreateProposal(t.Context(), proposal); err != nil {
		t.Fatal(err)
	}
	return proposal
}

func proposalApproval(proposal *Proposal) *Approval {
	return &Approval{
		DefinitionID: proposal.Spec.ID, DefinitionVersion: proposal.Spec.Version, Digest: proposal.Digest,
		AgentID: proposal.AgentID, ApproverID: "local-owner", Tainted: proposal.Tainted,
	}
}

func TestInstalledDefinitionIsRecordedAtLoad(t *testing.T) {
	interpreter, _, disk := serialTestSetup(t, &fakeToolRunner{})
	declaredGenerated := toolSpec("declared_generated")
	declaredGenerated.Source = SourceGenerated
	files := []*Spec{toolSpec("installed_file"), declaredGenerated}
	if err := interpreter.Install(t.Context(), files, testValidationDeps()); err != nil {
		t.Fatal(err)
	}
	for _, spec := range files {
		digest, err := Digest(spec)
		if err != nil {
			t.Fatal(err)
		}
		record, err := disk.Approval(t.Context(), spec.ID, spec.Version)
		if err != nil {
			t.Fatalf("approval of %s: %v", spec.ID, err)
		}
		if record.Kind != ApprovalInstalled || record.Digest != digest || record.ApproverID != "local-owner" || record.AgentID != "" || record.Tainted || record.ProposalID != "" || record.SourceDefinitionID != "" || record.ApprovedAt.IsZero() {
			t.Fatalf("approval of %s = %+v, want an installed record of digest %s approved by the owner", spec.ID, record, digest)
		}
	}
	first, err := disk.Approval(t.Context(), "installed_file", 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := interpreter.Install(t.Context(), files, testValidationDeps()); err != nil {
		t.Fatalf("second Install: %v", err)
	}
	again, err := disk.Approval(t.Context(), "installed_file", 1)
	if err != nil || *again != *first || approvalCount(t, disk) != 2 {
		t.Fatalf("second Install changed the record: %+v then %+v, %v, %d records", first, again, err, approvalCount(t, disk))
	}
	generated := NormalizeProposal(toolSpec("x"), "prop_loaded")
	if err := interpreter.LoadAll(t.Context(), []*Spec{generated}, testValidationDeps()); err != nil {
		t.Fatal(err)
	}
	if _, err := disk.Definition(t.Context(), generated.ID, generated.Version); err != nil {
		t.Fatalf("LoadAll did not save the generated definition: %v", err)
	}
	if _, err := disk.Approval(t.Context(), generated.ID, generated.Version); !hasCode(err, ErrorCodeApprovalMissing) {
		t.Fatalf("approval of a loaded generated definition = %v, want %s", err, ErrorCodeApprovalMissing)
	}
}

func TestStartRefusesAnUnapprovedDefinition(t *testing.T) {
	for _, tc := range originDrivers {
		driver := tc.driver
		t.Run(tc.name, func(t *testing.T) {
			tools := &fakeToolRunner{}
			interpreter, _, disk := driverTestSetup(t, tools, driver)
			generated := NormalizeProposal(toolSpec("x"), "prop_unapproved")
			if err := interpreter.LoadAll(t.Context(), []*Spec{generated}, testValidationDeps()); err != nil {
				t.Fatal(err)
			}
			if _, err := interpreter.Start(t.Context(), generated.ID, &RunInput{}, ownerOrigin()); !hasCode(err, ErrorCodeApprovalMissing) {
				t.Fatalf("Start = %v, want %s", err, ErrorCodeApprovalMissing)
			}
			if _, err := interpreter.StartKeyed(t.Context(), generated.ID, &RunInput{}, ownerOrigin(), "sched-unapproved"); !hasCode(err, ErrorCodeApprovalMissing) {
				t.Fatalf("StartKeyed = %v, want %s", err, ErrorCodeApprovalMissing)
			}
			if got := countRuns(t, disk); got != 0 {
				t.Fatalf("%d workflow_run rows written for an unapproved definition, want none", got)
			}
			if len(tools.called) != 0 {
				t.Fatalf("the runner was called for an unapproved definition: %v", tools.called)
			}
		})
	}
}

func TestApprovalRecordIsBoundToTheDigest(t *testing.T) {
	t.Run("a record of another digest does not approve the saved definition", func(t *testing.T) {
		interpreter, _, disk := serialTestSetup(t, &fakeToolRunner{})
		installAll(t, interpreter, toolSpec("leaf"))
		if _, err := disk.db.ExecContext(t.Context(), `UPDATE workflow_approval SET spec_sha256 = ? WHERE definition_id = 'leaf'`, staleDigest); err != nil {
			t.Fatal(err)
		}
		if _, err := interpreter.Start(t.Context(), "leaf", &RunInput{}, ownerOrigin()); !hasCode(err, ErrorCodeApprovalMissing) {
			t.Fatalf("Start = %v, want %s", err, ErrorCodeApprovalMissing)
		}
		if got := countRuns(t, disk); got != 0 {
			t.Fatalf("%d workflow_run rows written, want none", got)
		}
	})
	t.Run("a second approval of the same version with another digest is refused", func(t *testing.T) {
		disk := newTestStore(t)
		spec := NormalizeProposal(toolSpec("x"), "prop_twice")
		if err := disk.SaveDefinition(t.Context(), spec); err != nil {
			t.Fatal(err)
		}
		first := proposeSpec(t, disk, "prop_first", "main", false, spec)
		second := proposeSpec(t, disk, "prop_second", "main", false, spec)
		if err := disk.ApproveProposal(t.Context(), first.ID, proposalApproval(first)); err != nil {
			t.Fatal(err)
		}
		other := proposalApproval(second)
		other.Digest = staleDigest
		if err := disk.ApproveProposal(t.Context(), second.ID, other); !hasCode(err, ErrorCodeApprovalConflict) {
			t.Fatalf("second approval with another digest = %v, want %s", err, ErrorCodeApprovalConflict)
		}
		record, err := disk.Approval(t.Context(), spec.ID, spec.Version)
		if err != nil || record.Digest != first.Digest || record.ProposalID != first.ID {
			t.Fatalf("record after the refused approval = %+v, %v, want the first one", record, err)
		}
	})
}

func TestChildOfUnapprovedDefinitionFailsTheParentStep(t *testing.T) {
	for _, tc := range originDrivers {
		driver := tc.driver
		t.Run(tc.name, func(t *testing.T) {
			tools := &stepCallTools{}
			interpreter, fake, disk := driverTestSetup(t, tools, driver)
			leaf := toolSpec("leaf_unapproved")
			if err := interpreter.LoadAll(t.Context(), []*Spec{leaf}, testValidationDeps()); err != nil {
				t.Fatal(err)
			}
			installAll(t, interpreter, composingSpec("top", "leaf_unapproved"))
			run, err := disk.CreateRun(t.Context(), specOf(t, interpreter, "top"), &RunInput{}, ownerOrigin())
			if err != nil {
				t.Fatal(err)
			}
			if driver == DriverSerial {
				inv, _ := holdInvocation(t, fake, "holder-unapproved-child")
				if terminal, err := interpreter.DriveSerial(t.Context(), inv, run.ID); err != nil || terminal != RunFailed {
					t.Fatalf("DriveSerial = %s, %v; want a failed run", terminal, err)
				}
			} else {
				if _, err := fake.Start(t.Context(), durable.StartRequest{
					Handler: HandlerName, Key: "workflow/" + run.ID, Payload: mustEncode(tickPayload{RunID: run.ID, DefinitionID: "top", Version: 1}),
				}); err != nil {
					t.Fatal(err)
				}
				waitForRunStatus(t, disk, run.ID, RunFailed, 5*time.Second)
				steps, err := disk.Steps(t.Context(), run.ID)
				if err != nil || len(steps) != 1 {
					t.Fatalf("Steps = %d, %v; want one", len(steps), err)
				}
				if steps[0].Status != StepFailed || steps[0].ErrorCode != string(ErrorCodeChildFailed) {
					t.Fatalf("parent step = %s/%s, want failed/%s", steps[0].Status, steps[0].ErrorCode, ErrorCodeChildFailed)
				}
			}
			if children := childRuns(t, disk, "leaf_unapproved"); len(children) != 0 {
				t.Fatalf("%d child runs created for an unapproved definition, want none", len(children))
			}
			if calls := tools.recorded(); len(calls) != 0 {
				t.Fatalf("the runner was called %d times for an unapproved child", len(calls))
			}
		})
	}
}

func TestToolStepOfUnapprovedDigestNeverReachesTheRunner(t *testing.T) {
	for _, tc := range originDrivers {
		driver := tc.driver
		t.Run(tc.name, func(t *testing.T) {
			tools := &stepCallTools{}
			interpreter, fake, disk := driverTestSetup(t, tools, driver)
			spec := toolSpec("leaf")
			installAll(t, interpreter, spec)
			run, err := disk.CreateRun(t.Context(), spec, &RunInput{}, ownerOrigin())
			if err != nil {
				t.Fatal(err)
			}
			if _, err := disk.db.ExecContext(t.Context(), `DELETE FROM workflow_approval WHERE definition_id = 'leaf'`); err != nil {
				t.Fatal(err)
			}
			if driver == DriverSerial {
				inv, _ := holdInvocation(t, fake, "holder-unapproved-step")
				if terminal, err := interpreter.DriveSerial(t.Context(), inv, run.ID); err != nil || terminal != RunFailed {
					t.Fatalf("DriveSerial = %s, %v; want a failed run", terminal, err)
				}
			} else {
				if _, err := fake.Start(t.Context(), durable.StartRequest{
					Handler: HandlerName, Key: "workflow/" + run.ID, Payload: mustEncode(tickPayload{RunID: run.ID, DefinitionID: spec.ID, Version: spec.Version}),
				}); err != nil {
					t.Fatal(err)
				}
				waitForRunStatus(t, disk, run.ID, RunFailed, 5*time.Second)
			}
			steps, err := disk.Steps(t.Context(), run.ID)
			if err != nil || len(steps) != 1 {
				t.Fatalf("Steps = %d, %v; want one", len(steps), err)
			}
			if steps[0].Status != StepFailed || steps[0].ErrorCode != string(ErrorCodeApprovalMissing) {
				t.Fatalf("step = %s/%s, want failed/%s", steps[0].Status, steps[0].ErrorCode, ErrorCodeApprovalMissing)
			}
			if calls := tools.recorded(); len(calls) != 0 {
				t.Fatalf("the runner was called %d times for a tool step of an unapproved digest", len(calls))
			}
		})
	}
}

func TestApproveProposalRecordsInOneTransaction(t *testing.T) {
	disk := newTestStore(t)
	spec := NormalizeProposal(toolSpec("x"), "prop_atomic")
	if err := disk.SaveDefinition(t.Context(), spec); err != nil {
		t.Fatal(err)
	}
	proposal := proposeSpec(t, disk, "prop_atomic", "ops", true, spec)
	if _, err := disk.db.ExecContext(t.Context(),
		`INSERT INTO workflow_approval (definition_id, definition_version, spec_sha256, kind, approver_id, approved_at) VALUES (?, ?, ?, 'installed', 'local-owner', ?)`,
		spec.ID, spec.Version, staleDigest, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	if err := disk.ApproveProposal(t.Context(), proposal.ID, proposalApproval(proposal)); !hasCode(err, ErrorCodeApprovalConflict) {
		t.Fatalf("ApproveProposal = %v, want %s", err, ErrorCodeApprovalConflict)
	}
	stored, err := disk.Proposal(t.Context(), proposal.ID, time.Now())
	if err != nil || stored.Status != ProposalProposed || !stored.DecidedAt.IsZero() {
		t.Fatalf("proposal after the refused approval = %+v, %v, want it still proposed", stored, err)
	}
	if got := approvalCount(t, disk); got != 1 {
		t.Fatalf("%d approval records, want only the pre-existing one", got)
	}
	if _, err := disk.db.ExecContext(t.Context(), `DELETE FROM workflow_approval`); err != nil {
		t.Fatal(err)
	}
	if err := disk.ApproveProposal(t.Context(), proposal.ID, proposalApproval(proposal)); err != nil {
		t.Fatal(err)
	}
	record, err := disk.Approval(t.Context(), spec.ID, spec.Version)
	if err != nil {
		t.Fatal(err)
	}
	if record.Kind != ApprovalProposal || record.ProposalID != proposal.ID || record.AgentID != "ops" || record.ApproverID != "local-owner" || !record.Tainted || record.Digest != proposal.Digest {
		t.Fatalf("record = %+v, want a proposal record of %s by ops, tainted", record, proposal.ID)
	}
	decided, err := disk.Proposal(t.Context(), proposal.ID, time.Now())
	if err != nil || decided.Status != ProposalApproved {
		t.Fatalf("proposal after approval = %+v, %v", decided, err)
	}
	if err := disk.ApproveProposal(t.Context(), proposal.ID, proposalApproval(proposal)); !hasCode(err, ErrorCodeProposalDecided) {
		t.Fatalf("approving twice = %v, want %s", err, ErrorCodeProposalDecided)
	}
}

func TestRoutineCarriesItsSourceApproval(t *testing.T) {
	t.Run("a routine of an approved tainted proposal run", func(t *testing.T) {
		interpreter, _, disk := driverTestSetup(t, &fakeToolRunner{}, DriverConcurrent)
		generated := NormalizeProposal(toolSpec("x"), "prop_routine")
		if err := interpreter.LoadAll(t.Context(), []*Spec{generated}, testValidationDeps()); err != nil {
			t.Fatal(err)
		}
		proposal := proposeSpec(t, disk, "prop_routine", "ops", true, generated)
		if err := disk.ApproveProposal(t.Context(), proposal.ID, proposalApproval(proposal)); err != nil {
			t.Fatal(err)
		}
		run, err := interpreter.Start(t.Context(), generated.ID, &RunInput{}, ownerOrigin())
		if err != nil {
			t.Fatal(err)
		}
		waitForRunStatus(t, disk, run.ID, RunSucceeded, 5*time.Second)
		routine, err := interpreter.SaveRoutine(t.Context(), run.ID, "Weekly report")
		if err != nil {
			t.Fatal(err)
		}
		digest, err := Digest(routine)
		if err != nil {
			t.Fatal(err)
		}
		record, err := disk.Approval(t.Context(), routine.ID, routine.Version)
		if err != nil {
			t.Fatal(err)
		}
		want := Approval{
			DefinitionID: routine.ID, DefinitionVersion: routine.Version, Digest: digest, Kind: ApprovalRoutine, AgentID: "ops", ApproverID: "local-owner",
			SourceDefinitionID: generated.ID, SourceDefinitionVersion: generated.Version, Tainted: true,
		}
		record.ApprovedAt = time.Time{}
		if *record != want {
			t.Fatalf("routine record = %+v, want %+v", *record, want)
		}
		if started, err := interpreter.Start(t.Context(), routine.ID, &RunInput{}, ownerOrigin()); err != nil || started.DefinitionVersion != routine.Version {
			t.Fatalf("starting the saved routine = %+v, %v", started, err)
		}
	})
	t.Run("a run whose definition has no record", func(t *testing.T) {
		interpreter, _, disk := driverTestSetup(t, &fakeToolRunner{}, DriverConcurrent)
		generated := NormalizeProposal(toolSpec("x"), "prop_unrecorded")
		if err := interpreter.LoadAll(t.Context(), []*Spec{generated}, testValidationDeps()); err != nil {
			t.Fatal(err)
		}
		run, err := disk.CreateRun(t.Context(), generated, &RunInput{}, ownerOrigin())
		if err != nil {
			t.Fatal(err)
		}
		if err := disk.SetRunStatus(t.Context(), run.ID, RunSucceeded); err != nil {
			t.Fatal(err)
		}
		if _, err := interpreter.SaveRoutine(t.Context(), run.ID, "Weekly report"); !hasCode(err, ErrorCodeApprovalMissing) {
			t.Fatalf("SaveRoutine = %v, want %s", err, ErrorCodeApprovalMissing)
		}
		if _, err := disk.LatestDefinition(t.Context(), "routine_weekly_report"); !hasCode(err, ErrorCodeDefinitionNotFound) {
			t.Fatalf("a routine was saved without a source approval: %v", err)
		}
		if got := approvalCount(t, disk); got != 0 {
			t.Fatalf("%d approval records written, want none", got)
		}
	})
}

func TestPlanShowsEveryToolCall(t *testing.T) {
	event := "ci.done"
	spec := &Spec{ID: "gen_plan", Goal: "Ship the fix", Version: 1, Source: SourceGenerated, Steps: []StepSpec{
		{ID: "read", Executor: ExecutorSpec{Kind: KindTool, ToolID: ptr("read_file"), ToolArgs: json.RawMessage(`{"path":"a"}`)}, Timeout: time.Minute},
		{ID: "gate", DependsOn: []string{"read"}, Executor: ExecutorSpec{Kind: KindApproval}, Timeout: time.Minute},
		{ID: "write", DependsOn: []string{"gate"}, Executor: ExecutorSpec{Kind: KindTool, ToolID: ptr("write_file"), ToolArgs: json.RawMessage(`{"path":"b","content":"c"}`)}, Timeout: time.Minute},
		{ID: "ci", DependsOn: []string{"write"}, Executor: ExecutorSpec{Kind: KindWait, Event: &event}, Timeout: time.Minute},
	}}
	graph, err := Compile(spec, testValidationDeps())
	if err != nil {
		t.Fatal(err)
	}
	shown := func(toolID string, args json.RawMessage) (string, error) {
		return "target of " + toolID + "\nargs " + string(args), nil
	}
	plan, err := RenderPlan(spec, graph, shown)
	if err != nil {
		t.Fatal(err)
	}
	want := strings.Join([]string{
		"Plan: Ship the fix",
		"1. read — run tool read_file",
		"    target of read_file",
		`    args {"path":"a"}`,
		"2. gate — owner approval (after read)",
		"3. write — run tool write_file (after gate)",
		"    target of write_file",
		`    args {"path":"b","content":"c"}`,
		"4. ci — wait for ci.done (after write)",
	}, "\n")
	if plan != want {
		t.Fatalf("plan:\n%s\nwant:\n%s", plan, want)
	}
	refusing := func(toolID string, _ json.RawMessage) (string, error) {
		if toolID == "write_file" {
			return "", errors.New("arguments do not fit the approval display")
		}
		return "shown", nil
	}
	for name, show := range map[string]ToolCallText{"port error": refusing, "no port": nil} {
		plan, err := RenderPlan(spec, graph, show)
		if !hasCode(err, ErrorCodePlanUnshowable) || plan != "" {
			t.Fatalf("%s: RenderPlan = %q, %v, want no plan and %s", name, plan, err, ErrorCodePlanUnshowable)
		}
		wantStep := `step "write"`
		if name == "no port" {
			wantStep = `step "read"`
		}
		if !strings.Contains(err.Error(), wantStep) {
			t.Fatalf("%s: error %q does not name %s", name, err, wantStep)
		}
	}
}
