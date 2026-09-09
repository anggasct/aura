//go:build durable

package restate

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"
	"time"

	auraagent "github.com/anggasct/aura/internal/agent"
	"github.com/anggasct/aura/internal/approval"
	"github.com/anggasct/aura/internal/durable"
	"github.com/anggasct/aura/internal/effect"
	gatewaywebhook "github.com/anggasct/aura/internal/gateway/webhook"
	githubadapter "github.com/anggasct/aura/internal/integration/github"
	"github.com/anggasct/aura/internal/store"
	"github.com/anggasct/aura/internal/toolbroker"
	"github.com/anggasct/aura/internal/workflow"
)

type proofGitHubAPI struct {
	t       *testing.T
	server  *httptest.Server
	creates atomic.Int64
	merges  atomic.Int64
	notes   atomic.Int64
}

func startProofGitHubAPI(t *testing.T) *proofGitHubAPI {
	t.Helper()
	api := &proofGitHubAPI{t: t}
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/org/repo/pulls", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		if r.Header.Get("Authorization") != "Bearer proof-token" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"message":"bad credentials"}`))
			return
		}
		api.creates.Add(1)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"number":42,"html_url":"https://github.com/org/repo/pull/42"}`))
	})
	mux.HandleFunc("/repos/org/repo/pulls/42/merge", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer proof-token" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"message":"bad credentials"}`))
			return
		}
		api.merges.Add(1)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"merged":true,"sha":"deadbeef"}`))
	})
	mux.HandleFunc("/repos/org/repo/issues/42/comments", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer proof-token" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"message":"bad credentials"}`))
			return
		}
		api.notes.Add(1)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":99,"html_url":"https://github.com/org/repo/pull/42#issuecomment-99"}`))
	})
	api.server = httptest.NewTLSServer(mux)
	t.Cleanup(api.server.Close)
	return api
}

type proofAgentRunner struct{}

func (proofAgentRunner) Run(_ context.Context, _ *auraagent.Definition, _ *workflow.ExecutionContext) (json.RawMessage, error) {
	return json.RawMessage(`{"ok":true}`), nil
}

type brokerToolRunner struct {
	broker *toolbroker.Broker
	seq    atomic.Int64
}

func (r *brokerToolRunner) Invoke(ctx context.Context, toolID string, args json.RawMessage) (json.RawMessage, error) {
	n := r.seq.Add(1)
	id := fmt.Sprintf("proof-%d", n)
	request := &toolbroker.ToolRequest{
		RequestID: id, TurnID: "turn-proof", SessionID: "session-proof", PrincipalID: "owner-1",
		ToolName: toolID, ToolVersion: "v1", Arguments: args,
		Capabilities:    []string{"repository.write"},
		Trust:           approval.TrustTrustedConfiguration,
		IdempotencyKey:  "proof/" + id,
		EventSequence:   uint64(n),
		EventInvocation: id, EventBranch: "main", EventAuthor: "proof",
	}
	grant, err := r.broker.Grant(ctx, request, time.Minute)
	if err != nil {
		return nil, err
	}
	request.Approval = &grant
	result, err := r.broker.Execute(ctx, request)
	if err != nil {
		return nil, err
	}
	if result.Class != toolbroker.ResultOK {
		return nil, fmt.Errorf("tool %s finished as %s", toolID, result.Class)
	}
	return result.Output, nil
}

func proofEffectExecutor(t *testing.T, db *sql.DB) *effect.Executor {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC()
	if err := store.NewSessionService(db).Create(ctx, &store.Session{
		ID: "session-proof", OwnerID: "owner-1", CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("create session: %v", err)
	}
	journal, err := effect.NewJournal(db, effect.Options{})
	if err != nil {
		t.Fatalf("new journal: %v", err)
	}
	executor, err := effect.NewExecutor(journal)
	if err != nil {
		t.Fatalf("new executor: %v", err)
	}
	return executor
}

func proofSpec(credentialRef string) *workflow.Spec {
	tool := func(id string, toolID string, args string, depends ...string) workflow.StepSpec {
		return workflow.StepSpec{
			ID: id, DependsOn: depends,
			Executor: workflow.ExecutorSpec{Kind: workflow.KindTool, ToolID: &toolID, ToolArgs: json.RawMessage(args)},
			Timeout:  2 * time.Minute,
		}
	}
	agent := func(id string, agentID string, depends ...string) workflow.StepSpec {
		return workflow.StepSpec{
			ID: id, DependsOn: depends,
			Executor: workflow.ExecutorSpec{Kind: workflow.KindAgent, AgentID: &agentID},
			Timeout:  2 * time.Minute,
		}
	}
	engineer := "engineer"
	ciEvent := "check_suite.completed"
	openPR := "open_pr"
	return &workflow.Spec{
		ID: "software-development", Goal: "Ship the change", Version: 1, Source: workflow.SourceDefined,
		Steps: []workflow.StepSpec{
			{ID: "gate", Executor: workflow.ExecutorSpec{Kind: workflow.KindApproval}, Timeout: 10 * time.Minute},
			agent("implement", engineer, "gate"),
			agent("test", engineer, "implement"),
			tool("open_pr", githubadapter.ToolCreatePR,
				`{"repo":"org/repo","title":"Add thing","head":"feature","base":"main","credential_ref":"`+credentialRef+`"}`, "test"),
			{ID: "hold_ci", DependsOn: []string{"open_pr"},
				Executor: workflow.ExecutorSpec{Kind: workflow.KindWait, Event: &ciEvent, ExternalRef: &openPR},
				Timeout:  10 * time.Minute},
			tool("review", githubadapter.ToolComment,
				`{"repo":"org/repo","number":42,"body":"looks good","credential_ref":"`+credentialRef+`"}`, "hold_ci"),
			{ID: "approve_merge", DependsOn: []string{"review"},
				Executor: workflow.ExecutorSpec{Kind: workflow.KindApproval}, Timeout: 10 * time.Minute},
			tool("merge_pr", githubadapter.ToolMerge,
				`{"repo":"org/repo","number":42,"credential_ref":"`+credentialRef+`"}`, "approve_merge"),
		},
	}
}

func waitForHoldCISuspend(t *testing.T, disk *workflow.Store, runID string, api *proofGitHubAPI, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if api.creates.Load() != 1 {
			time.Sleep(200 * time.Millisecond)
			continue
		}
		run, err := disk.Run(context.Background(), runID)
		if err == nil && run.Status == workflow.RunSuspended {
			steps, err := disk.Steps(context.Background(), runID)
			if err == nil {
				for _, step := range steps {
					if step.StepID == "open_pr" && step.Status == workflow.StepSucceeded {
						return
					}
				}
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("run %s never reached the CI wait with its PR created", runID)
}

func TestProofWorkflowSurvivesCrashWithGitHubTools(t *testing.T) {
	binary := os.Getenv("AURA_RESTATE_BINARY")
	if binary == "" {
		t.Skip("AURA_RESTATE_BINARY is not set")
	}
	if _, err := os.Stat(binary); err != nil {
		t.Skipf("restate-server binary is not available: %v", err)
	}
	t.Setenv("AURA_PROOF_GITHUB_TOKEN", "proof-token")
	api := startProofGitHubAPI(t)

	ctx := context.Background()
	db := liveTestDB(t)
	disk := workflow.NewStore(db)
	executor := proofEffectExecutor(t, db)
	broker, err := toolbroker.New(&toolbroker.Options{Effects: executor})
	if err != nil {
		t.Fatalf("new broker: %v", err)
	}
	if err := githubadapter.Register(broker, githubadapter.ToolOptions{BaseURL: api.server.URL, HTTPClient: api.server.Client()}); err != nil {
		t.Fatalf("register github tools: %v", err)
	}
	runner := &brokerToolRunner{broker: broker}
	registry, err := auraagent.Build(nil, []string{"read_file", "list_dir"}, []string{"primary"})
	if err != nil {
		t.Fatalf("agent registry: %v", err)
	}
	newInterpreter := func() *workflow.Interpreter {
		interpreter := workflow.NewInterpreter(disk, durable.NewFake(), &workflow.Options{
			MaxConcurrentSteps: 1,
			AgentResolver:      registry,
			Agents:             proofAgentRunner{},
			Tools:              runner,
		})
		spec := proofSpec("env://AURA_PROOF_GITHUB_TOKEN")
		deps := workflow.ValidationDeps{
			KnownTools:     githubadapter.ToolNames(),
			EffectfulTools: githubadapter.ToolNames(),
			Agents:         registry,
		}
		if err := interpreter.Load(ctx, spec, deps); err != nil {
			t.Fatalf("Load: %v", err)
		}
		return interpreter
	}
	interpreter := newInterpreter()

	summary, err := disk.CreateRun(ctx, proofSpec("env://AURA_PROOF_GITHUB_TOKEN"), &workflow.RunInput{Objective: "ship it"})
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	runID := summary.ID
	durableKey := summary.DurableKey

	dir := t.TempDir()
	ingressPort, adminPort := freePort(t), freePort(t)
	handlerPort := freePort(t)
	handlerAddr := fmt.Sprintf("127.0.0.1:%d", handlerPort)
	ingressURL := fmt.Sprintf("http://127.0.0.1:%d", ingressPort)

	server := startLiveServer(t, binary, dir, ingressPort, adminPort)
	t.Cleanup(func() { server.stop() })

	startEndpoint := func(interpreter *workflow.Interpreter) (context.CancelFunc, chan error) {
		endpoint, err := NewEndpoint(EndpointConfig{HandlerAddr: handlerAddr}, nil)
		if err != nil {
			t.Fatalf("NewEndpoint: %v", err)
		}
		endpoint.RegisterHandler("workflow", func(ctx context.Context, inv durable.Invocation) error {
			_, err := interpreter.DriveSerial(ctx, inv, runID)
			return err
		})
		endpointCtx, stopEndpoint := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- endpoint.Start(endpointCtx) }()
		waitEndpointReady(t, endpoint)
		server.registerDeployment(t, handlerAddr)
		return stopEndpoint, done
	}

	stopEndpoint, endpointDone := startEndpoint(interpreter)
	adapter, err := NewAdapter(Config{IngressURL: ingressURL}, nil)
	if err != nil {
		t.Fatalf("NewAdapter: %v", err)
	}
	adapter.RegisterHandler("workflow", func(context.Context, durable.Invocation) error { return nil })
	_, err = adapter.Start(ctx, durable.StartRequest{Handler: "workflow", Key: durableKey, Payload: []byte(`{"objective":"ship it"}`)})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	// The gate approval suspends first; resolve it so the run reaches the CI wait.
	waitForStoreStatus(t, disk, runID, workflow.RunSuspended, 120*time.Second)
	t.Logf("phase=reached-gate-suspend")
	if err := adapter.Signal(ctx, durable.RunRef{Key: durableKey}, "approval.gate", []byte(`{"decision":"approve"}`)); err != nil {
		t.Fatalf("approve gate: %v", err)
	}
	waitForHoldCISuspend(t, disk, runID, api, 120*time.Second)
	t.Logf("phase=reached-hold-ci-suspend")
	rows, err := disk.ListCorrelationsByRun(ctx, runID)
	if err != nil {
		t.Fatalf("ListCorrelationsByRun: %v", err)
	}
	if len(rows) != 1 || rows[0].ExternalID != "org/repo#42" {
		t.Fatalf("bindings = %+v, want the create_pr external identity", rows)
	}

	// Crash both Aura and the durable runtime mid-wait, then restart both.
	server.stop()
	stopEndpoint()
	select {
	case <-endpointDone:
	case <-time.After(15 * time.Second):
		t.Fatal("endpoint did not stop")
	}

	server = startLiveServer(t, binary, dir, ingressPort, adminPort)
	interpreter = newInterpreter()
	stopEndpoint, endpointDone = startEndpoint(interpreter)
	t.Logf("phase=restarted")
	t.Cleanup(func() {
		stopEndpoint()
		select {
		case <-endpointDone:
		case <-time.After(15 * time.Second):
		}
	})

	// Deliver the CI completion through the real GitHub consume path.
	githubAdapter, err := githubadapter.NewAdapter(disk, adapter, nil)
	if err != nil {
		t.Fatalf("github adapter: %v", err)
	}
	ciBody := []byte(`{"action":"completed","repository":{"full_name":"org/repo"},` +
		`"check_suite":{"status":"completed","conclusion":"success","head_sha":"abc123",` +
		`"html_url":"https://github.com/org/repo/suites/1","pull_requests":[{"number":42}]}}`)
	ciEvent := &gatewaywebhook.AcceptedEvent{
		KeyID: "github", Nonce: "nonce-abcdefghijklmnop", BodyDigest: "digest-ci-proof-1", Body: ciBody,
	}
	handled, ref, err := githubAdapter.Handle(ctx, ciEvent)
	if err != nil || !handled || ref.ExecutionID != runID {
		t.Fatalf("Handle CI event = %v, %+v, %v; want run %s", handled, ref, err, runID)
	}
	// Duplicate delivery must not advance the run again.
	if _, _, err := githubAdapter.Handle(ctx, ciEvent); err != nil {
		t.Fatalf("duplicate Handle: %v", err)
	}

	// Resolve the merge approval; the run then merges and completes.
	if err := adapter.Signal(ctx, durable.RunRef{Key: durableKey}, "approval.approve_merge", []byte(`{"decision":"approve"}`)); err != nil {
		t.Fatalf("approve merge: %v", err)
	}
	t.Logf("phase=approved-merge")
	waitForStoreStatus(t, disk, runID, workflow.RunSucceeded, 180*time.Second)

	if got := api.creates.Load(); got != 1 {
		t.Errorf("create_pr calls = %d, want exactly one across the crash", got)
	}
	if got := api.notes.Load(); got != 1 {
		t.Errorf("comment calls = %d, want exactly one", got)
	}
	if got := api.merges.Load(); got != 1 {
		t.Errorf("merge calls = %d, want exactly one", got)
	}
	steps, err := disk.Steps(ctx, runID)
	if err != nil {
		t.Fatalf("Steps: %v", err)
	}
	for _, step := range steps {
		if step.Status != workflow.StepSucceeded {
			t.Errorf("step %s = %s, want %s", step.StepID, step.Status, workflow.StepSucceeded)
		}
	}
	rows, err = disk.ListCorrelationsByRun(ctx, runID)
	if err != nil {
		t.Fatalf("ListCorrelationsByRun: %v", err)
	}
	var bindings, deliveries int
	for _, row := range rows {
		if row.SignalName != "wait.hold_ci" || row.ExternalID != "org/repo#42" {
			t.Errorf("correlation row = %+v, want the CI binding family", row)
		}
		if row.DedupeKey == "" {
			bindings++
		} else {
			deliveries++
		}
	}
	if bindings != 1 || deliveries != 1 {
		t.Errorf("bindings = %d, deliveries = %d; want one of each", bindings, deliveries)
	}
	intents, err := executor.Journal().ListByState(ctx, effect.StateSucceeded, 0)
	if err != nil {
		t.Fatalf("list intents: %v", err)
	}
	seen := map[string]int{}
	for _, intent := range intents {
		if intent.Classification != effect.ClassificationEffectful {
			continue
		}
		seen[intent.Operation]++
	}
	for _, operation := range []string{githubadapter.ToolCreatePR, githubadapter.ToolComment, githubadapter.ToolMerge} {
		if seen[operation] != 1 {
			t.Errorf("effect intents for %s = %d, want one succeeded effectful intent", operation, seen[operation])
		}
	}
}
