//go:build durable

package restate

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"testing"
	"time"

	auraagent "github.com/anggasct/aura/internal/agent"
	"github.com/anggasct/aura/internal/durable"
	"github.com/anggasct/aura/internal/workflow"
)

type parkAgent struct{}

func (parkAgent) Run(_ context.Context, _ *auraagent.Definition, _ *workflow.ExecutionContext) (json.RawMessage, error) {
	return json.RawMessage(`{"external_id":"org/repo#42"}`), nil
}

func TestParkDurationSurvival(t *testing.T) {
	binary := os.Getenv("AURA_RESTATE_BINARY")
	if binary == "" {
		t.Skip("no binary")
	}
	ctx := context.Background()
	disk := workflow.NewStore(liveTestDB(t))
	registry, err := auraagent.Build(nil, []string{"read_file", "list_dir"}, []string{"primary"})
	if err != nil {
		t.Fatal(err)
	}
	interpreter := workflow.NewInterpreter(disk, durable.NewFake(), &workflow.Options{
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
	deps := workflow.ValidationDeps{KnownTools: []string{}, EffectfulTools: []string{}, Agents: registry}
	if err := interpreter.Load(ctx, spec, deps); err != nil {
		t.Fatal(err)
	}
	summary, err := disk.CreateRun(ctx, spec, nil)
	if err != nil {
		t.Fatal(err)
	}
	runID := summary.ID
	durableKey := fmt.Sprintf("park-%d", time.Now().UnixNano())

	dir := t.TempDir()
	ingressPort, adminPort := freePort(t), freePort(t)
	handlerPort := freePort(t)
	handlerAddr := fmt.Sprintf("127.0.0.1:%d", handlerPort)
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
	waitEndpointReady(t, endpoint)
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
