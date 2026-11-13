package workflow

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/platform/durable"
)

type recordingRegistrar struct {
	*durable.Fake
	mu         sync.Mutex
	registered []string
	keys       []string
}

func (r *recordingRegistrar) RegisterHandler(name string, fn durable.Handler) {
	r.mu.Lock()
	r.registered = append(r.registered, name)
	r.mu.Unlock()
	r.Fake.RegisterHandler(name, func(ctx context.Context, inv durable.Invocation) error {
		return fn(ctx, &keyCollectingInvocation{Invocation: inv, registrar: r})
	})
}

func (r *recordingRegistrar) actionKeys() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.keys)
}

func (r *recordingRegistrar) registeredNames() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.registered)
}

type keyCollectingInvocation struct {
	durable.Invocation
	registrar *recordingRegistrar
}

func (k *keyCollectingInvocation) RunAction(ctx context.Context, key string, fn func(ctx context.Context) ([]byte, error)) ([]byte, error) {
	k.registrar.mu.Lock()
	k.registrar.keys = append(k.registrar.keys, key)
	k.registrar.mu.Unlock()
	return k.Invocation.RunAction(ctx, key, fn)
}

func twoToolStepSpec(id string) *Spec {
	tool := "read_file"
	return &Spec{
		ID: id, Goal: "Two tool steps", Version: 1, Source: SourceDefined,
		Steps: []StepSpec{
			{ID: "first", Executor: ExecutorSpec{Kind: KindTool, ToolID: &tool}, Timeout: 5 * time.Second},
			{ID: "second", DependsOn: []string{"first"}, Executor: ExecutorSpec{Kind: KindTool, ToolID: &tool}, Timeout: 5 * time.Second},
		},
	}
}

func TestSerialDriverRegistersTheJournaledHandler(t *testing.T) {
	tests := []struct {
		name        string
		options     *Options
		wantJournal bool
	}{
		{name: "serial driver journals every step", options: &Options{Tools: &fakeToolRunner{}, Driver: DriverSerial}, wantJournal: true},
		{name: "zero driver journals every step", options: &Options{Tools: &fakeToolRunner{}}, wantJournal: true},
		{name: "concurrent driver journals nothing", options: &Options{Tools: &fakeToolRunner{}, Driver: DriverConcurrent}, wantJournal: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			disk := newTestStore(t)
			runtime := &recordingRegistrar{Fake: durable.NewFake()}
			interpreter := NewInterpreter(disk, runtime, tc.options)
			if err := interpreter.Install(ctx, []*Spec{twoToolStepSpec("driver-two-steps")}, testValidationDeps()); err != nil {
				t.Fatalf("Load: %v", err)
			}
			if got := runtime.registeredNames(); !slices.Equal(got, []string{HandlerName}) {
				t.Fatalf("registered handlers = %v, want [%s]", got, HandlerName)
			}
			summary, err := interpreter.Start(ctx, "driver-two-steps", nil, ownerOrigin())
			if err != nil {
				t.Fatalf("Start: %v", err)
			}
			waitForRunStatus(t, disk, summary.ID, RunSucceeded, 5*time.Second)
			keys := runtime.actionKeys()
			prefix := "run/" + summary.ID + "/step/"
			wantKeys := []string{prefix + "first/attempt/0", prefix + "second/attempt/0"}
			if !tc.wantJournal {
				wantKeys = nil
			}
			if !slices.Equal(keys, wantKeys) {
				t.Fatalf("journaled actions = %v, want %v", keys, wantKeys)
			}
		})
	}
}

type stepCallTools struct {
	mu      sync.Mutex
	calls   []StepCall
	failing map[string]bool
}

func (s *stepCallTools) Invoke(ctx context.Context, toolID string, args json.RawMessage) (json.RawMessage, error) {
	call, _ := StepCallFrom(ctx)
	s.mu.Lock()
	s.calls = append(s.calls, call)
	fail := s.failing[call.StepID]
	delete(s.failing, call.StepID)
	s.mu.Unlock()
	if fail {
		return nil, codedError(ErrorCodeStepFailed, "first attempt fails")
	}
	return json.RawMessage(`{"ok":true}`), nil
}

func (s *stepCallTools) recorded() []StepCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.calls)
}

func TestToolStepCarriesItsStepCall(t *testing.T) {
	ctx := t.Context()
	tools := &stepCallTools{failing: map[string]bool{"second": true}}
	registry, err := buildTestRegistry()
	if err != nil {
		t.Fatalf("registry: %v", err)
	}
	fake := durable.NewFake()
	disk := newTestStore(t)
	interpreter := NewInterpreter(disk, fake, &Options{AgentResolver: registry, Tools: tools})
	spec := twoToolStepSpec("step-call")
	spec.Steps[1].Retry = RetryPolicy{Attempts: 1, Backoff: time.Millisecond}
	runID := startSerialRun(t, interpreter, disk, spec, nil)
	inv, _ := holdInvocation(t, fake, "holder-step-call")
	terminal, err := interpreter.DriveSerial(ctx, inv, runID)
	if err != nil {
		t.Fatalf("DriveSerial: %v", err)
	}
	if terminal != RunSucceeded {
		t.Fatalf("terminal = %s, want %s", terminal, RunSucceeded)
	}
	digest, err := Digest(spec)
	if err != nil {
		t.Fatalf("Digest: %v", err)
	}
	want := []StepCall{
		{RunID: runID, StepID: "first", Attempt: 0, Origin: *ownerOrigin(), DefinitionDigest: digest},
		{RunID: runID, StepID: "second", Attempt: 0, Origin: *ownerOrigin(), DefinitionDigest: digest},
		{RunID: runID, StepID: "second", Attempt: 1, Origin: *ownerOrigin(), DefinitionDigest: digest},
	}
	if got := tools.recorded(); !slices.Equal(got, want) {
		t.Fatalf("step calls = %+v, want %+v", got, want)
	}
}

type payloadInvocation struct {
	durable.Invocation
	payload []byte
	actions int
}

func (p *payloadInvocation) Payload() []byte { return p.payload }

func (p *payloadInvocation) RunAction(ctx context.Context, key string, fn func(ctx context.Context) ([]byte, error)) ([]byte, error) {
	p.actions++
	return nil, errors.New("no action may run")
}

func TestSerialHandlerRefusesAPayloadWithoutARunID(t *testing.T) {
	tests := []struct {
		name    string
		payload []byte
	}{
		{name: "empty payload", payload: nil},
		{name: "undecodable payload", payload: []byte(`not json`)},
		{name: "payload without a run id", payload: []byte(`{"definition_id":"d"}`)},
		{name: "payload with an empty run id", payload: []byte(`{"run_id":""}`)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			disk := newTestStore(t)
			interpreter := NewInterpreter(disk, durable.NewFake(), &Options{Tools: &fakeToolRunner{}})
			inv := &payloadInvocation{payload: tc.payload}
			err := interpreter.HandleSerial(t.Context(), inv)
			if code, ok := CodeOf(err); !ok || code != ErrorCodeStepFailed {
				t.Fatalf("HandleSerial error = %v, want code %s", err, ErrorCodeStepFailed)
			}
			if inv.actions != 0 {
				t.Fatalf("HandleSerial ran %d actions, want none", inv.actions)
			}
		})
	}
	t.Run("no invocation", func(t *testing.T) {
		interpreter := NewInterpreter(newTestStore(t), durable.NewFake(), &Options{Tools: &fakeToolRunner{}})
		if code, ok := CodeOf(interpreter.HandleSerial(t.Context(), nil)); !ok || code != ErrorCodeStepFailed {
			t.Fatalf("HandleSerial without an invocation, want code %s", ErrorCodeStepFailed)
		}
	})
}
