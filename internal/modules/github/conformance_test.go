package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/platform/approval"
	"github.com/anggasct/aura/internal/platform/capability/capabilitytest"
	"github.com/anggasct/aura/internal/platform/effect"
	"github.com/anggasct/aura/internal/platform/toolbroker"
	"github.com/anggasct/aura/internal/platform/tools"
	"github.com/anggasct/aura/internal/store"
	"github.com/anggasct/aura/internal/store/storetest"
)

func brokerEffectExecutor(t *testing.T) *effect.Executor {
	t.Helper()
	db := storetest.OpenMigrated(t)
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
	broker, err := toolbroker.New(&toolbroker.Options{HostCapabilities: capabilitytest.Host(t), Effects: executor})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := Register(broker, toolOptions(server)); err != nil {
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
		Arguments:      json.RawMessage(`{"repo":"org/repo","title":"Add thing","head":"feature","base":"main","credential_name":"ci"}`),
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

func TestBrokerDeniesARequestClaimingACapabilityTheHostLacks(t *testing.T) {
	executor := brokerEffectExecutor(t)
	broker, err := toolbroker.New(&toolbroker.Options{HostCapabilities: capabilitytest.Host(t), Effects: executor})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := Register(broker, &ToolOptions{Credentials: testCredentials()}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	request := &toolbroker.ToolRequest{
		RequestID: "request-2", TurnID: "turn-1", SessionID: "session-1", PrincipalID: "owner-1",
		ToolName: ToolCreatePR, ToolVersion: ToolVersion,
		Arguments:      json.RawMessage(`{"repo":"org/repo","title":"t","head":"h","base":"b","credential_name":"ci"}`),
		Capabilities:   []string{"repository.write"},
		Trust:          approval.TrustTrustedConfiguration,
		IdempotencyKey: "idempotency-2",
	}
	_, err = broker.Evaluate(context.Background(), request)
	if code, ok := approval.CodeOf(err); !ok || code != approval.ErrorCodeCapabilityUnavailable {
		t.Errorf("Evaluate claiming an unregistered capability error = %v, want capability_unavailable", err)
	}
}

func TestBrokerSchemasMatchValidators(t *testing.T) {
	registrar := &recordingRegistrar{}
	if err := Register(registrar, &ToolOptions{Credentials: testCredentials()}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	t.Run("plain", func(t *testing.T) { assertSchemasMatchValidators(t, Definitions()) })
	t.Run("registered", func(t *testing.T) { assertSchemasMatchValidators(t, registrar.definitions) })
}

func assertSchemasMatchValidators(t *testing.T, definitions []tools.Definition) {
	t.Helper()
	byName := map[string]bool{}
	for i := range definitions {
		definition := &definitions[i]
		byName[definition.Name] = true
	}
	expectedRequired := map[string]map[string]bool{
		ToolCreatePR: {"repo": true, "credential_name": true, "title": true, "head": true, "base": true},
		ToolMerge:    {"repo": true, "credential_name": true, "number": true, "sha": true},
		ToolComment:  {"repo": true, "credential_name": true, "number": true, "body": true},
	}
	for i := range definitions {
		definition := &definitions[i]
		var schema struct {
			Type       string `json:"type"`
			Properties map[string]struct {
				Type        string `json:"type"`
				Minimum     *int   `json:"minimum"`
				MaxLength   *int   `json:"maxLength"`
				Description string `json:"description"`
			} `json:"properties"`
			Required             []string `json:"required"`
			AdditionalProperties *bool    `json:"additionalProperties"`
		}
		if err := json.Unmarshal(definition.Schema, &schema); err != nil {
			t.Fatalf("%s schema is not JSON: %v", definition.Name, err)
		}
		want, ok := expectedRequired[definition.Name]
		if !ok {
			t.Fatalf("unexpected tool %s", definition.Name)
		}
		if schema.AdditionalProperties == nil || *schema.AdditionalProperties {
			t.Errorf("%s schema additionalProperties = %v, want false", definition.Name, schema.AdditionalProperties)
		}
		if body, ok := schema.Properties["body"]; ok && (body.MaxLength == nil || *body.MaxLength != 800 || body.Description == "") {
			t.Errorf("%s body property = %+v, want maxLength 800 and a description", definition.Name, body)
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
		ToolCreatePR: `{"repo":"org/repo","title":"t","head":"h","base":"b","credential_name":"ci"}`,
		ToolMerge:    `{"repo":"org/repo","number":42,"sha":"0123456789abcdef0123456789abcdef01234567","credential_name":"ci"}`,
		ToolComment:  `{"repo":"org/repo","number":7,"body":"hi","credential_name":"ci"}`,
	}
	for i := range definitions {
		definition := &definitions[i]
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
		ToolMerge:   `{"repo":"org/repo","number":"42","sha":"0123456789abcdef0123456789abcdef01234567","credential_name":"ci"}`,
		ToolComment: `{"repo":"org/repo","number":"7","body":"hi","credential_name":"ci"}`,
	}
	for i := range definitions {
		definition := &definitions[i]
		raw, ok := wrongNumerics[definition.Name]
		if !ok {
			continue
		}
		if _, err := definition.Validate(json.RawMessage(raw)); err == nil {
			t.Errorf("%s accepts string-typed number, want rejection per integer schema", definition.Name)
		}
	}
}

func journalSize(t *testing.T, executor *effect.Executor) int {
	t.Helper()
	total := 0
	for _, state := range []effect.State{effect.StatePrepared, effect.StateStarted, effect.StateSucceeded, effect.StateUnknown, effect.StateFailed} {
		intents, err := executor.Journal().ListByState(context.Background(), state, 0)
		if err != nil {
			t.Fatalf("list %s intents: %v", state, err)
		}
		total += len(intents)
	}
	return total
}

func TestRegisteredToolsRefuseAnUnconfiguredCredentialBeforeApproval(t *testing.T) {
	t.Setenv("GITHUB_TEST_TOKEN", "test-token")
	server, requests := stubGitHubAPICounting(t)
	executor := brokerEffectExecutor(t)
	var asked atomic.Int32
	broker, err := toolbroker.New(&toolbroker.Options{
		HostCapabilities: capabilitytest.Host(t),
		Effects:          executor,
		ApprovalDecider: func(context.Context, *toolbroker.ApprovalPrompt) (bool, error) {
			asked.Add(1)
			return true, nil
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	creds := []Credential{{Name: "ci", Ref: "env://GITHUB_TEST_TOKEN"}}
	if err := Register(broker, &ToolOptions{BaseURL: server.URL, HTTPClient: server.Client(), Credentials: creds}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	var sequence uint64
	execute := func(credential string) error {
		sequence++
		key := "idempotency-" + strconv.FormatUint(sequence, 10)
		_, err := broker.Execute(context.Background(), &toolbroker.ToolRequest{
			RequestID: "request-" + key, TurnID: "turn-1", SessionID: "session-1", PrincipalID: "owner-1",
			ToolName: ToolCreatePR, ToolVersion: ToolVersion,
			Arguments:      json.RawMessage(`{"repo":"org/repo","title":"t","head":"h","base":"b","credential_name":"` + credential + `"}`),
			Trust:          approval.TrustTrustedConfiguration,
			IdempotencyKey: key,
			EventSequence:  sequence,
		})
		return err
	}
	assertRefusedBeforeApproval := func(credential string, wantAsked, wantRequests int32) {
		t.Helper()
		err := execute(credential)
		var coded *toolbroker.Error
		if !errors.As(err, &coded) || coded.Class != toolbroker.ResultInvalidArgument || coded.Detail != "credential is not a configured github credential" {
			t.Errorf("credential %q err = %v, want invalid_argument not a configured github credential", credential, err)
		}
		if asked.Load() != wantAsked || requests.Load() != wantRequests {
			t.Errorf("credential %q asked = %d, requests = %d, want %d and %d", credential, asked.Load(), requests.Load(), wantAsked, wantRequests)
		}
	}
	assertRefusedBeforeApproval("other", 0, 0)
	if size := journalSize(t, executor); size != 0 {
		t.Fatalf("journal holds %d intents after a refused credential, want none", size)
	}
	if err := execute("ci"); err != nil {
		t.Fatalf("configured credential: %v", err)
	}
	if asked.Load() != 1 || requests.Load() != 1 {
		t.Fatalf("asked = %d, requests = %d, want one each", asked.Load(), requests.Load())
	}
	creds[0].Name = "other"
	if err := execute("ci"); err != nil {
		t.Fatalf("credential after the caller renamed its slice: %v", err)
	}
	assertRefusedBeforeApproval("other", 2, 2)
}

type recordingRegistrar struct {
	registered  []string
	definitions []tools.Definition
}

func (r *recordingRegistrar) RegisterTool(definition *tools.Definition, _ toolbroker.Adapter, _ *approval.Rule) error {
	r.registered = append(r.registered, definition.Name)
	r.definitions = append(r.definitions, *definition)
	return nil
}

func TestRegisterAdvertisesTheConfiguredCredentialNames(t *testing.T) {
	registrar := &recordingRegistrar{}
	err := Register(registrar, &ToolOptions{Credentials: []Credential{
		{Name: "ci", Ref: "env://GITHUB_TEST_TOKEN"},
		{Name: "release", Ref: "file:///run/aura/gh-release"},
	}})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if len(registrar.definitions) != 3 {
		t.Fatalf("registered %d definitions, want 3", len(registrar.definitions))
	}
	for i := range registrar.definitions {
		definition := &registrar.definitions[i]
		var schema struct {
			Required   []string `json:"required"`
			Properties map[string]struct {
				Type string   `json:"type"`
				Enum []string `json:"enum"`
			} `json:"properties"`
		}
		if err := json.Unmarshal(definition.Schema, &schema); err != nil {
			t.Fatalf("%s schema is not JSON: %v", definition.Name, err)
		}
		if got := schema.Properties["credential_name"].Enum; !slices.Equal(got, []string{"ci", "release"}) {
			t.Errorf("%s credential_name enum = %v, want [ci release]", definition.Name, got)
		}
		if !slices.Contains(schema.Required, "credential_name") {
			t.Errorf("%s required = %v, want credential_name", definition.Name, schema.Required)
		}
		for _, leak := range []string{"env:", "file:", "GITHUB_TEST_TOKEN", "/run/aura"} {
			if strings.Contains(string(definition.Schema), leak) {
				t.Errorf("%s schema leaks %q: %s", definition.Name, leak, definition.Schema)
			}
		}
		for _, name := range []string{"ci", "release"} {
			if _, err := definition.Validate(validArguments(definition.Name, name)); err != nil {
				t.Errorf("%s rejects the configured credential %q: %v", definition.Name, name, err)
			}
		}
		if _, err := definition.Validate(validArguments(definition.Name, "other")); err == nil {
			t.Errorf("%s accepts an unconfigured credential", definition.Name)
		}
	}
	for _, definition := range Definitions() {
		if strings.Contains(string(definition.Schema), `"enum"`) {
			t.Errorf("%s plain definition schema carries an enum", definition.Name)
		}
	}
}

func validArguments(tool, credential string) json.RawMessage {
	arguments := map[string]string{
		ToolCreatePR: `{"repo":"org/repo","title":"t","head":"h","base":"b","credential_name":%q}`,
		ToolMerge:    `{"repo":"org/repo","number":42,"sha":"0123456789abcdef0123456789abcdef01234567","credential_name":%q}`,
		ToolComment:  `{"repo":"org/repo","number":7,"body":"hi","credential_name":%q}`,
	}
	return json.RawMessage(fmt.Sprintf(arguments[tool], credential))
}

func TestRegisterRefusesMissingEmptyOrRepeatedCredentials(t *testing.T) {
	cases := []struct {
		name    string
		options *ToolOptions
	}{
		{"nil options", nil},
		{"nil list", &ToolOptions{}},
		{"empty list", &ToolOptions{Credentials: []Credential{}}},
		{"repeated name", &ToolOptions{Credentials: []Credential{{Name: "ci", Ref: "env://A"}, {Name: "ci", Ref: "env://B"}}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			registrar := &recordingRegistrar{}
			err := Register(registrar, tc.options)
			var coded *toolbroker.Error
			if !errors.As(err, &coded) || coded.Class != toolbroker.ResultInvalidArgument {
				t.Fatalf("err = %v, want an invalid_argument error", err)
			}
			if len(registrar.registered) != 0 {
				t.Errorf("registered = %v, want nothing", registrar.registered)
			}
			if tc.name == "repeated name" && (!strings.Contains(coded.Detail, `"ci"`) || strings.Contains(coded.Detail, "env://")) {
				t.Errorf("detail = %q, want the name and no reference", coded.Detail)
			}
		})
	}
}

func TestApprovalCardShowsTheTargetFirstAndRefusesWhatDoesNotFit(t *testing.T) {
	t.Setenv("GITHUB_TEST_TOKEN", "test-token")
	server, requests := stubGitHubAPICounting(t)
	executor := brokerEffectExecutor(t)
	var prompts []*toolbroker.ApprovalPrompt
	broker, err := toolbroker.New(&toolbroker.Options{
		HostCapabilities: capabilitytest.Host(t),
		Effects:          executor,
		ApprovalDecider: func(_ context.Context, prompt *toolbroker.ApprovalPrompt) (bool, error) {
			prompts = append(prompts, prompt)
			return true, nil
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := Register(broker, toolOptions(server)); err != nil {
		t.Fatalf("Register: %v", err)
	}
	var sequence uint64
	execute := func(tool, arguments string) error {
		sequence++
		key := "idempotency-" + strconv.FormatUint(sequence, 10)
		_, err := broker.Execute(context.Background(), &toolbroker.ToolRequest{
			RequestID: "request-" + key, TurnID: "turn-1", SessionID: "session-1", PrincipalID: "owner-1",
			ToolName: tool, ToolVersion: ToolVersion,
			Arguments:      json.RawMessage(arguments),
			Trust:          approval.TrustTrustedConfiguration,
			IdempotencyKey: key,
			EventSequence:  sequence,
		})
		return err
	}
	err = execute(ToolComment, `{"repo":"org/repo","number":7,"credential_name":"ci","body":"`+strings.Repeat("b", 2048)+`"}`)
	var coded *toolbroker.Error
	if !errors.As(err, &coded) || coded.Class != toolbroker.ResultApprovalDisplayTooLarge {
		t.Fatalf("long body error = %v, want approval_display_too_large", err)
	}
	if len(prompts) != 0 || requests.Load() != 0 || journalSize(t, executor) != 0 {
		t.Fatalf("prompts = %d requests = %d intents = %d, want none of them", len(prompts), requests.Load(), journalSize(t, executor))
	}
	if err := execute(ToolComment, `{"body":"looks good","credential_name":"ci","number":7,"repo":"org/repo"}`); err != nil {
		t.Fatalf("short comment: %v", err)
	}
	if want := "repo: org/repo\nnumber: 7\ncredential_name: ci\nbody: looks good"; len(prompts) != 1 || prompts[0].Arguments != want {
		t.Fatalf("comment prompts = %+v, want arguments %q", prompts, want)
	}
	if err := execute(ToolCreatePR, `{"body":"text","title":"Add thing","base":"main","head":"feature","credential_name":"ci","repo":"org/repo"}`); err != nil {
		t.Fatalf("create pr: %v", err)
	}
	if want := "repo: org/repo\nhead: feature\nbase: main\ntitle: Add thing\ncredential_name: ci\nbody: text"; len(prompts) != 2 || prompts[1].Arguments != want {
		t.Fatalf("create pr prompts = %+v, want arguments %q", prompts, want)
	}
	for _, definition := range Definitions() {
		if err := tools.CheckDisplaySchema(definition.Display, definition.Schema); err != nil {
			t.Errorf("%s display does not fit its schema: %v", definition.Name, err)
		}
	}
}
