package app

import (
	"testing"

	"github.com/anggasct/aura/internal/modules/workflow"
	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/principal"
)

func taintedOwner() *workflow.Caller {
	owner := caller(principal.ClassOwner, principal.OwnerID)
	owner.Tainted = true
	return owner
}

func TestTaintedOwnerTurnCannotCancelARunAndAnUntaintedOneCan(t *testing.T) {
	controller, runs, _ := workflowToolFixture(t, "book_visit")
	owner := caller(principal.ClassOwner, principal.OwnerID)
	started, err := controller.Start(t.Context(), owner, "refund", "refund order 42")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := controller.Cancel(t.Context(), taintedOwner(), started.RunID); !fault.HasCode(err, workflow.ErrorCodeNotPermitted) {
		t.Fatalf("tainted cancel = %v, want %s", err, workflow.ErrorCodeNotPermitted)
	}
	after, err := runs.Run(t.Context(), started.RunID)
	if err != nil || after.Status == workflow.RunCancelled {
		t.Fatalf("run after a refused cancel = %+v, %v", after, err)
	}
	if _, err := controller.Cancel(t.Context(), owner, started.RunID); err != nil {
		t.Fatalf("untainted cancel = %v", err)
	}
}

func TestTaintedOwnerTurnCannotSaveARoutineAndAnUntaintedOneCan(t *testing.T) {
	controller, runs, db := workflowToolFixture(t, "book_visit")
	runID := succeededGeneratedRun(t, controller, runs, db)
	if _, err := controller.SaveRoutine(t.Context(), taintedOwner(), runID, "Weekly report"); !fault.HasCode(err, workflow.ErrorCodeNotPermitted) {
		t.Fatalf("tainted save = %v, want %s", err, workflow.ErrorCodeNotPermitted)
	}
	routines, err := runs.Routines(t.Context())
	if err != nil || len(routines) != 0 {
		t.Fatalf("routines after a refused save = %+v, %v", routines, err)
	}
	saved, err := controller.SaveRoutine(t.Context(), caller(principal.ClassOwner, principal.OwnerID), runID, "Weekly report")
	if err != nil || saved.Version != 1 {
		t.Fatalf("untainted save = %+v, %v", saved, err)
	}
	if routines, err := runs.Routines(t.Context()); err != nil || len(routines) != 1 {
		t.Fatalf("routines after the untainted save = %+v, %v", routines, err)
	}
}

func TestRunStartedByATaintedOwnerTurnIsStoredTainted(t *testing.T) {
	controller, runs, _ := workflowToolFixture(t, "book_visit")
	cases := map[string]struct {
		caller *workflow.Caller
		want   bool
	}{
		"tainted owner caller":   {taintedOwner(), true},
		"untainted owner caller": {caller(principal.ClassOwner, principal.OwnerID), false},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			started, err := controller.Start(t.Context(), tc.caller, "refund", "")
			if err != nil {
				t.Fatal(err)
			}
			run, err := runs.Run(t.Context(), started.RunID)
			if err != nil || run.Origin.Tainted != tc.want {
				t.Fatalf("stored origin = %+v, %v, want tainted %t", run.Origin, err, tc.want)
			}
		})
	}
}
