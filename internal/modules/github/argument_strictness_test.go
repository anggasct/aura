package github

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/anggasct/aura/internal/platform/approval"
	"github.com/anggasct/aura/internal/platform/toolbroker"
)

const strictHead = "0123456789abcdef0123456789abcdef01234567"

func argumentsForRepo(t *testing.T, tool, repo string) json.RawMessage {
	t.Helper()
	quoted, err := json.Marshal(repo)
	if err != nil {
		t.Fatalf("encode repo: %v", err)
	}
	templates := map[string]string{
		ToolCreatePR: `{"repo":%s,"title":"t","head":"h","base":"b","credential_name":"ci"}`,
		ToolMerge:    `{"repo":%s,"number":42,"sha":"` + strictHead + `","merge_method":"squash","credential_name":"ci"}`,
		ToolComment:  `{"repo":%s,"number":7,"body":"hi","credential_name":"ci"}`,
	}
	return json.RawMessage(fmt.Sprintf(templates[tool], quoted))
}

func requireInvalidArgument(t *testing.T, err error) {
	t.Helper()
	if code, ok := toolbroker.CodeOf(err); !ok || code != toolbroker.ResultInvalidArgument {
		t.Fatalf("err = %v, want invalid_argument", err)
	}
}

func TestRepoArgumentIsStrictBeforeTheCard(t *testing.T) {
	refused := []string{
		"a/b#x", "a/b?x", "a/%2Fb", "a/..", "a/.", "./b", "a/b/c",
		"a b/c", " org/repo", "org/repo ",
		"-org/repo", "my.org/repo",
		strings.Repeat("o", 40) + "/repo",
		"org/" + strings.Repeat("r", 101),
		"оrg/repo",
	}
	accepted := []string{
		"octo-org/my.repo_1",
		"user_ent1/r",
		strings.Repeat("o", 39) + "/" + strings.Repeat("r", 100),
	}
	definitions := Definitions()
	for i := range definitions {
		definition := &definitions[i]
		validate := func(repo string) error {
			_, err := definition.Validate(argumentsForRepo(t, definition.Name, repo))
			return err
		}
		for _, repo := range refused {
			t.Run(fmt.Sprintf("%s refuses %q", definition.Name, repo), func(t *testing.T) {
				requireInvalidArgument(t, validate(repo))
			})
		}
		for _, repo := range accepted {
			t.Run(fmt.Sprintf("%s accepts %q", definition.Name, repo), func(t *testing.T) {
				if err := validate(repo); err != nil {
					t.Fatalf("validate: %v", err)
				}
			})
		}
	}
}

func TestAdaptersRefuseABadRepo(t *testing.T) {
	t.Setenv("GITHUB_TEST_TOKEN", "test-token")
	server, requests := stubGitHubAPICounting(t)
	options := toolOptions(server)
	adapters := map[string]toolbroker.Adapter{
		ToolCreatePR: CreatePRAdapter(options),
		ToolMerge:    MergeAdapter(options),
		ToolComment:  CommentAdapter(options),
	}
	for _, tool := range ToolNames() {
		for _, repo := range []string{"a/b#x", " org/repo"} {
			t.Run(fmt.Sprintf("%s refuses %q", tool, repo), func(t *testing.T) {
				before := requests.Load()
				_, err := adapters[tool](t.Context(), toolRequest(t, string(argumentsForRepo(t, tool, repo))), approval.Constraints{})
				requireInvalidArgument(t, err)
				if got := requests.Load() - before; got != 0 {
					t.Errorf("requests = %d, want none", got)
				}
			})
		}
	}
}

func TestRepoPathEscapesEachSegment(t *testing.T) {
	ref := repoRef{owner: "a#b", name: "c/d?%"}
	if got, want := ref.path("pulls", "1", "merge"), "/repos/a%23b/c%2Fd%3F%25/pulls/1/merge"; got != want {
		t.Errorf("path = %q, want %q", got, want)
	}
	t.Setenv("GITHUB_TEST_TOKEN", "test-token")
	server, requests := stubGitHubAPICounting(t)
	adapter := CommentAdapter(toolOptions(server))
	before := requests.Load()
	if _, err := adapter(t.Context(), toolRequest(t, string(argumentsForRepo(t, ToolComment, "org/repo"))), approval.Constraints{}); err != nil {
		t.Fatalf("comment on org/repo: %v", err)
	}
	if got := requests.Load() - before; got != 1 {
		t.Errorf("requests = %d, want 1", got)
	}
	if got, want := (repoRef{owner: "octo-org", name: "my.repo_1"}).path("issues", "7", "comments"), "/repos/octo-org/my.repo_1/issues/7/comments"; got != want {
		t.Errorf("valid path = %q, want %q", got, want)
	}
}

func TestMergeRequiresAnExplicitMethod(t *testing.T) {
	t.Setenv("GITHUB_TEST_TOKEN", "test-token")
	definition := Definitions()[1]
	withMethod := func(method string) json.RawMessage {
		return json.RawMessage(`{"repo":"org/repo","number":42,"sha":"` + strictHead + `",` + method + `"credential_name":"ci"}`)
	}
	cases := []struct {
		name string
		raw  json.RawMessage
	}{
		{"missing", withMethod("")},
		{"unknown", withMethod(`"merge_method":"fast-forward",`)},
		{"wrong case", withMethod(`"merge_method":"Squash",`)},
	}
	for _, tc := range cases {
		t.Run("validator refuses "+tc.name, func(t *testing.T) {
			_, err := definition.Validate(tc.raw)
			requireInvalidArgument(t, err)
		})
	}
	for _, method := range []string{"merge", "squash", "rebase"} {
		if _, err := definition.Validate(withMethod(`"merge_method":"` + method + `",`)); err != nil {
			t.Errorf("validator refused %s: %v", method, err)
		}
	}

	var schema struct {
		Required   []string `json:"required"`
		Properties map[string]struct {
			Enum []string `json:"enum"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(definition.Schema, &schema); err != nil {
		t.Fatalf("schema is not JSON: %v", err)
	}
	if !slices.Contains(schema.Required, "merge_method") {
		t.Errorf("required = %v, want merge_method", schema.Required)
	}
	if got := schema.Properties["merge_method"].Enum; !slices.Equal(got, []string{"merge", "squash", "rebase"}) {
		t.Errorf("merge_method enum = %v, want merge, squash, rebase", got)
	}

	server, requests := stubGitHubAPICounting(t)
	adapter := MergeAdapter(toolOptions(server))
	before := requests.Load()
	_, err := adapter(t.Context(), toolRequest(t, string(withMethod(""))), approval.Constraints{})
	requireInvalidArgument(t, err)
	if got := requests.Load() - before; got != 0 {
		t.Errorf("requests without a method = %d, want none", got)
	}

	card, err := definition.ApprovalText(withMethod(`"merge_method":"squash",`), nil)
	if err != nil {
		t.Fatalf("card: %v", err)
	}
	if sha, method := strings.Index(card, "sha:"), strings.Index(card, "merge_method: squash"); sha < 0 || method < sha {
		t.Errorf("card = %q, want merge_method: squash after sha", card)
	}
}
