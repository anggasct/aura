//go:build durable

package app

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/modules/builtin"
	"github.com/anggasct/aura/internal/modules/child"
	githubadapter "github.com/anggasct/aura/internal/modules/github"
	"github.com/anggasct/aura/internal/modules/workflow"
	auraagent "github.com/anggasct/aura/internal/platform/agent"
	"github.com/anggasct/aura/internal/platform/approval"
	"github.com/anggasct/aura/internal/platform/capability/capabilitytest"
	"github.com/anggasct/aura/internal/platform/durable"
	"github.com/anggasct/aura/internal/platform/principal"
	"github.com/anggasct/aura/internal/platform/toolbroker"
	"github.com/anggasct/aura/internal/platform/toolbroker/toolbrokertest"
	"github.com/anggasct/aura/internal/store"
)

const (
	readFileTool   = "read_file"
	listDirTool    = "list_dir"
	holdSignal     = "wait.hold"
	generatedID    = "gen_crash_resume"
	proofTokenEnv  = "AURA_PROOF_GITHUB_TOKEN"
	proofToken     = "proof-token"
	proofPullID    = "org/repo#42"
	childID        = "child-1"
	childParent    = "session-parent"
	childSession   = "session-child"
	childTask      = "CHILD-TASK"
	gateSignal     = "approval.gate"
	mergeSignal    = "approval.approve_merge"
	approveOutcome = `{"decision":"approve"}`
)

type runScenario struct {
	seed      func(t *testing.T, suite *runSuite)
	host      func(t *testing.T, life *runLife)
	firstLife func(t *testing.T, life *runLife)
}

var runScenarios = map[string]runScenario{
	"defined":   definedWorkflow.scenario(startTheWorkflow),
	"generated": generatedWorkflow.scenario(startTheWorkflow),
	"github":    proofWorkflow.scenario(approveTheGate),
	"child":     {seed: seedChild, host: hostChild, firstLife: startTheChild},
}

type workflowShape struct {
	spec            func() *workflow.Spec
	known           []string
	effectful       []string
	budget          workflow.Budget
	resumeFromStore bool
	tools           func(t *testing.T, life *runLife) workflow.ToolRunner
}

var definedWorkflow = &workflowShape{
	spec:  func() *workflow.Spec { return crashResumeSpec("crash-resume", workflow.SourceDefined) },
	known: []string{readFileTool, listDirTool},
	tools: countedTools,
}

var generatedWorkflow = &workflowShape{
	spec:            func() *workflow.Spec { return crashResumeSpec(generatedID, workflow.SourceGenerated) },
	known:           []string{readFileTool, listDirTool},
	budget:          workflow.Budget{MaxSteps: 3, MaxTokens: 1},
	resumeFromStore: true,
	tools:           countedTools,
}

var proofWorkflow = &workflowShape{
	spec:      func() *workflow.Spec { return proofSpec("env://" + proofTokenEnv) },
	known:     githubadapter.ToolNames(),
	effectful: githubadapter.ToolNames(),
	tools:     proofTools,
}

func (w *workflowShape) scenario(firstLife func(t *testing.T, life *runLife)) runScenario {
	return runScenario{seed: w.seed, host: w.host, firstLife: firstLife}
}

func (w *workflowShape) deps(t *testing.T) workflow.ValidationDeps {
	t.Helper()
	registry, err := auraagent.Build(nil, []string{readFileTool, listDirTool}, []string{"primary"})
	if err != nil {
		t.Fatalf("agent registry: %v", err)
	}
	return workflow.ValidationDeps{KnownTools: w.known, EffectfulTools: append([]string{}, w.effectful...), Agents: registry}
}

func (w *workflowShape) seed(t *testing.T, suite *runSuite) {
	t.Helper()
	disk := workflow.NewStore(suite.db)
	spec := w.spec()
	if err := disk.SaveDefinition(t.Context(), spec); err != nil {
		t.Fatalf("SaveDefinition: %v", err)
	}
	summary, err := disk.CreateRun(t.Context(), spec, &workflow.RunInput{Objective: "survive the kill"})
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	suite.plan.Run, suite.plan.Key = summary.ID, summary.DurableKey
}

func (w *workflowShape) host(t *testing.T, life *runLife) {
	t.Helper()
	deps := w.deps(t)
	interpreter := workflow.NewInterpreter(life.workflows, durable.NewFake(), &workflow.Options{
		MaxConcurrentSteps: 1,
		AgentResolver:      deps.Agents,
		Agents:             okAgentRunner{},
		Tools:              w.tools(t, life),
		GeneratedBudget:    w.budget,
	})
	spec := w.spec()
	specs := []*workflow.Spec{spec}
	if w.resumeFromStore && life.plan.Life > 1 {
		specs = nil
	}
	if err := interpreter.LoadAll(t.Context(), specs, deps); err != nil {
		t.Fatalf("LoadAll: %v", err)
	}
	if _, loaded := interpreter.Spec(spec.ID); loaded != (specs != nil) {
		t.Fatalf("the interpreter of life %d holds the definition %s in memory = %v, want %v before the run is driven", life.plan.Life, spec.ID, loaded, specs != nil)
	}
	registerWorkflowHandler(life.runtime, interpreter)
}

func crashResumeSpec(id string, source workflow.Source) *workflow.Spec {
	readFile, listDir, ciEvent := readFileTool, listDirTool, "ci"
	return &workflow.Spec{
		ID: id, Goal: "Resume after a killed process", Version: 1, Source: source,
		Steps: []workflow.StepSpec{
			{ID: "build", Executor: workflow.ExecutorSpec{Kind: workflow.KindTool, ToolID: &readFile}, Timeout: 30 * time.Second},
			{ID: "hold", DependsOn: []string{"build"}, Executor: workflow.ExecutorSpec{Kind: workflow.KindWait, Event: &ciEvent}, Timeout: 5 * time.Minute},
			{ID: "ship", DependsOn: []string{"hold"}, Executor: workflow.ExecutorSpec{Kind: workflow.KindTool, ToolID: &listDir}, Timeout: 30 * time.Second},
		},
	}
}

func countedTools(_ *testing.T, life *runLife) workflow.ToolRunner {
	return &ledgerToolRunner{ledger: life.ledger}
}

func startTheWorkflow(t *testing.T, life *runLife) {
	t.Helper()
	life.startWorkflow(t)
}

type okAgentRunner struct{}

func (okAgentRunner) Run(context.Context, *auraagent.Definition, *workflow.ExecutionContext) (json.RawMessage, error) {
	return json.RawMessage(`{"ok":true}`), nil
}

func proofSpec(credentialRef string) *workflow.Spec {
	tool := func(id, toolID, args string, depends ...string) workflow.StepSpec {
		return workflow.StepSpec{
			ID: id, DependsOn: depends,
			Executor: workflow.ExecutorSpec{Kind: workflow.KindTool, ToolID: &toolID, ToolArgs: json.RawMessage(args)},
			Timeout:  2 * time.Minute,
		}
	}
	agent := func(id, agentID string, depends ...string) workflow.StepSpec {
		return workflow.StepSpec{
			ID: id, DependsOn: depends,
			Executor: workflow.ExecutorSpec{Kind: workflow.KindAgent, AgentID: &agentID},
			Timeout:  2 * time.Minute,
		}
	}
	engineer, ciEvent, openPR := "engineer", "check_suite.completed", "open_pr"
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

func proofTools(t *testing.T, life *runLife) workflow.ToolRunner {
	t.Helper()
	effects, err := toolsbuiltin.NewChannelEffects(life.db, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("NewChannelEffects: %v", err)
	}
	broker := toolbrokertest.NewBroker(t, &toolbroker.Options{HostCapabilities: capabilitytest.Host(t), Effects: effects})
	if err := githubadapter.Register(broker, githubadapter.ToolOptions{BaseURL: life.plan.GitHub, HTTPClient: &http.Client{}}); err != nil {
		t.Fatalf("register the github tools: %v", err)
	}
	life.effects = effects
	return &runToolRunner{broker: broker, ledger: life.ledger, key: life.plan.Key}
}

type runToolRunner struct {
	broker *toolbroker.Broker
	ledger *turnLedger
	key    string
}

func (r *runToolRunner) Invoke(ctx context.Context, toolID string, args json.RawMessage) (json.RawMessage, error) {
	position := slices.Index(githubadapter.ToolNames(), toolID)
	if position < 0 {
		return nil, fmt.Errorf("tool %s is not a github tool", toolID)
	}
	if _, err := r.ledger.record(executions(toolID), ""); err != nil {
		return nil, err
	}
	id := r.key + "/" + toolID
	request := &toolbroker.ToolRequest{
		RequestID: id, TurnID: r.key, SessionID: suiteSession, PrincipalID: suiteOwner,
		ToolName: toolID, ToolVersion: githubadapter.ToolVersion, Arguments: args,
		Trust:           approval.TrustTrustedConfiguration,
		IdempotencyKey:  id,
		EventSequence:   uint64(position) + 1,
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

func approveTheGate(t *testing.T, life *runLife) {
	t.Helper()
	life.startWorkflow(t)
	waitUntil(t, "the run suspends at its gate", func() bool {
		return workflowStatus(t, life.workflows, life.plan.Run) == workflow.RunSuspended
	})
	life.signal(t, gateSignal, []byte(approveOutcome))
}

type gitHubStub struct {
	server  *httptest.Server
	creates atomic.Int64
	merges  atomic.Int64
	notes   atomic.Int64
}

func startGitHubStub(t *testing.T) *gitHubStub {
	t.Helper()
	stub := &gitHubStub{}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /repos/org/repo/pulls", stub.answer(&stub.creates, http.StatusCreated, `{"number":42,"html_url":"https://github.com/org/repo/pull/42"}`))
	mux.HandleFunc("/repos/org/repo/pulls/42/merge", stub.answer(&stub.merges, http.StatusOK, `{"merged":true,"sha":"deadbeef"}`))
	mux.HandleFunc("/repos/org/repo/issues/42/comments", stub.answer(&stub.notes, http.StatusCreated, `{"id":99,"html_url":"https://github.com/org/repo/pull/42#issuecomment-99"}`))
	stub.server = httptest.NewServer(mux)
	t.Cleanup(stub.server.Close)
	return stub
}

func (s *gitHubStub) answer(calls *atomic.Int64, status int, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+proofToken {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"message":"bad credentials"}`))
			return
		}
		calls.Add(1)
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}
}

func seedChild(t *testing.T, suite *runSuite) {
	t.Helper()
	now := time.Now().UTC()
	for _, id := range []string{childParent, childSession} {
		if err := store.NewSessionService(suite.db).Create(t.Context(), &store.Session{ID: id, OwnerID: principal.OwnerID, CreatedAt: now, UpdatedAt: now, Metadata: []byte(`{}`)}); err != nil {
			t.Fatalf("create %s: %v", id, err)
		}
	}
	run := &store.ChildRun{
		ID: childID, IdempotencyKey: "key-1",
		ParentSessionID: childParent, ParentTurnID: "turn-1", ParentInvocation: "inv-1",
		ChildSessionID: childSession, DurableKey: child.DurableChildKey(childID),
		ContextDigest: "digest-1", GrantsJSON: `[]`,
		BudgetJSON: `{"max_tokens":1000}`, State: child.StatusQueued,
		Deadline: now.Add(time.Hour), CreatedAt: now, UpdatedAt: now,
	}
	if err := store.NewChildStore(suite.db).InsertRun(t.Context(), run); err != nil {
		t.Fatalf("InsertRun: %v", err)
	}
	suite.plan.Run, suite.plan.Key = run.ID, run.DurableKey
}

func hostChild(t *testing.T, life *runLife) {
	t.Helper()
	executor := newLedgerExecutor(t, life.db, &ledgerTurn{scenario: turnScenarios["tools"], ledger: life.ledger})
	handler, err := buildChildHandler(life.db, nil, &runtimeSignaler{runtime: life.runtime}, executor, scriptedModelName)
	if err != nil {
		t.Fatalf("buildChildHandler: %v", err)
	}
	if err := registerChildHandler(life.runtime, handler); err != nil {
		t.Fatalf("registerChildHandler: %v", err)
	}
}

func startTheChild(t *testing.T, life *runLife) {
	t.Helper()
	payload, err := json.Marshal(child.StartPayload{ChildID: life.plan.Run, SessionID: childSession, DurableKey: life.plan.Key, ReservationID: "", Task: childTask})
	if err != nil {
		t.Fatalf("encode the child start payload: %v", err)
	}
	life.start(t, durable.StartRequest{Handler: child.HandlerName, Key: life.plan.Key, Payload: payload})
}
