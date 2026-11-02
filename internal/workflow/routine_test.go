package workflow

import (
	"context"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/durable"
)

func TestRoutineIDsAreSlugsOfValidNames(t *testing.T) {
	for name, want := range map[string]string{"Weekly Report": "routine_weekly_report", " ship-it_now ": "routine_ship_it_now", "a": "routine_a"} {
		if got, err := RoutineID(name); err != nil || got != want {
			t.Errorf("RoutineID(%q) = %q, %v, want %q", name, got, err, want)
		}
	}
	for _, name := range []string{"", "   ", "rm -rf /", "naïve", string(make([]byte, 65))} {
		if _, err := RoutineID(name); !hasCode(err, ErrorCodeRoutineInvalid) {
			t.Errorf("RoutineID(%q) = %v, want workflow_routine_invalid", name, err)
		}
	}
}

func TestSavedRoutinesVersionAndStayStartable(t *testing.T) {
	tools := &fakeToolRunner{}
	interpreter, _, disk := serialTestSetup(t, tools)
	generated := NormalizeProposal(toolSpec("x"), "prop_r")
	loadAll(t, interpreter, generated, threeToolSteps("defined", SourceDefined))
	run, err := interpreter.Start(context.Background(), generated.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := interpreter.SaveRoutine(context.Background(), run.ID, "Weekly report"); !hasCode(err, ErrorCodeRoutineInvalid) {
		t.Fatalf("saving an unfinished run = %v", err)
	}
	waitForRunStatus(t, disk, run.ID, RunSucceeded, 5*time.Second)
	first, err := interpreter.SaveRoutine(context.Background(), run.ID, "Weekly report")
	if err != nil || first.ID != "routine_weekly_report" || first.Version != 1 || first.Name != "Weekly report" || first.Source != SourceGenerated {
		t.Fatalf("first save = %+v, %v", first, err)
	}
	second, err := interpreter.SaveRoutine(context.Background(), run.ID, "Weekly report")
	if err != nil || second.Version != 2 {
		t.Fatalf("second save = %+v, %v", second, err)
	}
	if stored, err := disk.Definition(context.Background(), first.ID, 1); err != nil || stored.Version != 1 {
		t.Fatalf("version 1 after saving version 2 = %+v, %v", stored, err)
	}
	defined, err := interpreter.Start(context.Background(), "defined", nil)
	if err != nil {
		t.Fatal(err)
	}
	waitForRunStatus(t, disk, defined.ID, RunSucceeded, 5*time.Second)
	if _, err := interpreter.SaveRoutine(context.Background(), defined.ID, "Not generated"); !hasCode(err, ErrorCodeRoutineInvalid) {
		t.Fatalf("saving a defined run = %v", err)
	}
	listed := map[string]bool{}
	for _, spec := range interpreter.Definitions() {
		listed[spec.ID] = true
	}
	if !listed[first.ID] || listed[generated.ID] || !listed["defined"] {
		t.Fatalf("listed = %v: routines and definitions are listed, proposal definitions are not", listed)
	}
	routines, err := disk.Routines(context.Background())
	if err != nil || len(routines) != 1 || routines[0].Version != 2 {
		t.Fatalf("Routines = %+v, %v", routines, err)
	}
	fresh := NewInterpreter(disk, durable.NewFake(), &Options{Tools: tools})
	loadAll(t, fresh, toolSpec("leaf"))
	started, err := fresh.StartKeyed(context.Background(), first.ID, &RunInput{}, nil, "sched-once")
	if err != nil || started.DefinitionVersion != 2 {
		t.Fatalf("starting a routine that is only in the store = %+v, %v", started, err)
	}
	again, err := fresh.StartKeyed(context.Background(), first.ID, &RunInput{}, nil, "sched-once")
	if err != nil || again.ID != started.ID {
		t.Fatalf("a replayed keyed start created %v, %v instead of returning %s", again, err, started.ID)
	}
}
