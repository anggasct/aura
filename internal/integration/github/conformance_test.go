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

func TestBrokerSchemasMatchValidators(t *testing.T) {
	definitions := Definitions()
	byName := map[string]bool{}
	for _, definition := range definitions {
		byName[definition.Name] = true
	}
	expectedRequired := map[string]map[string]bool{
		ToolCreatePR: {"repo": true, "credential_ref": true, "title": true, "head": true, "base": true},
		ToolMerge:    {"repo": true, "credential_ref": true, "number": true},
		ToolComment:  {"repo": true, "credential_ref": true, "number": true, "body": true},
	}
	for _, definition := range definitions {
		var schema struct {
			Type       string `json:"type"`
			Properties map[string]struct {
				Type    string `json:"type"`
				Minimum *int   `json:"minimum"`
			} `json:"properties"`
			Required []string `json:"required"`
		}
		if err := json.Unmarshal(definition.Schema, &schema); err != nil {
			t.Fatalf("%s schema is not JSON: %v", definition.Name, err)
		}
		want, ok := expectedRequired[definition.Name]
		if !ok {
			t.Fatalf("unexpected tool %s", definition.Name)
		}
		if len(schema.Required) != len(want) {
			t.Errorf("%s required = %v, want %d fields", definition.Name, schema.Required, len(want))
		}
		for _, field := range schema.Required {
			if !want[field] {
				t.Errorf("%s schema marks optional field %q as required", definition.Name, field)
			}
		}
		for field := range want {
			found := false
			for _, required := range schema.Required {
				if required == field {
					found = true
					break
				}
			}
			if !found {
				t.Errorf("%s schema omits required field %q", definition.Name, field)
			}
		}
		if prop, ok := schema.Properties["number"]; ok {
			if prop.Type != "integer" {
				t.Errorf("%s number type = %q, want integer", definition.Name, prop.Type)
			}
			if prop.Minimum == nil || *prop.Minimum != 1 {
				t.Errorf("%s number minimum = %v, want 1", definition.Name, prop.Minimum)
			}
		} else if definition.Name == ToolMerge || definition.Name == ToolComment {
			t.Errorf("%s schema misses number property", definition.Name)
		}
	}
	minimal := map[string]string{
		ToolCreatePR: `{"repo":"org/repo","title":"t","head":"h","base":"b","credential_ref":"env://GITHUB_TEST_TOKEN"}`,
		ToolMerge:    `{"repo":"org/repo","number":42,"credential_ref":"env://GITHUB_TEST_TOKEN"}`,
		ToolComment:  `{"repo":"org/repo","number":7,"body":"hi","credential_ref":"env://GITHUB_TEST_TOKEN"}`,
	}
	for _, definition := range definitions {
		raw, err := definition.Validate(json.RawMessage(minimal[definition.Name]))
		if err != nil {
			t.Errorf("%s rejects minimal valid arguments: %v", definition.Name, err)
			continue
		}
		var schema struct {
			Required   []string `json:"required"`
			Properties map[string]struct {
				Type string `json:"type"`
			} `json:"properties"`
		}
		if err := json.Unmarshal(definition.Schema, &schema); err != nil {
			t.Fatalf("%s schema decode: %v", definition.Name, err)
		}
		var document map[string]json.RawMessage
		if err := json.Unmarshal(raw, &document); err != nil {
			t.Fatalf("%s validated output is not JSON: %v", definition.Name, err)
		}
		for _, field := range schema.Required {
			if _, ok := document[field]; !ok {
				t.Errorf("%s minimal arguments miss schema-required field %q", definition.Name, field)
			}
		}
	}
	wrongNumerics := map[string]string{
		ToolMerge:   `{"repo":"org/repo","number":"42","credential_ref":"env://GITHUB_TEST_TOKEN"}`,
		ToolComment: `{"repo":"org/repo","number":"7","body":"hi","credential_ref":"env://GITHUB_TEST_TOKEN"}`,
	}
	for _, definition := range definitions {
		raw, ok := wrongNumerics[definition.Name]
		if !ok {
			continue
		}
		if _, err := definition.Validate(json.RawMessage(raw)); err == nil {
			t.Errorf("%s accepts string-typed number, want rejection per integer schema", definition.Name)
		}
	}
}
