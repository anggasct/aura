package workflow

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/durable"
)

func serialTestSetup(t *testing.T, tools *fakeToolRunner) (*Interpreter, *durable.Fake, *Store) {
	t.Helper()
	registry, err := buildTestRegistry()
	if err != nil {
		t.Fatalf("registry: %v", err)
	}
	fake := durable.NewFake()
	disk := newTestStore(t)
	interpreter := NewInterpreter(disk, fake, &Options{
		MaxConcurrentSteps: 2,
		AgentResolver:      registry,
		Agents:             &fakeAgentRunner{output: json.RawMessage(`{"decision":"approve"}`)},
		Tools:              tools,
	})
	return interpreter, fake, disk
}

func holdInvocation(t *testing.T, fake *durable.Fake, key string) (durable.Invocation, durable.RunRef) {
	t.Helper()
	release := make(chan struct{})
	invCh := make(chan durable.Invocation, 1)
	fake.RegisterHandler("holder", func(ctx context.Context, inv durable.Invocation) error {
		invCh <- inv
		select {
		case <-release:
		case <-ctx.Done():
		}
		return nil
	})
	ref, err := fake.Start(context.Background(), durable.StartRequest{Handler: "holder", Key: key, Payload: []byte(`{}`)})
	if err != nil {
		t.Fatalf("Start holder: %v", err)
	}
	t.Cleanup(func() { close(release) })
	select {
	case inv := <-invCh:
		return inv, ref
	case <-time.After(2 * time.Second):
		t.Fatal("holder invocation never started")
		return nil, durable.RunRef{}
	}
}

func startSerialRun(t *testing.T, interpreter *Interpreter, disk *Store, spec *Spec, input *RunInput) string {
	t.Helper()
	ctx := context.Background()
	if err := interpreter.Load(ctx, spec, testValidationDeps()); err != nil {
		t.Fatalf("Load: %v", err)
	}
	summary, err := disk.CreateRun(ctx, spec, input)
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	return summary.ID
}

func signalHolder(t *testing.T, fake *durable.Fake, ref durable.RunRef, name, payload string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if err := fake.Signal(context.Background(), ref, name, []byte(payload)); err == nil {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("signal %s never reached the holder", name)
}

func TestDriveSerialRunsMixedWorkflow(t *testing.T) {
	ctx := context.Background()
	tools := &fakeToolRunner{output: func(toolID string) json.RawMessage {
		return json.RawMessage(`{"pr":123}`)
	}}
	interpreter, fake, disk := serialTestSetup(t, tools)
	tool := "read_file"
	spec := &Spec{
		ID: "serial-mixed", Goal: "Serial mixed", Version: 1, Source: SourceDefined,
		Steps: []StepSpec{
			{ID: "implement", Executor: ExecutorSpec{Kind: KindAgent, AgentID: ptr("engineer")}, Timeout: 5 * time.Second},
			{ID: "record", DependsOn: []string{"implement"}, Executor: ExecutorSpec{Kind: KindTool, ToolID: &tool}, Timeout: 5 * time.Second},
			{ID: "approve", DependsOn: []string{"record"}, Executor: ExecutorSpec{Kind: KindApproval}, Timeout: 5 * time.Second},
			{
				ID: "close", DependsOn: []string{"approve"},
				Condition: strPtr(`steps.approve.output.decision == "approve" && steps.record.output.pr == 123`),
				Executor:  ExecutorSpec{Kind: KindApproval}, Timeout: 5 * time.Second,
			},
			{
				ID: "drop", DependsOn: []string{"approve"},
				Condition: strPtr(`steps.approve.output.decision == "reject"`),
				Executor:  ExecutorSpec{Kind: KindApproval}, Timeout: 5 * time.Second,
			},
		},
	}
	runID := startSerialRun(t, interpreter, disk, spec, &RunInput{Objective: "ship it"})
	inv, holder := holdInvocation(t, fake, "holder-mixed")

	done := make(chan struct{})
	var terminal string
	var driveErr error
	go func() {
		defer close(done)
		terminal, driveErr = interpreter.DriveSerial(ctx, inv, runID)
	}()
	waitForRunStatus(t, disk, runID, RunSuspended, 2*time.Second)
	signalHolder(t, fake, holder, "approval.approve", `{"decision":"approve"}`)
	waitForRunStatus(t, disk, runID, RunSuspended, 2*time.Second)
	signalHolder(t, fake, holder, "approval.close", `{"decision":"approve"}`)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("serial drive never finished")
	}
	if driveErr != nil {
		t.Fatalf("DriveSerial: %v", driveErr)
	}
	if terminal != RunSucceeded {
		t.Fatalf("terminal = %s, want %s", terminal, RunSucceeded)
	}
	steps, err := disk.Steps(ctx, runID)
	if err != nil {
		t.Fatalf("Steps: %v", err)
	}
	statuses := map[string]string{}
	for _, step := range steps {
		statuses[step.StepID] = step.Status
	}
	for stepID, want := range map[string]string{
		"implement": StepSucceeded, "record": StepSucceeded,
		"approve": StepSucceeded, "close": StepSucceeded, "drop": StepSkipped,
	} {
		if statuses[stepID] != want {
			t.Errorf("step %s = %s, want %s", stepID, statuses[stepID], want)
		}
	}
}

func TestDriveSerialWaitTimeoutFailsRun(t *testing.T) {
	ctx := context.Background()
	tools := &fakeToolRunner{}
	interpreter, fake, disk := serialTestSetup(t, tools)
	event := "ci"
	spec := &Spec{
		ID: "serial-timeout", Goal: "Serial timeout", Version: 1, Source: SourceDefined,
		Steps: []StepSpec{
			{ID: "hold", Executor: ExecutorSpec{Kind: KindWait, Event: &event}, Timeout: 50 * time.Millisecond},
		},
	}
	runID := startSerialRun(t, interpreter, disk, spec, nil)
	inv, _ := holdInvocation(t, fake, "holder-timeout")
	terminal, err := interpreter.DriveSerial(ctx, inv, runID)
	if err != nil {
		t.Fatalf("DriveSerial: %v", err)
	}
	if terminal != RunFailed {
		t.Fatalf("terminal = %s, want %s", terminal, RunFailed)
	}
	steps, err := disk.Steps(ctx, runID)
	if err != nil {
		t.Fatalf("Steps: %v", err)
	}
	if len(steps) != 1 || steps[0].Status != StepFailed || steps[0].ErrorCode != string(ErrorCodeStepTimeout) {
		t.Fatalf("steps = %+v, want one timeout failure", steps)
	}
}

func TestDriveSerialResumeConverges(t *testing.T) {
	ctx := context.Background()
	tools := &fakeToolRunner{}
	interpreter, fake, disk := serialTestSetup(t, tools)
	tool := "read_file"
	event := "ci"
	spec := &Spec{
		ID: "serial-resume", Goal: "Serial resume", Version: 1, Source: SourceDefined,
		Steps: []StepSpec{
			{ID: "build", Executor: ExecutorSpec{Kind: KindTool, ToolID: &tool}, Timeout: 5 * time.Second},
			{ID: "hold", DependsOn: []string{"build"}, Executor: ExecutorSpec{Kind: KindWait, Event: &event}, Timeout: 5 * time.Second},
			{ID: "ship", DependsOn: []string{"hold"}, Executor: ExecutorSpec{Kind: KindTool, ToolID: &tool}, Timeout: 5 * time.Second},
		},
	}
	runID := startSerialRun(t, interpreter, disk, spec, nil)

	firstInv, _ := holdInvocation(t, fake, "holder-resume-1")
	firstCtx, cancel := context.WithCancel(ctx)
	firstDone := make(chan error, 1)
	go func() {
		_, err := interpreter.DriveSerial(firstCtx, firstInv, runID)
		firstDone <- err
	}()
	waitForRunStatus(t, disk, runID, RunSuspended, 2*time.Second)
	if got := len(tools.snapshotCalled()); got != 1 {
		t.Fatalf("tool calls before cancel = %d, want 1", got)
	}
	cancel()
	select {
	case err := <-firstDone:
		if err == nil {
			t.Fatal("cancelled drive returned nil error")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cancelled drive never returned")
	}
	waitForRunStatus(t, disk, runID, RunCancelled, 2*time.Second)

	secondInv, secondHolder := holdInvocation(t, fake, "holder-resume-2")
	secondDone := make(chan struct{})
	var terminal string
	var driveErr error
	go func() {
		defer close(secondDone)
		terminal, driveErr = interpreter.DriveSerial(ctx, secondInv, runID)
	}()
	waitForRunStatus(t, disk, runID, RunSuspended, 2*time.Second)
	signalHolder(t, fake, secondHolder, "wait.hold", `{"ci":"green"}`)
	select {
	case <-secondDone:
	case <-time.After(5 * time.Second):
		t.Fatal("resumed drive never finished")
	}
	if driveErr != nil {
		t.Fatalf("DriveSerial: %v", driveErr)
	}
	if terminal != RunSucceeded {
		t.Fatalf("terminal = %s, want %s", terminal, RunSucceeded)
	}
	steps, err := disk.Steps(ctx, runID)
	if err != nil {
		t.Fatalf("Steps: %v", err)
	}
	for _, step := range steps {
		if step.Status != StepSucceeded {
			t.Errorf("step %s = %s, want %s", step.StepID, step.Status, StepSucceeded)
		}
	}
}

type flakyApprovalRequester struct {
	mu    sync.Mutex
	calls int
}

func (r *flakyApprovalRequester) Request(_ context.Context, _, _ string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++
	if r.calls == 1 {
		return errors.New("transient approval backend failure")
	}
	return nil
}

func (r *flakyApprovalRequester) requestCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls
}

type keyRecordingInvocation struct {
	durable.Invocation
	mu   sync.Mutex
	keys []string
}

func (k *keyRecordingInvocation) RunAction(ctx context.Context, key string, fn func(ctx context.Context) ([]byte, error)) ([]byte, error) {
	k.mu.Lock()
	k.keys = append(k.keys, key)
	k.mu.Unlock()
	return k.Invocation.RunAction(ctx, key, fn)
}

func (k *keyRecordingInvocation) approvalRequestKeys() []string {
	k.mu.Lock()
	defer k.mu.Unlock()
	var matched []string
	for _, key := range k.keys {
		if strings.Contains(key, "approval-request") {
			matched = append(matched, key)
		}
	}
	return matched
}

func TestDriveSerialApprovalRequestRetryUsesDistinctKeys(t *testing.T) {
	ctx := context.Background()
	registry, err := buildTestRegistry()
	if err != nil {
		t.Fatalf("registry: %v", err)
	}
	fake := durable.NewFake()
	disk := newTestStore(t)
	requester := &flakyApprovalRequester{}
	interpreter := NewInterpreter(disk, fake, &Options{
		MaxConcurrentSteps: 2,
		AgentResolver:      registry,
		Agents:             &fakeAgentRunner{output: json.RawMessage(`{"decision":"approve"}`)},
		Tools:              &fakeToolRunner{},
		Approvals:          requester,
	})
	spec := &Spec{
		ID: "serial-approval-retry", Goal: "Serial approval retry", Version: 1, Source: SourceDefined,
		Steps: []StepSpec{
			{ID: "gate", Executor: ExecutorSpec{Kind: KindApproval}, Timeout: 5 * time.Second, Retry: RetryPolicy{Attempts: 1, Backoff: time.Millisecond}},
		},
	}
	runID := startSerialRun(t, interpreter, disk, spec, nil)
	inv, holder := holdInvocation(t, fake, "holder-approval-retry")
	recording := &keyRecordingInvocation{Invocation: inv}

	done := make(chan struct{})
	var terminal string
	var driveErr error
	go func() {
		defer close(done)
		terminal, driveErr = interpreter.DriveSerial(ctx, recording, runID)
	}()
	waitForRunStatus(t, disk, runID, RunSuspended, 2*time.Second)
	signalHolder(t, fake, holder, "approval.gate", `{"decision":"approve"}`)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("serial drive never finished")
	}
	if driveErr != nil {
		t.Fatalf("DriveSerial: %v", driveErr)
	}
	if terminal != RunSucceeded {
		t.Fatalf("terminal = %s, want %s", terminal, RunSucceeded)
	}
	if got := requester.requestCount(); got != 2 {
		t.Fatalf("approval requests = %d, want 2", got)
	}
	keys := recording.approvalRequestKeys()
	if len(keys) != 2 || keys[0] == keys[1] {
		t.Fatalf("approval-request keys = %v, want two distinct keys", keys)
	}
}
