package workflow

import (
	"context"
	"database/sql"
	"encoding/json"
	"log/slog"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/platform/durable"
	"github.com/anggasct/aura/internal/platform/durable/sqlite"
	"github.com/anggasct/aura/internal/store/storetest"
)

const (
	hostPoll    = 10 * time.Millisecond
	hostTTL     = time.Second
	hostGrace   = 5 * time.Second
	hostTimeout = 20 * time.Second
	hostHandler = "workflow"
)

type driveRecord struct {
	status string
	err    error
}

type workflowHost struct {
	t           *testing.T
	db          *sql.DB
	disk        *Store
	interpreter *Interpreter
	runtime     *sqlite.Runtime
	stop        context.CancelFunc
	stopped     chan error

	warnings *warningLog
	mu       sync.Mutex
	drives   []driveRecord
}

type warningLog struct {
	mu       sync.Mutex
	messages []string
}

func (l *warningLog) InfoContext(context.Context, string, ...any) {}

func (l *warningLog) WarnContext(_ context.Context, msg string, _ ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.messages = append(l.messages, msg)
}

func (l *warningLog) logged() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.messages...)
}

type hostedRun struct {
	id  string
	ref durable.RunRef
}

func newWorkflowHost(t *testing.T, options *Options) *workflowHost {
	t.Helper()
	db := storetest.OpenMigrated(t)
	disk := NewStore(db)
	warnings := &warningLog{}
	options.Logger = warnings
	options.Driver = DriverConcurrent
	host := &workflowHost{t: t, db: db, disk: disk, interpreter: NewInterpreter(disk, durable.NewFake(), options), warnings: warnings}
	host.startHost()
	t.Cleanup(host.stopHost)
	return host
}

func (h *workflowHost) startHost() {
	h.t.Helper()
	rt, err := sqlite.New(&sqlite.Options{DB: h.db, Logger: slog.New(slog.DiscardHandler), PollInterval: hostPoll, HostTTL: hostTTL, StopGrace: hostGrace})
	if err != nil {
		h.t.Fatalf("sqlite.New: %v", err)
	}
	rt.RegisterHandler(hostHandler, h.handle)
	ctx, stop := context.WithCancel(context.WithoutCancel(h.t.Context()))
	stopped := make(chan error, 1)
	go func() { stopped <- rt.Run(ctx) }()
	h.runtime, h.stop, h.stopped = rt, stop, stopped
}

func (h *workflowHost) stopHost() {
	h.t.Helper()
	if h.stop == nil {
		return
	}
	h.stop()
	h.stop = nil
	select {
	case err := <-h.stopped:
		if err != nil {
			h.t.Errorf("stop the durable host: %v", err)
		}
	case <-time.After(hostTimeout):
		h.t.Error("the durable host did not stop")
	}
}

func (h *workflowHost) handle(ctx context.Context, inv durable.Invocation) error {
	var tick struct {
		RunID string `json:"run_id"`
	}
	if err := json.Unmarshal(inv.Payload(), &tick); err != nil {
		return err
	}
	_, err := h.interpreter.DriveSerial(ctx, inv, tick.RunID)
	record := driveRecord{status: "unreadable", err: err}
	if run, readErr := h.disk.Run(context.WithoutCancel(ctx), tick.RunID); readErr == nil {
		record.status = run.Status
	}
	h.mu.Lock()
	h.drives = append(h.drives, record)
	h.mu.Unlock()
	return err
}

func (h *workflowHost) driveRecords() []driveRecord {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]driveRecord(nil), h.drives...)
}

func (h *workflowHost) load(specs ...*Spec) {
	h.t.Helper()
	installAll(h.t, h.interpreter, specs...)
}

func (h *workflowHost) startRun(definitionID string) hostedRun {
	h.t.Helper()
	summary, err := h.disk.newRun(h.t.Context(), specOf(h.t, h.interpreter, definitionID), &RunInput{}, ownerOrigin())
	if err != nil {
		h.t.Fatalf("CreateRun: %v", err)
	}
	payload, err := json.Marshal(map[string]string{"run_id": summary.ID})
	if err != nil {
		h.t.Fatalf("encode the run payload: %v", err)
	}
	ref, err := h.runtime.Start(h.t.Context(), durable.StartRequest{Handler: hostHandler, Key: "workflow/" + summary.ID, Payload: payload})
	if err != nil {
		h.t.Fatalf("start the durable run: %v", err)
	}
	return hostedRun{id: summary.ID, ref: ref}
}

func (h *workflowHost) eventually(what string, ready func() bool) {
	h.t.Helper()
	deadline := time.NewTimer(hostTimeout)
	defer deadline.Stop()
	tick := time.NewTicker(2 * time.Millisecond)
	defer tick.Stop()
	for !ready() {
		select {
		case <-tick.C:
		case <-deadline.C:
			h.t.Fatalf("timed out waiting until %s", what)
		}
	}
}

func (h *workflowHost) runStatus(runID string) string {
	h.t.Helper()
	run, err := h.disk.Run(h.t.Context(), runID)
	if err != nil {
		h.t.Fatalf("read run %s: %v", runID, err)
	}
	return run.Status
}

func (h *workflowHost) awaitRunStatus(runID, status string) {
	h.t.Helper()
	h.eventually("run "+runID+" is "+status, func() bool { return h.runStatus(runID) == status })
}

func (h *workflowHost) steps(runID string) map[string]*StepRun {
	h.t.Helper()
	rows, err := h.disk.Steps(h.t.Context(), runID)
	if err != nil {
		h.t.Fatalf("read the steps of %s: %v", runID, err)
	}
	byID := map[string]*StepRun{}
	for _, row := range rows {
		byID[row.StepID] = row
	}
	return byID
}

func (h *workflowHost) durableState(ref durable.RunRef) durable.RunState {
	h.t.Helper()
	status, err := h.runtime.Status(h.t.Context(), ref)
	if err != nil {
		h.t.Fatalf("status of %s: %v", ref.Key, err)
	}
	return status.State
}

func (h *workflowHost) awaitDurableState(ref durable.RunRef, state durable.RunState) {
	h.t.Helper()
	h.eventually("durable run "+ref.Key+" is "+string(state), func() bool { return h.durableState(ref) == state })
}

func (h *workflowHost) signal(ref durable.RunRef, name, payload string) {
	h.t.Helper()
	if err := h.runtime.Signal(h.t.Context(), ref, name, []byte(payload)); err != nil {
		h.t.Fatalf("signal %s: %v", name, err)
	}
}

func (h *workflowHost) dropLease() {
	h.t.Helper()
	if _, err := h.db.ExecContext(h.t.Context(), `DELETE FROM durable_host`); err != nil {
		h.t.Fatalf("delete the host lease: %v", err)
	}
}

func (h *workflowHost) requirePreemptedDrives() {
	h.t.Helper()
	drives := h.driveRecords()
	if len(drives) == 0 {
		h.t.Fatal("no drive returned before the host stopped")
	}
	for index, drive := range drives {
		if drive.err == nil {
			h.t.Errorf("drive %d returned nil after its host stopped", index+1)
		}
	}
	h.requireNoStepFailureLogged()
}

func (h *workflowHost) requireNoStepFailureLogged() {
	h.t.Helper()
	if warnings := h.warnings.logged(); slices.Contains(warnings, "workflow step failed") {
		h.t.Errorf("warnings = %v, want no step failure", warnings)
	}
}

func awaitClosed(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(hostTimeout):
		t.Fatalf("timed out waiting until %s", what)
	}
}

type scriptedTools struct {
	mu      sync.Mutex
	calls   map[string]int
	respond func(ctx context.Context, call int) (json.RawMessage, error)
}

func (s *scriptedTools) Invoke(ctx context.Context, _ string, args json.RawMessage) (json.RawMessage, error) {
	var target struct {
		Step string `json:"step"`
	}
	if err := json.Unmarshal(args, &target); err != nil {
		return nil, err
	}
	s.mu.Lock()
	if s.calls == nil {
		s.calls = map[string]int{}
	}
	s.calls[target.Step]++
	call := s.calls[target.Step]
	s.mu.Unlock()
	if s.respond == nil {
		return json.RawMessage(`{"ok":true}`), nil
	}
	return s.respond(ctx, call)
}

func (s *scriptedTools) callsOf(stepID string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls[stepID]
}

func failsOnceWhenItsContextEnds(entered chan struct{}) func(ctx context.Context, call int) (json.RawMessage, error) {
	return func(ctx context.Context, call int) (json.RawMessage, error) {
		if call > 1 {
			return json.RawMessage(`{"ok":true}`), nil
		}
		close(entered)
		<-ctx.Done()
		return nil, ctx.Err()
	}
}

type scriptedApprovals struct {
	mu      sync.Mutex
	calls   int
	respond func(ctx context.Context, call int) error
}

func (s *scriptedApprovals) Request(ctx context.Context, _, _ string) error {
	s.mu.Lock()
	s.calls++
	call := s.calls
	s.mu.Unlock()
	return s.respond(ctx, call)
}

func definedSpec(id string, steps ...StepSpec) *Spec {
	return &Spec{ID: id, Goal: "Run " + id, Version: 1, Source: SourceDefined, Steps: steps}
}

func toolStep(id string, dependsOn ...string) StepSpec {
	return StepSpec{ID: id, DependsOn: dependsOn, Executor: ExecutorSpec{Kind: KindTool, ToolID: ptr("read_file"), ToolArgs: json.RawMessage(`{"step":"` + id + `"}`)}, Timeout: time.Minute}
}

func waitStep(id string, dependsOn ...string) StepSpec {
	return StepSpec{ID: id, DependsOn: dependsOn, Executor: ExecutorSpec{Kind: KindWait, Event: ptr("ci")}, Timeout: time.Minute}
}

func TestSerialDriveLeavesAWaitingRunSuspendedWhenItsHostStops(t *testing.T) {
	tools := &scriptedTools{}
	host := newWorkflowHost(t, &Options{Tools: tools})
	host.load(definedSpec("stop-while-waiting", toolStep("build"), waitStep("hold", "build"), toolStep("ship", "hold")))
	run := host.startRun("stop-while-waiting")
	host.awaitRunStatus(run.id, RunSuspended)
	host.stopHost()

	if status := host.runStatus(run.id); status != RunSuspended {
		t.Errorf("run status after the stop = %s, want %s", status, RunSuspended)
	}
	if step := host.steps(run.id)["hold"]; step != nil && step.Status == StepFailed {
		t.Errorf("wait step after the stop = %s, want it not failed", step.Status)
	}
	if state := host.durableState(run.ref); state != durable.RunRunning {
		t.Errorf("durable run after the stop = %s, want %s", state, durable.RunRunning)
	}
	host.requirePreemptedDrives()

	host.startHost()
	host.signal(run.ref, "wait.hold", `{"ci":"green"}`)
	host.awaitDurableState(run.ref, durable.RunSucceeded)
	if status := host.runStatus(run.id); status != RunSucceeded {
		t.Errorf("run status = %s, want %s", status, RunSucceeded)
	}
	steps := host.steps(run.id)
	for _, stepID := range []string{"build", "hold", "ship"} {
		if step := steps[stepID]; step == nil || step.Status != StepSucceeded {
			t.Errorf("step %s = %+v, want %s", stepID, step, StepSucceeded)
		}
	}
	for _, stepID := range []string{"build", "ship"} {
		if calls := tools.callsOf(stepID); calls != 1 {
			t.Errorf("tool calls of %s = %d, want 1", stepID, calls)
		}
	}
}

func TestSerialDriveDoesNotFailAnApprovalItsHostPreempted(t *testing.T) {
	entered := make(chan struct{})
	approvals := &scriptedApprovals{respond: func(ctx context.Context, call int) error {
		if call > 1 {
			return nil
		}
		close(entered)
		<-ctx.Done()
		return ctx.Err()
	}}
	host := newWorkflowHost(t, &Options{Tools: &scriptedTools{}, Approvals: approvals})
	host.load(definedSpec("stop-while-requesting", StepSpec{ID: "gate", Executor: ExecutorSpec{Kind: KindApproval}, Timeout: time.Minute}))
	run := host.startRun("stop-while-requesting")
	awaitClosed(t, entered, "the approval is requested")
	host.stopHost()

	if step := host.steps(run.id)["gate"]; step != nil && step.Status == StepFailed {
		t.Errorf("approval step after the stop = %s, want it not failed", step.Status)
	}
	if status := host.runStatus(run.id); status == RunCancelled || status == RunFailed {
		t.Errorf("run status after the stop = %s, want neither cancelled nor failed", status)
	}
	host.requirePreemptedDrives()

	host.startHost()
	host.awaitRunStatus(run.id, RunSuspended)
	host.signal(run.ref, "approval.gate", `{"decision":"approve"}`)
	host.awaitDurableState(run.ref, durable.RunSucceeded)
	if status := host.runStatus(run.id); status != RunSucceeded {
		t.Errorf("run status = %s, want %s", status, RunSucceeded)
	}
}

func TestSerialDriveDoesNotJournalAnAttemptItsHostPreempted(t *testing.T) {
	cases := []struct {
		name    string
		respond func(entered chan struct{}) func(ctx context.Context, call int) (json.RawMessage, error)
		calls   int
	}{
		{name: "an attempt the stop failed runs again", respond: failsOnceWhenItsContextEnds, calls: 2},
		{name: "an attempt that succeeded after the stop is kept", calls: 1, respond: func(entered chan struct{}) func(ctx context.Context, call int) (json.RawMessage, error) {
			return func(ctx context.Context, call int) (json.RawMessage, error) {
				if call == 1 {
					close(entered)
					<-ctx.Done()
				}
				return json.RawMessage(`{"ok":true}`), nil
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			entered := make(chan struct{})
			tools := &scriptedTools{respond: tc.respond(entered)}
			host := newWorkflowHost(t, &Options{Tools: tools})
			host.load(definedSpec("stop-while-executing", toolStep("read")))
			run := host.startRun("stop-while-executing")
			awaitClosed(t, entered, "the tool is called")
			host.stopHost()

			if step := host.steps(run.id)["read"]; step == nil || step.Status == StepFailed {
				t.Errorf("tool step after the stop = %+v, want a row that is not failed", step)
			}
			if status := host.runStatus(run.id); status == RunCancelled {
				t.Errorf("run status after the stop = %s", status)
			}
			if state := host.durableState(run.ref); state != durable.RunRunning {
				t.Errorf("durable run after the stop = %s, want %s", state, durable.RunRunning)
			}
			host.requirePreemptedDrives()

			host.startHost()
			host.awaitDurableState(run.ref, durable.RunSucceeded)
			if step := host.steps(run.id)["read"]; step == nil || step.Status != StepSucceeded {
				t.Errorf("tool step = %+v, want %s", step, StepSucceeded)
			}
			if status := host.runStatus(run.id); status != RunSucceeded {
				t.Errorf("run status = %s, want %s", status, RunSucceeded)
			}
			if calls := tools.callsOf("read"); calls != tc.calls {
				t.Errorf("tool calls = %d, want %d", calls, tc.calls)
			}
		})
	}
}

func TestSerialDriveKeepsARunWhoseHostLostItsLease(t *testing.T) {
	entered := make(chan struct{})
	tools := &scriptedTools{respond: failsOnceWhenItsContextEnds(entered)}
	host := newWorkflowHost(t, &Options{Tools: tools})
	host.load(definedSpec("lose-while-executing", toolStep("read")))
	run := host.startRun("lose-while-executing")
	awaitClosed(t, entered, "the tool is called")
	host.dropLease()
	host.awaitDurableState(run.ref, durable.RunSucceeded)

	if status := host.runStatus(run.id); status != RunSucceeded {
		t.Errorf("run status = %s, want %s", status, RunSucceeded)
	}
	if step := host.steps(run.id)["read"]; step == nil || step.Status != StepSucceeded {
		t.Errorf("tool step = %+v, want %s", step, StepSucceeded)
	}
	if calls := tools.callsOf("read"); calls != 2 {
		t.Errorf("tool calls = %d, want 2", calls)
	}
	drives := host.driveRecords()
	if len(drives) != 2 {
		t.Fatalf("drives = %+v, want two", drives)
	}
	if drives[0].err == nil {
		t.Error("the drive that lost its lease returned nil")
	}
	if status := drives[0].status; status == RunCancelled || status == RunFailed {
		t.Errorf("run status after the lease was lost = %s, want neither cancelled nor failed", status)
	}
	host.requireNoStepFailureLogged()
}

func TestSerialDriveLeavesAChildWorkflowAsItIsWhenItsHostStops(t *testing.T) {
	host := newWorkflowHost(t, &Options{Tools: &scriptedTools{}})
	host.load(definedSpec("waiting-child", waitStep("hold")), composingSpec("waiting-parent", "waiting-child"))
	run := host.startRun("waiting-parent")
	var childID string
	host.eventually("the child run waits", func() bool {
		children := childRuns(t, host.disk, "waiting-child")
		if len(children) == 1 && children[0].Status == RunSuspended {
			childID = children[0].ID
		}
		return childID != ""
	})
	host.stopHost()

	for label, runID := range map[string]string{"parent": run.id, "child": childID} {
		if status := host.runStatus(runID); status == RunCancelled {
			t.Errorf("%s run after the stop = %s", label, status)
		}
	}
	host.requirePreemptedDrives()

	host.startHost()
	host.signal(run.ref, "wait.hold", `{"ci":"green"}`)
	host.awaitDurableState(run.ref, durable.RunSucceeded)
	for label, runID := range map[string]string{"parent": run.id, "child": childID} {
		if status := host.runStatus(runID); status != RunSucceeded {
			t.Errorf("%s run = %s, want %s", label, status, RunSucceeded)
		}
	}
	if children := childRuns(t, host.disk, "waiting-child"); len(children) != 1 {
		t.Errorf("child runs = %d, want 1", len(children))
	}
}

func TestSerialDriveStillCancelsACancelledRun(t *testing.T) {
	host := newWorkflowHost(t, &Options{Tools: &scriptedTools{}})
	host.load(definedSpec("cancel-while-waiting", toolStep("build"), waitStep("hold", "build")))
	run := host.startRun("cancel-while-waiting")
	host.awaitRunStatus(run.id, RunSuspended)
	if err := host.runtime.Cancel(t.Context(), run.ref); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	host.awaitDurableState(run.ref, durable.RunCancelled)

	if status := host.runStatus(run.id); status != RunCancelled {
		t.Errorf("run status = %s, want %s", status, RunCancelled)
	}
	step := host.steps(run.id)["hold"]
	if step == nil || step.Status != StepFailed || step.ErrorCode != "" {
		t.Errorf("wait step = %+v, want failed without an error code", step)
	}
}

func TestSerialDriveStillTimesOutAToolStep(t *testing.T) {
	tools := &scriptedTools{respond: func(ctx context.Context, _ int) (json.RawMessage, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}}
	host := newWorkflowHost(t, &Options{Tools: tools})
	slow := toolStep("read")
	slow.Timeout = 50 * time.Millisecond
	host.load(definedSpec("tool-times-out", slow))
	run := host.startRun("tool-times-out")
	host.awaitDurableState(run.ref, durable.RunSucceeded)

	step := host.steps(run.id)["read"]
	if step == nil || step.Status != StepFailed || step.ErrorCode != string(ErrorCodeStepTimeout) {
		t.Errorf("tool step = %+v, want failed with %s", step, ErrorCodeStepTimeout)
	}
	if status := host.runStatus(run.id); status != RunFailed {
		t.Errorf("run status = %s, want %s", status, RunFailed)
	}
	if calls := tools.callsOf("read"); calls != 1 {
		t.Errorf("tool calls = %d, want 1", calls)
	}
}
