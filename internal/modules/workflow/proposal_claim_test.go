package workflow

import (
	"testing"
	"time"

	"github.com/anggasct/aura/internal/platform/principal"
)

func proposalRunID(t *testing.T, disk *Store, proposalID string) string {
	t.Helper()
	proposal, err := disk.Proposal(t.Context(), proposalID, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return proposal.RunID
}

func TestTaintedProposalStartsOnce(t *testing.T) {
	startsOfADefinition := []struct {
		name  string
		start func(t *testing.T, interpreter *Interpreter, id string) error
	}{
		{name: "Start", start: func(t *testing.T, interpreter *Interpreter, id string) error {
			_, err := interpreter.Start(t.Context(), id, &RunInput{}, ownerOrigin())
			return err
		}},
		{name: "a keyed configured start", start: func(t *testing.T, interpreter *Interpreter, id string) error {
			_, err := interpreter.StartConfigured(t.Context(), id, &RunInput{}, "local-owner", principal.ClassOwner, "schedule-key")
			return err
		}},
		{name: "a configured start", start: func(t *testing.T, interpreter *Interpreter, id string) error {
			_, err := interpreter.StartConfigured(t.Context(), id, &RunInput{}, "local-owner", principal.ClassOwner, "")
			return err
		}},
		{name: "a child start", start: func(t *testing.T, interpreter *Interpreter, id string) error {
			_, err := interpreter.createChildRun(t.Context(), id, &RunInput{}, "run-parent", ownerOrigin())
			return err
		}},
	}

	t.Run("its own start claims it once", func(t *testing.T) {
		interpreter, _, disk := serialTestSetup(t, &fakeToolRunner{})
		approveGenerated(t, interpreter, disk, "prop_once", true)
		run, err := interpreter.StartApproved(t.Context(), "prop_once", &RunInput{}, ownerOrigin())
		if err != nil {
			t.Fatal(err)
		}
		if got := proposalRunID(t, disk, "prop_once"); got != run.ID {
			t.Fatalf("proposal run_id = %q, want %q", got, run.ID)
		}
		if _, err := interpreter.StartApproved(t.Context(), "prop_once", &RunInput{}, ownerOrigin()); !hasCode(err, ErrorCodeApprovalSpent) {
			t.Fatalf("second StartApproved = %v, want %s", err, ErrorCodeApprovalSpent)
		}
		if got := countRuns(t, disk); got != 1 {
			t.Fatalf("workflow_run rows = %d, want 1", got)
		}
	})

	for _, claimed := range []bool{false, true} {
		for _, tc := range startsOfADefinition {
			name := tc.name + " before its own start"
			if claimed {
				name = tc.name + " after its own start"
			}
			t.Run(name, func(t *testing.T) {
				interpreter, _, disk := serialTestSetup(t, &fakeToolRunner{})
				generated := approveGenerated(t, interpreter, disk, "prop_other", true)
				want := 0
				if claimed {
					if _, err := interpreter.StartApproved(t.Context(), "prop_other", &RunInput{}, ownerOrigin()); err != nil {
						t.Fatal(err)
					}
					want = 1
				}
				if err := tc.start(t, interpreter, generated.ID); !hasCode(err, ErrorCodeApprovalSpent) {
					t.Fatalf("start = %v, want %s", err, ErrorCodeApprovalSpent)
				}
				if got := countRuns(t, disk); got != want {
					t.Fatalf("workflow_run rows = %d, want %d", got, want)
				}
				if !claimed && proposalRunID(t, disk, "prop_other") != "" {
					t.Fatal("a refused start claimed the proposal")
				}
			})
		}
	}

	t.Run("a root composing it is refused before any step runs", func(t *testing.T) {
		tools := &fakeToolRunner{}
		interpreter, _, disk := serialTestSetup(t, tools)
		generated := approveGenerated(t, interpreter, disk, "prop_composed", true)
		installAll(t, interpreter, composingSpec("top", generated.ID))
		if _, err := interpreter.Start(t.Context(), "top", &RunInput{}, ownerOrigin()); !hasCode(err, ErrorCodeApprovalSpent) {
			t.Fatalf("Start of the composing root = %v, want %s", err, ErrorCodeApprovalSpent)
		}
		if got := countRuns(t, disk); got != 0 || len(tools.called) != 0 {
			t.Fatalf("runs = %d, tool calls = %v; want none", got, tools.called)
		}
		if proposalRunID(t, disk, "prop_composed") != "" {
			t.Fatal("a refused root claimed the proposal")
		}
	})

	t.Run("an untainted proposal starts twice and records its first run", func(t *testing.T) {
		interpreter, _, disk := serialTestSetup(t, &fakeToolRunner{})
		generated := approveGenerated(t, interpreter, disk, "prop_free", false)
		first, err := interpreter.Start(t.Context(), generated.ID, &RunInput{}, ownerOrigin())
		if err != nil {
			t.Fatal(err)
		}
		if _, err := interpreter.Start(t.Context(), generated.ID, &RunInput{}, ownerOrigin()); err != nil {
			t.Fatalf("second Start of an untainted proposal: %v", err)
		}
		if _, err := interpreter.StartApproved(t.Context(), "prop_free", &RunInput{}, ownerOrigin()); err != nil {
			t.Fatalf("StartApproved after another run claimed an untainted proposal: %v", err)
		}
		if got := proposalRunID(t, disk, "prop_free"); got != first.ID {
			t.Fatalf("proposal run_id = %q, want the first run %q", got, first.ID)
		}
		if got := countRuns(t, disk); got != 3 {
			t.Fatalf("workflow_run rows = %d, want 3", got)
		}
	})

	t.Run("a start that cannot be written leaves the claim free", func(t *testing.T) {
		interpreter, _, disk := serialTestSetup(t, &fakeToolRunner{})
		approveGenerated(t, interpreter, disk, "prop_atomic_claim", true)
		if _, err := disk.db.ExecContext(t.Context(), `CREATE TRIGGER abort_step_rows BEFORE INSERT ON workflow_step_run BEGIN SELECT RAISE(ABORT, 'no step rows'); END`); err != nil {
			t.Fatal(err)
		}
		if _, err := interpreter.StartApproved(t.Context(), "prop_atomic_claim", &RunInput{}, ownerOrigin()); err == nil {
			t.Fatal("StartApproved succeeded although its step rows cannot be written")
		}
		if got := proposalRunID(t, disk, "prop_atomic_claim"); got != "" {
			t.Fatalf("proposal run_id = %q after a failed start, want none", got)
		}
		if got := countRuns(t, disk); got != 0 {
			t.Fatalf("workflow_run rows = %d, want none", got)
		}
	})

	t.Run("a claim that cannot be written leaves no run", func(t *testing.T) {
		interpreter, _, disk := serialTestSetup(t, &fakeToolRunner{})
		approveGenerated(t, interpreter, disk, "prop_atomic_run", true)
		if _, err := disk.db.ExecContext(t.Context(), `CREATE TRIGGER abort_claims BEFORE UPDATE OF run_id ON workflow_proposal BEGIN SELECT RAISE(ABORT, 'no claims'); END`); err != nil {
			t.Fatal(err)
		}
		if _, err := interpreter.StartApproved(t.Context(), "prop_atomic_run", &RunInput{}, ownerOrigin()); err == nil {
			t.Fatal("StartApproved succeeded although its claim cannot be written")
		}
		if got := countRuns(t, disk); got != 0 {
			t.Fatalf("workflow_run rows = %d, want none", got)
		}
	})

	t.Run("a proposal that was never approved has no run to claim", func(t *testing.T) {
		interpreter, _, _ := serialTestSetup(t, &fakeToolRunner{})
		if _, err := interpreter.StartApproved(t.Context(), "prop_unknown", &RunInput{}, ownerOrigin()); !hasCode(err, ErrorCodeApprovalMissing) {
			t.Fatalf("StartApproved = %v, want %s", err, ErrorCodeApprovalMissing)
		}
	})
}

func TestRoutineIsNotSavedFromATaintedDefinition(t *testing.T) {
	t.Run("a run of a tainted proposal", func(t *testing.T) {
		interpreter, _, disk := driverTestSetup(t, &fakeToolRunner{}, DriverConcurrent)
		approveGenerated(t, interpreter, disk, "prop_tainted_routine", true)
		run, err := interpreter.StartApproved(t.Context(), "prop_tainted_routine", &RunInput{}, ownerOrigin())
		if err != nil {
			t.Fatal(err)
		}
		waitForRunStatus(t, disk, run.ID, RunSucceeded, 5*time.Second)
		if _, err := interpreter.SaveRoutine(t.Context(), run.ID, "Weekly report"); !hasCode(err, ErrorCodeRoutineInvalid) {
			t.Fatalf("SaveRoutine = %v, want %s", err, ErrorCodeRoutineInvalid)
		}
		if _, err := disk.LatestDefinition(t.Context(), "routine_weekly_report"); !hasCode(err, ErrorCodeDefinitionNotFound) {
			t.Fatalf("routine definition after the refusal = %v, want %s", err, ErrorCodeDefinitionNotFound)
		}
	})

	t.Run("a tainted run of an untainted proposal", func(t *testing.T) {
		interpreter, _, disk := driverTestSetup(t, &fakeToolRunner{}, DriverConcurrent)
		generated := approveGenerated(t, interpreter, disk, "prop_clean_routine", false)
		origin := ownerOrigin()
		origin.Tainted = true
		run, err := interpreter.Start(t.Context(), generated.ID, &RunInput{}, origin)
		if err != nil {
			t.Fatal(err)
		}
		waitForRunStatus(t, disk, run.ID, RunSucceeded, 5*time.Second)
		stored, err := disk.Run(t.Context(), run.ID)
		if err != nil || !stored.Origin.Tainted {
			t.Fatalf("run = %+v, %v; want a tainted origin", stored, err)
		}
		routine, err := interpreter.SaveRoutine(t.Context(), run.ID, "Weekly report")
		if err != nil {
			t.Fatalf("SaveRoutine: %v", err)
		}
		record, err := disk.Approval(t.Context(), routine.ID, routine.Version)
		if err != nil || record.Tainted || record.Kind != ApprovalRoutine {
			t.Fatalf("routine record = %+v, %v; want an untainted routine record", record, err)
		}
	})
}
