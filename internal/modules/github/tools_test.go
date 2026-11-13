package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/anggasct/aura/internal/platform/approval"
	"github.com/anggasct/aura/internal/platform/toolbroker"
)

var providerSafeName = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

func stubGitHubAPI(t *testing.T) *httptest.Server {
	t.Helper()
	server, _ := stubGitHubAPICounting(t)
	return server
}

func stubGitHubAPICounting(t *testing.T) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var requests atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/org/repo/pulls", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		if r.Header.Get("Authorization") != "Bearer test-token" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"message":"bad credentials"}`))
			return
		}
		var body struct {
			Title string `json:"title"`
			Head  string `json:"head"`
			Base  string `json:"base"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Title == "" {
			w.WriteHeader(http.StatusUnprocessableEntity)
			_, _ = w.Write([]byte(`{"message":"title is required"}`))
			return
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"number":42,"html_url":"https://github.com/org/repo/pull/42"}`))
	})
	mux.HandleFunc("/repos/org/repo/pulls/42/merge", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-token" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"message":"bad credentials"}`))
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"merged":true,"sha":"abc123"}`))
	})
	mux.HandleFunc("/repos/org/repo/issues/7/comments", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-token" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"message":"bad credentials"}`))
			return
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":99,"html_url":"https://github.com/org/repo/pull/42#issuecomment-99"}`))
	})
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		mux.ServeHTTP(w, r)
	}))
	t.Cleanup(server.Close)
	return server, &requests
}

func testCredentials() []Credential {
	return []Credential{
		{Name: "ci", Ref: "env://GITHUB_TEST_TOKEN"},
		{Name: "absent", Ref: "env://GITHUB_TEST_TOKEN_ABSENT"},
	}
}

func toolOptions(server *httptest.Server) *ToolOptions {
	return &ToolOptions{BaseURL: server.URL, HTTPClient: server.Client(), Credentials: testCredentials()}
}

func toolRequest(t *testing.T, args string) *toolbroker.ToolRequest {
	t.Helper()
	return &toolbroker.ToolRequest{
		RequestID: "req-1", TurnID: "turn-1", SessionID: "sess-1",
		ToolName: "github", ToolVersion: ToolVersion,
		Arguments: json.RawMessage(args),
		Trust:     approval.TrustTrustedConfiguration,
	}
}

func TestCreatePRReturnsCorrelationIdentity(t *testing.T) {
	t.Setenv("GITHUB_TEST_TOKEN", "test-token")
	server := stubGitHubAPI(t)
	adapter := CreatePRAdapter(toolOptions(server))
	result, err := adapter(context.Background(), toolRequest(t,
		`{"repo":"org/repo","title":"Add thing","head":"feature","base":"main","credential_name":"ci"}`),
		approval.Constraints{})
	if err != nil {
		t.Fatalf("CreatePR: %v", err)
	}
	if result.Class != toolbroker.ResultOK || !result.Untrusted {
		t.Errorf("result = %+v, want ok untrusted output", result)
	}
	var output struct {
		ExternalID string `json:"external_id"`
		Number     int    `json:"number"`
	}
	if err := json.Unmarshal(result.Output, &output); err != nil {
		t.Fatalf("decode output: %v", err)
	}
	if output.ExternalID != "org/repo#42" || output.Number != 42 {
		t.Errorf("output = %s, want external identity org/repo#42", result.Output)
	}
}

func TestMergeAndCommentRoundTrip(t *testing.T) {
	t.Setenv("GITHUB_TEST_TOKEN", "test-token")
	server := stubGitHubAPI(t)
	options := toolOptions(server)
	merged, err := MergeAdapter(options)(context.Background(), toolRequest(t,
		`{"repo":"org/repo","number":42,"sha":"0123456789abcdef0123456789abcdef01234567","merge_method":"squash","credential_name":"ci"}`),
		approval.Constraints{})
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}
	if !strings.Contains(string(merged.Output), `"merged":true`) {
		t.Errorf("merge output = %s, want merged true", merged.Output)
	}
	commented, err := CommentAdapter(options)(context.Background(), toolRequest(t,
		`{"repo":"org/repo","number":7,"body":"looks good","credential_name":"ci"}`),
		approval.Constraints{})
	if err != nil {
		t.Fatalf("Comment: %v", err)
	}
	if !strings.Contains(string(commented.Output), `"id":99`) {
		t.Errorf("comment output = %s, want id 99", commented.Output)
	}
}

func TestToolsRejectBadInput(t *testing.T) {
	t.Setenv("GITHUB_TEST_TOKEN", "test-token")
	server := stubGitHubAPI(t)
	options := toolOptions(server)
	cases := []struct {
		name    string
		adapter toolbroker.Adapter
		args    string
	}{
		{"create bad repo", CreatePRAdapter(options), `{"repo":"nonsense","title":"t","head":"h","base":"b","credential_name":"ci"}`},
		{"create missing title", CreatePRAdapter(options), `{"repo":"org/repo","head":"h","base":"b","credential_name":"ci"}`},
		{"create bad credential", CreatePRAdapter(options), `{"repo":"org/repo","title":"t","head":"h","base":"b","credential_name":"token"}`},
		{"create missing credential", CreatePRAdapter(options), `{"repo":"org/repo","title":"t","head":"h","base":"b","credential_name":"absent"}`},
		{"merge bad method", MergeAdapter(options), `{"repo":"org/repo","number":42,"sha":"0123456789abcdef0123456789abcdef01234567","merge_method":"fast-forward","credential_name":"ci"}`},
		{"merge zero number", MergeAdapter(options), `{"repo":"org/repo","number":0,"sha":"0123456789abcdef0123456789abcdef01234567","merge_method":"merge","credential_name":"ci"}`},
		{"comment empty body", CommentAdapter(options), `{"repo":"org/repo","number":7,"body":"","credential_name":"ci"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := tc.adapter(context.Background(), toolRequest(t, tc.args), approval.Constraints{}); err == nil {
				t.Error("expected invalid input to fail, got nil")
			}
		})
	}
}

func TestToolsSurfaceAuthFailure(t *testing.T) {
	t.Setenv("GITHUB_TEST_TOKEN", "wrong-token")
	server := stubGitHubAPI(t)
	_, err := CreatePRAdapter(toolOptions(server))(context.Background(), toolRequest(t,
		`{"repo":"org/repo","title":"t","head":"h","base":"b","credential_name":"ci"}`),
		approval.Constraints{})
	if err == nil || !strings.Contains(err.Error(), "credential") {
		t.Errorf("err = %v, want credential rejection without the secret", err)
	}
}

var displayOrders = map[string][]string{
	ToolCreatePR: {"repo", "head", "base", "title", "credential_name", "body"},
	ToolMerge:    {"repo", "number", "sha", "merge_method", "credential_name"},
	ToolComment:  {"repo", "number", "credential_name", "body"},
}

func TestDefinitionsCarryBrokerContract(t *testing.T) {
	definitions := Definitions()
	if len(definitions) != 3 {
		t.Fatalf("definitions = %d, want three github tools", len(definitions))
	}
	seen := map[string]bool{}
	for _, definition := range definitions {
		seen[definition.Name] = true
		if definition.Version != ToolVersion {
			t.Errorf("%s version = %q, want %s", definition.Name, definition.Version, ToolVersion)
		}
		if !definition.RequiresApproval || !definition.Effectful {
			t.Errorf("%s requires approval and effect handling, got %+v", definition.Name, definition)
		}
		if !definition.NoStandingGrant || !definition.ExactDisplay {
			t.Errorf("%s NoStandingGrant = %t, ExactDisplay = %t; want both", definition.Name, definition.NoStandingGrant, definition.ExactDisplay)
		}
		if want := displayOrders[definition.Name]; !slices.Equal(definition.Display, want) {
			t.Errorf("%s display = %v, want %v", definition.Name, definition.Display, want)
		}
		if !slices.Equal(definition.RequiredCapabilities, []string{"provider-github"}) {
			t.Errorf("%s requires capabilities %v, want provider-github", definition.Name, definition.RequiredCapabilities)
		}
		if !definition.OptIn {
			t.Errorf("%s must be opt-in", definition.Name)
		}
		if want := definition.Name != ToolMerge; definition.ExternalSink != want {
			t.Errorf("%s ExternalSink = %t, want %t", definition.Name, definition.ExternalSink, want)
		}
		if !providerSafeName.MatchString(definition.Name) {
			t.Errorf("%s is not a provider-safe tool name", definition.Name)
		}
		var schema struct {
			Required   []string                   `json:"required"`
			Properties map[string]json.RawMessage `json:"properties"`
		}
		if err := json.Unmarshal(definition.Schema, &schema); err != nil {
			t.Fatalf("%s schema is not JSON: %v", definition.Name, err)
		}
		if !slices.Contains(schema.Required, "credential_name") {
			t.Errorf("%s required = %v, want the credential argument", definition.Name, schema.Required)
		}
		for key := range schema.Properties {
			if strings.HasPrefix(key, "credential") && key != "credential_name" {
				t.Errorf("%s schema has the property %q, want only credential_name", definition.Name, key)
			}
		}
	}
	for _, name := range []string{ToolCreatePR, ToolMerge, ToolComment} {
		if !seen[name] {
			t.Errorf("missing tool %s", name)
		}
	}
	rules := Rules()
	if len(rules) != 3 {
		t.Fatalf("rules = %d, want one per tool", len(rules))
	}
	for _, rule := range rules {
		if !rule.RequiresApproval {
			t.Errorf("rule %s must require approval", rule.ToolName)
		}
	}
}

func TestAdaptersRefuseACredentialThatIsNotConfigured(t *testing.T) {
	t.Setenv("GITHUB_TEST_TOKEN", "test-token")
	tokenFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenFile, []byte("test-token\n"), 0o600); err != nil {
		t.Fatalf("write token file: %v", err)
	}
	server, requests := stubGitHubAPICounting(t)
	cases := []struct {
		name  string
		build func(*ToolOptions) toolbroker.Adapter
		args  string
	}{
		{"create_pr", CreatePRAdapter, `{"repo":"org/repo","title":"t","head":"h","base":"b","credential_name":%s}`},
		{"merge", MergeAdapter, `{"repo":"org/repo","number":42,"sha":"0123456789abcdef0123456789abcdef01234567","merge_method":"merge","credential_name":%s}`},
		{"comment", CommentAdapter, `{"repo":"org/repo","number":7,"body":"hi","credential_name":%s}`},
	}
	configured := func() []Credential { return []Credential{{Name: "ci", Ref: "env://GITHUB_TEST_TOKEN"}} }
	call := func(adapter toolbroker.Adapter, template, credential string) error {
		quoted, err := json.Marshal(credential)
		if err != nil {
			t.Fatalf("encode credential: %v", err)
		}
		_, err = adapter(context.Background(), toolRequest(t, fmt.Sprintf(template, quoted)), approval.Constraints{})
		return err
	}
	assertRefused := func(t *testing.T, err error, detail string) {
		t.Helper()
		var coded *toolbroker.Error
		if !errors.As(err, &coded) || coded.Class != toolbroker.ResultInvalidArgument || coded.Detail != detail {
			t.Errorf("err = %v, want invalid_argument %q", err, detail)
		}
	}
	const notConfigured = "credential is not a configured github credential"
	for _, tool := range cases {
		t.Run(tool.name, func(t *testing.T) {
			options := &ToolOptions{BaseURL: server.URL, HTTPClient: server.Client(), Credentials: configured()}
			before := requests.Load()
			if err := call(tool.build(options), tool.args, "ci"); err != nil {
				t.Fatalf("configured credential: %v", err)
			}
			if got := requests.Load() - before; got != 1 {
				t.Fatalf("requests for the configured credential = %d, want 1", got)
			}
			before = requests.Load()
			for _, credential := range []string{"ci ", "CI", "c", "ci2", "env://GITHUB_TEST_TOKEN", "file://" + tokenFile} {
				assertRefused(t, call(tool.build(options), tool.args, credential), notConfigured)
			}
			for _, credentials := range [][]Credential{nil, {}} {
				empty := &ToolOptions{BaseURL: server.URL, HTTPClient: server.Client(), Credentials: credentials}
				assertRefused(t, call(tool.build(empty), tool.args, "ci"), notConfigured)
			}
			assertRefused(t, call(tool.build(nil), tool.args, "ci"), "github tool options are required")
			if got := requests.Load() - before; got != 0 {
				t.Errorf("requests for refused credentials = %d, want 0", got)
			}
			creds := configured()
			cloned := tool.build(&ToolOptions{BaseURL: server.URL, HTTPClient: server.Client(), Credentials: creds})
			creds[0].Name = "other"
			if err := call(cloned, tool.args, "ci"); err != nil {
				t.Errorf("credential after the caller renamed its slice: %v", err)
			}
			assertRefused(t, call(cloned, tool.args, "other"), notConfigured)
		})
	}
}

func TestMergeSendsTheExpectedHead(t *testing.T) {
	t.Setenv("GITHUB_TEST_TOKEN", "test-token")
	const head = "0123456789abcdef0123456789abcdef01234567"
	var requests atomic.Int32
	var sent atomic.Value
	status := atomic.Int32{}
	status.Store(http.StatusOK)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		var body map[string]string
		if err := json.NewDecoder(r.Body).Decode(&body); err == nil {
			sent.Store(body)
		}
		w.WriteHeader(int(status.Load()))
		_, _ = w.Write([]byte(`{"merged":true,"sha":"abc123","message":"Head branch was modified"}`))
	}))
	t.Cleanup(server.Close)
	options := toolOptions(server)
	arguments := func(sha string) json.RawMessage {
		return json.RawMessage(`{"repo":"org/repo","number":42,"sha":"` + sha + `","merge_method":"squash","credential_name":"ci"}`)
	}
	if _, err := MergeAdapter(options)(t.Context(), toolRequest(t, string(arguments(head))), approval.Constraints{}); err != nil {
		t.Fatalf("Merge: %v", err)
	}
	if body, _ := sent.Load().(map[string]string); body["sha"] != head || body["merge_method"] != "squash" {
		t.Fatalf("merge request body = %v, want the sha next to merge_method", body)
	}
	status.Store(http.StatusConflict)
	_, err := MergeAdapter(options)(t.Context(), toolRequest(t, string(arguments(head))), approval.Constraints{})
	var coded *toolbroker.Error
	if !errors.As(err, &coded) || coded.Class != toolbroker.ResultInvalidArgument || coded.Detail != headMoved || strings.Contains(err.Error(), head) {
		t.Fatalf("409 error = %v, want invalid_argument %q without the sha", err, headMoved)
	}
	validate := Definitions()[1].Validate
	before := requests.Load()
	for name, sha := range map[string]string{
		"missing":   "",
		"39 chars":  head[:39],
		"41 chars":  head + "0",
		"uppercase": strings.ToUpper(head),
		"non hex":   head[:39] + "g",
	} {
		raw := arguments(sha)
		if name == "missing" {
			raw = json.RawMessage(`{"repo":"org/repo","number":42,"merge_method":"squash","credential_name":"ci"}`)
		}
		if _, err := validate(raw); err == nil {
			t.Errorf("validator accepted a %s sha", name)
		}
	}
	if _, err := validate(arguments(head)); err != nil {
		t.Errorf("validator refused a 40 character lowercase hex sha: %v", err)
	}
	if requests.Load() != before {
		t.Errorf("validation made %d requests, want none", requests.Load()-before)
	}
}
