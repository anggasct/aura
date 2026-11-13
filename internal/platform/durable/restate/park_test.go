//go:build durable

package restate

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/modules/workflow"
	auraagent "github.com/anggasct/aura/internal/platform/agent"
	"github.com/anggasct/aura/internal/platform/durable"
)

type parkAgent struct{}

func (parkAgent) Run(_ context.Context, _ *auraagent.Definition, _ *workflow.ExecutionContext) (json.RawMessage, error) {
	return json.RawMessage(`{"external_id":"org/repo#42"}`), nil
}

func TestParkDurationSurvival(t *testing.T) {
	binary := liveRestateBinary(t)
	ctx := context.Background()
	disk := workflow.NewStore(liveTestDB(t))
	registry, err := auraagent.Build(nil, []string{"read_file", "list_dir"}, []string{"primary"})
	if err != nil {
		t.Fatal(err)
	}
	interpreter := workflow.NewInterpreter(disk, queuedRuntime{durable.NewFake()}, &workflow.Options{
		MaxConcurrentSteps: 1, AgentResolver: registry, Agents: parkAgent{},
	})
	engineer := "engineer"
	ciEvent := "ci"
	implementRef := "implement"
	spec := &workflow.Spec{
		ID: "park", Goal: "park", Version: 1, Source: workflow.SourceDefined,
		Steps: []workflow.StepSpec{
			{ID: "implement", Executor: workflow.ExecutorSpec{Kind: workflow.KindAgent, AgentID: &engineer}, Timeout: 2 * time.Minute},
			{ID: "hold", DependsOn: []string{"implement"}, Executor: workflow.ExecutorSpec{Kind: workflow.KindWait, Event: &ciEvent, ExternalRef: &implementRef}, Timeout: 15 * time.Minute},
		},
	}
	deps := &workflow.ValidationDeps{KnownTools: []string{}, EffectfulTools: []string{}, Agents: registry}
	if err := interpreter.Install(ctx, []*workflow.Spec{spec}, deps); err != nil {
		t.Fatal(err)
	}
	summary, err := interpreter.Start(ctx, spec.ID, nil, ownerOrigin())
	if err != nil {
		t.Fatal(err)
	}
	runID := summary.ID
	durableKey := fmt.Sprintf("park-%d", time.Now().UnixNano())

	dir := t.TempDir()
	ingressPort, adminPort := freePort(t), freePort(t)
	handlerAddr := "127.0.0.1:0"
	server := startLiveServer(t, binary, dir, ingressPort, adminPort)
	t.Cleanup(func() { server.stop() })

	endpoint, err := NewEndpoint(EndpointConfig{HandlerAddr: handlerAddr}, nil)
	if err != nil {
		t.Fatal(err)
	}
	endpoint.RegisterHandler("workflow", func(ctx context.Context, inv durable.Invocation) error {
		_, err := interpreter.DriveSerial(ctx, inv, runID)
		return err
	})
	endpointCtx, stopEndpoint := context.WithCancel(context.Background())
	endpointDone := make(chan error, 1)
	go func() { endpointDone <- endpoint.Start(endpointCtx) }()
	t.Cleanup(func() {
		stopEndpoint()
		select {
		case <-endpointDone:
		case <-time.After(10 * time.Second):
		}
	})
	waitEndpointReadyStarted(t, endpoint, endpointDone)
	handlerAddr = resolveHandlerAddr(t, endpoint, handlerAddr)
	server.registerDeployment(t, handlerAddr)

	adapter, err := NewAdapter(Config{IngressURL: fmt.Sprintf("http://127.0.0.1:%d", ingressPort)}, nil)
	if err != nil {
		t.Fatal(err)
	}
	adapter.RegisterHandler("workflow", func(context.Context, durable.Invocation) error { return nil })
	ref, err := adapter.Start(ctx, durable.StartRequest{Handler: "workflow", Key: durableKey, Payload: []byte(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	waitForStoreStatus(t, disk, runID, workflow.RunSuspended, 90*time.Second)
	t.Logf("parked, waiting 30s before signaling")
	time.Sleep(30 * time.Second)
	if err := adapter.Signal(ctx, ref, "wait.hold", []byte(`{"ci":"green"}`)); err != nil {
		t.Fatal(err)
	}
	waitForStoreStatus(t, disk, runID, workflow.RunSucceeded, 90*time.Second)
}
