//go:build durable

package app

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/config"
	"github.com/anggasct/aura/internal/modules/child"
	githubadapter "github.com/anggasct/aura/internal/modules/github"
	"github.com/anggasct/aura/internal/modules/workflow"
	auraagent "github.com/anggasct/aura/internal/platform/agent"
	"github.com/anggasct/aura/internal/platform/durable"
	"github.com/anggasct/aura/internal/platform/principal"
	"github.com/anggasct/aura/internal/platform/toolbroker/toolbrokertest"
	"github.com/anggasct/aura/internal/store"
)

const (
	stubBuildTool   = "stub-build"
	stubShipTool    = "stub-ship"
	holdSignal      = "wait.hold"
	generatedID     = "gen_crash_resume"
	proofTokenEnv   = "AURA_PROOF_GITHUB_TOKEN"
	proofCredential = "proof"
	proofToken      = "proof-token"
	proofPullID     = "org/repo#42"
	childID         = "child-1"
	childParent     = "session-parent"
	childSession    = "session-child"
	childTask       = "CHILD-TASK"
	gateSignal      = "approval.gate"
	mergeSignal     = "approval.approve_merge"
	approveOutcome  = `{"decision":"approve"}`
)

type runScenario struct {
	seed      func(t *testing.T, suite *runSuite)
	host      func(t *testing.T, life *runLife)
	firstLife func(t *testing.T, life *runLife)
	started   func(t *testing.T, suite *runSuite)
}

var runScenarios = map[string]runScenario{
	"defined":            definedWorkflow.scenario(nil),
	"generated":          generatedWorkflow.scenario(nil),
	"github":             proofWorkflow.scenario(approveTheGate),
	"effect-returned":    effectReturnedWorkflow.scenario(approveTheGate),
	"effect-interrupted": effectInterruptedWorkflow.scenario(approveTheGate),
	"child":              {seed: seedChild, host: hostChild, firstLife: startTheChild},
}

type workflowStub struct {
	tool      stubTool
	effectful bool
}

type workflowShape struct {
	spec            func() *workflow.Spec
	known           []string
	effectful       []string
	budget          workflow.Budget
	resumeFromStore bool
	stubs           []workflowStub
	wire            func(t *testing.T, life *runLife, runner *workflowToolRunner) workflow.ToolRunner
}

var buildAndShipStubs = []workflowStub{{tool: stubTool{name: stubBuildTool}}, {tool: stubTool{name: stubShipTool}}}

var definedWorkflow = &workflowShape{
	spec:  func() *workflow.Spec { return crashResumeSpec("crash-resume", workflow.SourceDefined) },
	known: []string{stubBuildTool, stubShipTool},
	stubs: buildAndShipStubs,
}

var generatedWorkflow = &workflowShape{
	spec:            func() *workflow.Spec { return crashResumeSpec(generatedID, workflow.SourceGenerated) },
	known:           []string{stubBuildTool, stubShipTool},
	budget:          workflow.Budget{MaxSteps: 3, MaxTokens: 1},
	resumeFromStore: true,
	stubs:           buildAndShipStubs,
}

var proofWorkflow = &workflowShape{
	spec:      func() *workflow.Spec { return proofSpec(proofCredential) },
	known:     githubadapter.ToolNames(),
	effectful: githubadapter.ToolNames(),
	wire:      registerGitHubOnTheStub,
}

func (w *workflowShape) scenario(afterStart func(t *testing.T, life *runLife)) runScenario {
	firstLife := func(t *testing.T, life *runLife) {
		t.Helper()
		w.startRun(t, life)
		if afterStart != nil {
			afterStart(t, life)
		}
	}
	return runScenario{seed: w.seed, host: w.host, firstLife: firstLife, started: discoverTheRun}
}

func (w *workflowShape) deps(t *testing.T) *workflow.ValidationDeps {
	t.Helper()
	registry, err := auraagent.Build(nil, w.known, []string{"primary"})
	if err != nil {
		t.Fatalf("agent registry: %v", err)
	}
	return &workflow.ValidationDeps{KnownTools: w.known, EffectfulTools: append([]string{}, w.effectful...), Agents: registry, CheckToolCall: acceptToolCall}
}

func (w *workflowShape) seed(t *testing.T, suite *runSuite) {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("the production workflow tool runner needs Linux: the filesystem adapters refuse other systems")
	}
	spec := w.spec()
	if err := workflow.NewStore(suite.db).SaveDefinition(t.Context(), spec); err != nil {
		t.Fatalf("SaveDefinition: %v", err)
	}
	digest, err := workflow.Digest(spec)
	if err != nil {
		t.Fatalf("Digest: %v", err)
	}
	if _, err := suite.db.ExecContext(t.Context(),
		`INSERT INTO workflow_approval (definition_id, definition_version, spec_sha256, kind, approver_id, tainted, approved_at) VALUES (?, ?, ?, 'installed', ?, 0, ?)`,
		spec.ID, spec.Version, digest, principal.OwnerID, time.Now().UTC().Format(time.RFC3339Nano),
	); err != nil {
		t.Fatalf("approve the seeded definition: %v", err)
	}
}

func newProductionToolRunner(t *testing.T, life *runLife, cfg *config.Config) *workflowToolRunner {
	t.Helper()
	cfg.Storage.Path = filepath.Dir(life.plan.Database)
	cfg.Tools.Workspace = filepath.Join(cfg.Storage.Path, "workspace")
	if err := os.MkdirAll(cfg.Tools.Workspace, 0o700); err != nil {
		t.Fatalf("create the tool workspace: %v", err)
	}
	built, err := newWorkflowToolRunner(loadResultWithAll(t, cfg), life.db, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("newWorkflowToolRunner: %v", err)
	}
	runner, ok := built.(*workflowToolRunner)
	if !ok {
		t.Fatalf("newWorkflowToolRunner returned %T, want *workflowToolRunner", built)
	}
	return runner
}

func (w *workflowShape) registerStubs(t *testing.T, life *runLife, runner *workflowToolRunner) {
	t.Helper()
	for _, stub := range w.stubs {
		toolbrokertest.Register(t, runner.executor.Broker(), toolbrokertest.Stub{
			Name:             stub.tool.name,
			RequiresApproval: stub.tool.approval,
			Effectful:        stub.effectful,
			Run:              life.ledger.adapter(stub.tool),
		})
	}
}

func (w *workflowShape) host(t *testing.T, life *runLife) {
	t.Helper()
	deps := w.deps(t)
	cfg := config.Default()
	runner := newProductionToolRunner(t, life, &cfg)
	w.registerStubs(t, life, runner)
	var tools workflow.ToolRunner = runner
	if w.wire != nil {
		tools = w.wire(t, life, runner)
	}
	interpreter := workflow.NewInterpreter(life.workflows, life.runtime, &workflow.Options{
		MaxConcurrentSteps: workflowMaxConcurrentSteps(&cfg),
		Logger:             slog.New(slog.DiscardHandler),
		AgentResolver:      deps.Agents,
		Agents:             okAgentRunner{},
		Tools:              tools,
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
	life.interpreter = interpreter
}

func (w *workflowShape) startRun(t *testing.T, life *runLife) {
	t.Helper()
	summary, err := life.interpreter.Start(t.Context(), w.spec().ID, &workflow.RunInput{Objective: "survive the kill"}, ownerRunOrigin())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	life.plan.Run, life.plan.Key = summary.ID, summary.DurableKey
}

func discoverTheRun(t *testing.T, suite *runSuite) {
	t.Helper()
	disk := workflow.NewStore(suite.db)
	var started *workflow.RunSummary
	suite.child.waitKillPoint(t, "the first life has started its workflow run", func() bool {
		runs, err := disk.Runs(t.Context(), "")
		if err != nil {
			t.Fatalf("Runs: %v", err)
		}
		if len(runs) != 1 {
			return false
		}
		started = runs[0]
		return countRows(t, suite.db, `SELECT COUNT(*) FROM durable_run WHERE key = ?`, started.DurableKey) == 1
	})
	suite.plan.Run, suite.plan.Key = started.ID, started.DurableKey
}

func crashResumeSpec(id string, source workflow.Source) *workflow.Spec {
	build, ship, ciEvent := stubBuildTool, stubShipTool, "ci"
	args := func(step string) json.RawMessage { return json.RawMessage(`{"step":"` + step + `"}`) }
	return &workflow.Spec{
		ID: id, Goal: "Resume after a killed process", Version: 1, Source: source,
		Steps: []workflow.StepSpec{
			{ID: "build", Executor: workflow.ExecutorSpec{Kind: workflow.KindTool, ToolID: &build, ToolArgs: args("build")}, Timeout: 30 * time.Second},
			{ID: "hold", DependsOn: []string{"build"}, Executor: workflow.ExecutorSpec{Kind: workflow.KindWait, Event: &ciEvent}, Timeout: 5 * time.Minute},
			{ID: "ship", DependsOn: []string{"hold"}, Executor: workflow.ExecutorSpec{Kind: workflow.KindTool, ToolID: &ship, ToolArgs: args("ship")}, Timeout: 30 * time.Second},
		},
	}
}

type okAgentRunner struct{}

func (okAgentRunner) Run(context.Context, *auraagent.Definition, *workflow.ExecutionContext) (json.RawMessage, error) {
	return json.RawMessage(`{"ok":true}`), nil
}

func proofSpec(credential string) *workflow.Spec {
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
				`{"repo":"org/repo","title":"Add thing","head":"feature","base":"main","credential_name":"`+credential+`"}`, "test"),
			{ID: "hold_ci", DependsOn: []string{"open_pr"},
				Executor: workflow.ExecutorSpec{Kind: workflow.KindWait, Event: &ciEvent, ExternalRef: &openPR},
				Timeout:  10 * time.Minute},
			tool("review", githubadapter.ToolComment,
				`{"repo":"org/repo","number":42,"body":"looks good","credential_name":"`+credential+`"}`, "hold_ci"),
			{ID: "approve_merge", DependsOn: []string{"review"},
				Executor: workflow.ExecutorSpec{Kind: workflow.KindApproval}, Timeout: 10 * time.Minute},
			tool("merge_pr", githubadapter.ToolMerge,
				`{"repo":"org/repo","number":42,"sha":"0123456789abcdef0123456789abcdef01234567","merge_method":"merge","credential_name":"`+credential+`"}`, "approve_merge"),
		},
	}
}

func registerGitHubOnTheStub(t *testing.T, life *runLife, runner *workflowToolRunner) workflow.ToolRunner {
	t.Helper()
	if err := githubadapter.Register(runner.executor, &githubadapter.ToolOptions{BaseURL: life.plan.GitHub, HTTPClient: &http.Client{}, Credentials: []githubadapter.Credential{{Name: proofCredential, Ref: "env://" + proofTokenEnv}}}); err != nil {
		t.Fatalf("register the github tools: %v", err)
	}
	return runner
}

func approveTheGate(t *testing.T, life *runLife) {
	t.Helper()
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
