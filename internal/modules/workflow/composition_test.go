package workflow

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/platform/durable/durabletest"
)

func toolSpec(id string) *Spec {
	tool := "read_file"
	return &Spec{ID: id, Goal: "Run " + id, Version: 1, Source: SourceDefined, Steps: []StepSpec{
		{ID: "read", Executor: ExecutorSpec{Kind: KindTool, ToolID: &tool, ToolArgs: json.RawMessage(`{"path":"a"}`)}, Timeout: 5 * time.Second},
	}}
}

func composingSpec(id string, children ...string) *Spec {
	spec := &Spec{ID: id, Goal: "Compose " + id, Version: 1, Source: SourceComposed}
	for index, child := range children {
		step := StepSpec{ID: "child" + string(rune('a'+index)), Executor: ExecutorSpec{Kind: KindWorkflow, WorkflowID: ptr(child)}, Timeout: 5 * time.Second}
		if index > 0 {
			step.DependsOn = []string{spec.Steps[index-1].ID}
		}
		spec.Steps = append(spec.Steps, step)
	}
	return spec
}

func setOf(specs ...*Spec) func(id string) (*Spec, bool) {
	byID := map[string]*Spec{}
	for _, spec := range specs {
		byID[spec.ID] = spec
	}
	return func(id string) (*Spec, bool) {
		spec, ok := byID[id]
		return spec, ok
	}
}

func compileComposed(spec *Spec, lookup func(string) (*Spec, bool)) error {
	if _, err := Compile(spec, testValidationDeps()); err != nil {
		return err
	}
	return ValidateComposition(spec, lookup)
}

func TestCompositionValidatesReferencesDepthAndCycles(t *testing.T) {
	leaf := toolSpec("leaf")
	two := composingSpec("two", "leaf")
	three := composingSpec("three", "two")
	four := composingSpec("four", "three")
	self := composingSpec("self", "self")
	ping := composingSpec("ping", "pong")
	pong := composingSpec("pong", "ping")
	lookup := setOf(leaf, two, three, four, self, ping, pong)
	for _, spec := range []*Spec{leaf, two, three} {
		if err := compileComposed(spec, lookup); err != nil {
			t.Fatalf("%s: %v", spec.ID, err)
		}
	}
	for label, tc := range map[string]struct {
		spec   *Spec
		lookup func(string) (*Spec, bool)
		want   string
	}{
		"four levels deep":       {spec: four, lookup: lookup, want: "4 levels"},
		"reaches itself":         {spec: self, lookup: lookup, want: "self -> self"},
		"mutual recursion":       {spec: ping, lookup: lookup, want: "ping -> pong -> ping"},
		"unknown child":          {spec: composingSpec("lost", "nowhere"), lookup: lookup, want: `unknown workflow "nowhere"`},
		"no workflow set loaded": {spec: two, want: "no workflow set"},
	} {
		if err := compileComposed(tc.spec, tc.lookup); !hasCode(err, ErrorCodeCompositionInvalid) || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want %s mentioning %q", label, err, ErrorCodeCompositionInvalid, tc.want)
		}
	}
	extras := composingSpec("extras", "leaf")
	extras.Steps[0].Executor.ToolID = ptr("read_file")
	if err := compileComposed(extras, lookup); !hasCode(err, ErrorCodeExecutorInvalid) {
		t.Errorf("workflow step with a tool: %v", err)
	}
	missing := composingSpec("missing", "leaf")
	missing.Steps[0].Executor.WorkflowID = nil
	if err := compileComposed(missing, lookup); !hasCode(err, ErrorCodeExecutorInvalid) {
		t.Errorf("workflow step without an id: %v", err)
	}
}

func hasCode(err error, want ErrorCode) bool {
	code, ok := CodeOf(err)
	return ok && code == want
}

func TestLoaderReadsTheWorkflowExecutor(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "parent.yaml")
	body := "id: parent\nversion: 1\ngoal: Parent\nsource: composed\nsteps:\n  - id: sub\n    timeout: 5s\n    executor:\n      kind: workflow\n      workflow: leaf\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	spec, err := LoadSpecFile(path)
	if err != nil || spec.Steps[0].Executor.Kind != KindWorkflow || spec.Steps[0].Executor.WorkflowID == nil || *spec.Steps[0].Executor.WorkflowID != "leaf" {
		t.Fatalf("LoadSpecFile = %+v, %v", spec, err)
	}
}

func installAll(t *testing.T, interpreter *Interpreter, specs ...*Spec) {
	t.Helper()
	if err := interpreter.Install(context.Background(), specs, testValidationDeps()); err != nil {
		t.Fatalf("Install: %v", err)
	}
}

func TestLoadRejectsAChildThatIsNotLoaded(t *testing.T) {
	interpreter, _, _ := serialTestSetup(t, &fakeToolRunner{})
	if err := interpreter.Install(context.Background(), []*Spec{composingSpec("top", "leaf")}, testValidationDeps()); !hasCode(err, ErrorCodeCompositionInvalid) {
		t.Fatalf("Load of a parent before its child = %v", err)
	}
	installAll(t, interpreter, toolSpec("leaf"))
	if err := interpreter.Install(context.Background(), []*Spec{composingSpec("top", "leaf")}, testValidationDeps()); err != nil {
		t.Fatalf("Load after the child is loaded: %v", err)
	}
}

func childRuns(t *testing.T, disk *Store, definitionID string) []*RunSummary {
	t.Helper()
	runs, err := disk.Runs(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	var matched []*RunSummary
	for _, run := range runs {
		if run.DefinitionID == definitionID {
			matched = append(matched, run)
		}
	}
	return matched
}

func TestComposedRunDrivesChildrenInProcess(t *testing.T) {
	tools := &fakeToolRunner{output: func(string) json.RawMessage { return json.RawMessage(`{"ok":true}`) }}
	interpreter, _, disk := driverTestSetup(t, tools, DriverConcurrent)
	interpreter.options.MaxConcurrentSteps = 1
	installAll(t, interpreter, toolSpec("leaf"), composingSpec("middle", "leaf"), composingSpec("top", "middle", "leaf"))
	summary, err := interpreter.Start(context.Background(), "top", &RunInput{Objective: "nest"}, ownerOrigin())
	if err != nil {
		t.Fatal(err)
	}
	waitForRunStatus(t, disk, summary.ID, RunSucceeded, 5*time.Second)
	leaves := childRuns(t, disk, "leaf")
	if len(leaves) != 2 || len(childRuns(t, disk, "middle")) != 1 {
		t.Fatalf("child runs: leaf=%d middle=%d", len(leaves), len(childRuns(t, disk, "middle")))
	}
	for _, listed := range leaves {
		leaf, err := disk.Run(context.Background(), listed.ID)
		if err != nil || leaf.Status != RunSucceeded || leaf.Origin != *ownerOrigin() || leaf.ParentRunID == "" {
			t.Fatalf("leaf run = %+v, %v", leaf, err)
		}
		input, err := disk.RunInputFor(context.Background(), leaf.ID)
		if err != nil || input.Objective != "nest" {
			t.Fatalf("leaf input = %+v, %v", input, err)
		}
	}
	steps, err := disk.Steps(context.Background(), summary.ID)
	if err != nil || len(steps) != 2 || !strings.Contains(string(steps[0].Output), `"status":"succeeded"`) {
		t.Fatalf("parent steps = %+v, %v", steps, err)
	}
}

func TestFailedChildFailsTheComposingStep(t *testing.T) {
	disk := newTestStore(t)
	interpreter := NewInterpreter(disk, durabletest.NewFake(), &Options{Driver: DriverConcurrent, MaxConcurrentSteps: 2, Tools: failingTools{}})
	installAll(t, interpreter, toolSpec("leaf"), composingSpec("top", "leaf"))
	summary, err := interpreter.Start(context.Background(), "top", nil, ownerOrigin())
	if err != nil {
		t.Fatal(err)
	}
	waitForRunStatus(t, disk, summary.ID, RunFailed, 5*time.Second)
	steps, err := disk.Steps(context.Background(), summary.ID)
	if err != nil || steps[0].ErrorCode != string(ErrorCodeChildFailed) {
		t.Fatalf("parent step = %+v, %v", steps, err)
	}
	if leaves := childRuns(t, disk, "leaf"); len(leaves) != 1 || leaves[0].Status != RunFailed {
		t.Fatalf("leaf runs = %+v", leaves)
	}
}

func TestSerialCompositionJournalsTheChildAndPropagatesCancel(t *testing.T) {
	tools := &fakeToolRunner{output: func(string) json.RawMessage { return json.RawMessage(`{"ok":true}`) }}
	interpreter, fake, disk := serialTestSetup(t, tools)
	event := "never"
	waiting := &Spec{ID: "waiting", Goal: "Wait", Version: 1, Source: SourceDefined, Steps: []StepSpec{{ID: "hold", Executor: ExecutorSpec{Kind: KindWait, Event: &event}, Timeout: time.Minute}}}
	installAll(t, interpreter, toolSpec("leaf"), waiting, composingSpec("top", "leaf"), composingSpec("stuck", "waiting"))
	topRun, err := disk.newRun(context.Background(), specOf(t, interpreter, "top"), &RunInput{}, ownerOrigin())
	if err != nil {
		t.Fatal(err)
	}
	inv, _ := holdInvocation(t, fake, "holder-compose")
	terminal, err := interpreter.DriveSerial(context.Background(), inv, topRun.ID)
	if err != nil || terminal != RunSucceeded {
		t.Fatalf("serial composed drive = %s, %v", terminal, err)
	}
	if leaves := childRuns(t, disk, "leaf"); len(leaves) != 1 || leaves[0].Status != RunSucceeded {
		t.Fatalf("leaf runs after serial drive = %+v", leaves)
	}
	stuckRun, err := disk.newRun(context.Background(), specOf(t, interpreter, "stuck"), &RunInput{}, ownerOrigin())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	second, _ := holdInvocation(t, fake, "holder-cancel")
	done := make(chan error, 1)
	go func() {
		_, driveErr := interpreter.DriveSerial(ctx, second, stuckRun.ID)
		done <- driveErr
	}()
	deadline := time.Now().Add(5 * time.Second)
	for len(childRuns(t, disk, "waiting")) == 0 || childRuns(t, disk, "waiting")[0].Status != RunSuspended {
		if time.Now().After(deadline) {
			t.Fatal("the child never parked on its wait step")
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("cancelling the parent did not stop the child")
	}
	waitForRunStatus(t, disk, stuckRun.ID, RunCancelled, 2*time.Second)
	waitForRunStatus(t, disk, childRuns(t, disk, "waiting")[0].ID, RunCancelled, 2*time.Second)
}

type failingTools struct{}

func (failingTools) Invoke(context.Context, string, json.RawMessage) (json.RawMessage, error) {
	return nil, errors.New("tool exploded")
}

func specOf(t *testing.T, interpreter *Interpreter, id string) *Spec {
	t.Helper()
	spec, ok := interpreter.Spec(id)
	if !ok {
		t.Fatalf("spec %s is not loaded", id)
	}
	return spec
}
