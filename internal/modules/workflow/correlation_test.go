package workflow

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

func TestBindCorrelationRoundTrip(t *testing.T) {
	disk := newTestStore(t)
	ctx := context.Background()
	spec := &Spec{
		ID: "corr", Goal: "Correlation", Version: 1, Source: SourceDefined,
		Steps: []StepSpec{
			{ID: "hold", Executor: ExecutorSpec{Kind: KindWait, Event: strPtr("check_suite.completed")}, Timeout: time.Minute},
		},
	}
	if err := disk.SaveDefinition(ctx, spec); err != nil {
		t.Fatalf("SaveDefinition: %v", err)
	}
	summary, err := disk.CreateRun(ctx, spec, nil)
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	binding := Correlation{
		Source: "github", EventType: "check_suite.completed", ExternalID: "org/repo#42",
		RunID: summary.ID, SignalName: "wait.hold", DedupeKey: "",
	}
	if err := disk.BindCorrelation(ctx, &binding); err != nil {
		t.Fatalf("BindCorrelation: %v", err)
	}
	if err := disk.BindCorrelation(ctx, &binding); err == nil {
		t.Fatal("expected duplicate binding to conflict, got nil")
	} else if code, ok := CodeOf(err); !ok || code != ErrorCodeCorrelationConflict {
		t.Fatalf("conflict code = %v (ok=%v), want %s", code, ok, ErrorCodeCorrelationConflict)
	}
	delivery := binding
	delivery.DedupeKey = "delivery-1"
	if err := disk.BindCorrelation(ctx, &delivery); err != nil {
		t.Fatalf("BindCorrelation delivery: %v", err)
	}
	if err := disk.BindCorrelation(ctx, &delivery); err == nil {
		t.Fatal("expected duplicate delivery to conflict, got nil")
	} else if code, ok := CodeOf(err); !ok || code != ErrorCodeCorrelationConflict {
		t.Fatalf("delivery conflict code = %v (ok=%v), want %s", code, ok, ErrorCodeCorrelationConflict)
	}
	resolved, err := disk.ResolveCorrelation(ctx, "github", "check_suite.completed", "org/repo#42", "")
	if err != nil {
		t.Fatalf("ResolveCorrelation: %v", err)
	}
	if resolved.RunID != summary.ID || resolved.SignalName != "wait.hold" {
		t.Errorf("resolved = %+v, want run-1/wait.hold", resolved)
	}
	if _, err := disk.ResolveCorrelation(ctx, "github", "check_suite.completed", "org/other#9", ""); err == nil {
		t.Fatal("expected unmatched event to fail, got nil")
	} else if code, ok := CodeOf(err); !ok || code != ErrorCodeCorrelationUnmatched {
		t.Fatalf("unmatched code = %v (ok=%v), want %s", code, ok, ErrorCodeCorrelationUnmatched)
	}
	listed, err := disk.ListCorrelationsByRun(ctx, summary.ID)
	if err != nil {
		t.Fatalf("ListCorrelationsByRun: %v", err)
	}
	if len(listed) != 2 {
		t.Fatalf("listed = %d rows, want binding plus delivery", len(listed))
	}
	empty, err := disk.ListCorrelationsByRun(ctx, "run-missing")
	if err != nil {
		t.Fatalf("ListCorrelationsByRun missing: %v", err)
	}
	if len(empty) != 0 {
		t.Errorf("listed missing run = %d rows, want none", len(empty))
	}
}

func TestBindCorrelationRejectsEmptyKeys(t *testing.T) {
	disk := newTestStore(t)
	ctx := context.Background()
	if err := disk.BindCorrelation(ctx, &Correlation{RunID: "run-1", SignalName: "wait.hold", DedupeKey: "d"}); err == nil {
		t.Fatal("expected empty source/event/external to fail, got nil")
	}
}

func TestDriveSerialWaitBindsCorrelation(t *testing.T) {
	ctx := context.Background()
	tools := &fakeToolRunner{output: func(toolID string) json.RawMessage {
		return json.RawMessage(`{"external_id":"org/repo#42"}`)
	}}
	interpreter, fake, disk := serialTestSetup(t, tools)
	tool := "read_file"
	event := "check_suite.completed"
	ref := "build"
	spec := &Spec{
		ID: "serial-bind", Goal: "Serial bind", Version: 1, Source: SourceDefined,
		Steps: []StepSpec{
			{ID: "build", Executor: ExecutorSpec{Kind: KindTool, ToolID: &tool}, Timeout: 5 * time.Second},
			{ID: "hold", DependsOn: []string{"build"}, Executor: ExecutorSpec{Kind: KindWait, Event: &event, ExternalRef: &ref}, Timeout: 5 * time.Second},
		},
	}
	runID := startSerialRun(t, interpreter, disk, spec, nil)
	inv, holder := holdInvocation(t, fake, "holder-bind")
	done := make(chan struct{})
	var driveErr error
	go func() {
		defer close(done)
		_, driveErr = interpreter.DriveSerial(ctx, inv, runID)
	}()
	waitForRunStatus(t, disk, runID, RunSuspended, 2*time.Second)
	rows, err := disk.ListCorrelationsByRun(ctx, runID)
	if err != nil {
		t.Fatalf("ListCorrelationsByRun: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("bindings = %d rows, want one", len(rows))
	}
	if rows[0].Source != "github" || rows[0].EventType != event || rows[0].ExternalID != "org/repo#42" || rows[0].SignalName != "wait.hold" {
		t.Errorf("binding = %+v, want the wait declaration", rows[0])
	}
	signalHolder(t, fake, holder, "wait.hold", `{"ci":"green"}`)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("serial drive never finished")
	}
	if driveErr != nil {
		t.Fatalf("DriveSerial: %v", driveErr)
	}
}
