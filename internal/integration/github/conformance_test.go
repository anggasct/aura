package github

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/approval"
	"github.com/anggasct/aura/internal/effect"
	"github.com/anggasct/aura/internal/store"
	"github.com/anggasct/aura/internal/toolbroker"
)

func brokerEffectExecutor(t *testing.T) *effect.Executor {
	t.Helper()
	db, err := store.OpenDB(context.Background(), t.TempDir()+"/aura.db")
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := store.Migrate(context.Background(), db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	now := time.Now().UTC()
	if err := store.NewSessionService(db).Create(context.Background(), &store.Session{
		ID: "session-1", OwnerID: "owner-1", CreatedAt: now, UpdatedAt: now,
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

func TestBrokerConformance(t *testing.T) {
	t.Setenv("GITHUB_TEST_TOKEN", "test-token")
	server := stubGitHubAPI(t)
	executor := brokerEffectExecutor(t)
	broker, err := toolbroker.New(&toolbroker.Options{Effects: executor})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := Register(broker, ToolOptions{BaseURL: server.URL, client: server.Client()}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	names := map[string]bool{}
	for _, definition := range broker.Definitions() {
		names[definition.Name] = true
	}
	for _, name := range []string{ToolCreatePR, ToolMerge, ToolComment} {
		if !names[name] {
			t.Errorf("broker definitions miss %s", name)
		}
	}

	request := &toolbroker.ToolRequest{
		RequestID: "request-1", TurnID: "turn-1", SessionID: "session-1", PrincipalID: "owner-1",
		ToolName: ToolCreatePR, ToolVersion: ToolVersion,
		Arguments:      json.RawMessage(`{"repo":"org/repo","title":"Add thing","head":"feature","base":"main","credential_ref":"env://GITHUB_TEST_TOKEN"}`),
		Capabilities:   []string{"repository.write"},
		Trust:          approval.TrustTrustedConfiguration,
		IdempotencyKey: "idempotency-1",
		EventSequence:  1,
	}
	decision, err := broker.Evaluate(context.Background(), request)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if decision.Outcome != toolbroker.PolicyOutcomeRequireApproval {
		t.Errorf("policy outcome = %q, want approval required", decision.Outcome)
	}
	grant, err := broker.Grant(context.Background(), request, time.Minute)
	if err != nil {
		t.Fatalf("Grant: %v", err)
	}
	request.Approval = &grant
	result, err := broker.Execute(context.Background(), request)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if result.Class != toolbroker.ResultOK {
		t.Fatalf("result class = %q, want ok", result.Class)
	}
	if !strings.Contains(string(result.Output), `"external_id":"org/repo#42"`) {
		t.Errorf("output = %s, want the correlation identity", result.Output)
	}
	intents, err := executor.Journal().ListByState(context.Background(), effect.StateSucceeded, 0)
	if err != nil {
		t.Fatalf("list intents: %v", err)
	}
	if len(intents) != 1 || intents[0].Operation != ToolCreatePR || intents[0].Classification != effect.ClassificationEffectful {
		t.Fatalf("succeeded intents = %+v, want one effectful create_pr", intents)
	}
}

func TestBrokerDeniesWithoutCapability(t *testing.T) {
	executor := brokerEffectExecutor(t)
	broker, err := toolbroker.New(&toolbroker.Options{Effects: executor})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := Register(broker, ToolOptions{}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	request := &toolbroker.ToolRequest{
		RequestID: "request-2", TurnID: "turn-1", SessionID: "session-1", PrincipalID: "owner-1",
		ToolName: ToolCreatePR, ToolVersion: ToolVersion,
		Arguments:      json.RawMessage(`{"repo":"org/repo","title":"t","head":"h","base":"b","credential_ref":"env://GITHUB_TEST_TOKEN"}`),
		Capabilities:   []string{"repository.read"},
		Trust:          approval.TrustTrustedConfiguration,
		IdempotencyKey: "idempotency-2",
	}
	decision, err := broker.Evaluate(context.Background(), request)
	if err == nil || !strings.Contains(err.Error(), "repository.write") {
		t.Errorf("Evaluate without capability = %+v, %v; want capability rejection", decision, err)
	}
}
