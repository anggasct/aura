package github

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/anggasct/aura/internal/approval"
	"github.com/anggasct/aura/internal/toolbroker"
)

func stubGitHubAPI(t *testing.T) *httptest.Server {
	t.Helper()
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
	server := httptest.NewTLSServer(mux)
	t.Cleanup(server.Close)
	return server
}

func toolOptions(server *httptest.Server) ToolOptions {
	return ToolOptions{BaseURL: server.URL, client: server.Client()}
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
		`{"repo":"org/repo","title":"Add thing","head":"feature","base":"main","credential_ref":"env://GITHUB_TEST_TOKEN"}`),
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
		`{"repo":"org/repo","number":42,"merge_method":"squash","credential_ref":"env://GITHUB_TEST_TOKEN"}`),
		approval.Constraints{})
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}
	if !strings.Contains(string(merged.Output), `"merged":true`) {
		t.Errorf("merge output = %s, want merged true", merged.Output)
	}
	commented, err := CommentAdapter(options)(context.Background(), toolRequest(t,
		`{"repo":"org/repo","number":7,"body":"looks good","credential_ref":"env://GITHUB_TEST_TOKEN"}`),
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
		{"create bad repo", CreatePRAdapter(options), `{"repo":"nonsense","title":"t","head":"h","base":"b","credential_ref":"env://GITHUB_TEST_TOKEN"}`},
		{"create missing title", CreatePRAdapter(options), `{"repo":"org/repo","head":"h","base":"b","credential_ref":"env://GITHUB_TEST_TOKEN"}`},
		{"create bad credential", CreatePRAdapter(options), `{"repo":"org/repo","title":"t","head":"h","base":"b","credential_ref":"token"}`},
		{"create missing credential", CreatePRAdapter(options), `{"repo":"org/repo","title":"t","head":"h","base":"b","credential_ref":"env://GITHUB_TEST_TOKEN_ABSENT"}`},
		{"merge bad method", MergeAdapter(options), `{"repo":"org/repo","number":42,"merge_method":"fast-forward","credential_ref":"env://GITHUB_TEST_TOKEN"}`},
		{"merge zero number", MergeAdapter(options), `{"repo":"org/repo","number":0,"credential_ref":"env://GITHUB_TEST_TOKEN"}`},
		{"comment empty body", CommentAdapter(options), `{"repo":"org/repo","number":7,"body":"","credential_ref":"env://GITHUB_TEST_TOKEN"}`},
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
		`{"repo":"org/repo","title":"t","head":"h","base":"b","credential_ref":"env://GITHUB_TEST_TOKEN"}`),
		approval.Constraints{})
	if err == nil || !strings.Contains(err.Error(), "credential") {
		t.Errorf("err = %v, want credential rejection without the secret", err)
	}
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
		if len(definition.RequiredCapabilities) == 0 {
			t.Errorf("%s carries no capabilities", definition.Name)
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
