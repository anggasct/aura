package personaeval

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	auraagent "github.com/anggasct/aura/internal/platform/agent"
	"github.com/anggasct/aura/internal/platform/principal"
	"github.com/anggasct/aura/internal/platform/tools"
)

func evalTestCatalog() []tools.Definition {
	accept := func(raw json.RawMessage) (json.RawMessage, error) { return raw, nil }
	return []tools.Definition{
		{Name: "read_file", Version: tools.Version, RequiredCapabilities: []string{"workspace-read"}, Validator: accept},
		{Name: "write_file", Version: tools.Version, RequiredCapabilities: []string{"workspace-write"}, RequiresApproval: true, Effectful: true, Validator: accept},
		{Name: "exec", Version: tools.Version, RequiredCapabilities: []string{"exec-linux"}, RequiresApproval: true, Effectful: true, Validator: accept},
		{Name: "web_search", Version: tools.Version, RequiredCapabilities: []string{"provider-search"}, ExternalSink: true, Validator: accept},
		{Name: "handoff_to_owner", Version: tools.Version, OptIn: true, Orchestration: true, Validator: accept},
	}
}

func lobbyDefinition() *auraagent.Definition {
	return &auraagent.Definition{
		ID: "lobby", Name: "Lobby", Instructions: "You help visitors.", Audience: auraagent.AudiencePublic,
		Tools:          []string{"read_file", "write_file", "web_search", "handoff_to_owner"},
		PrincipalTools: []string{"read_file", "write_file", "handoff_to_owner"},
		PublicTools:    []string{"web_search", "handoff_to_owner"},
		Messages:       map[string]string{auraagent.MessageBudgetExhausted: "Kuota habis."},
	}
}

func fakeCapturer(byClass map[principal.Class]Capture) Capturer {
	return func(_ context.Context, _ *auraagent.Definition, class principal.Class) (Capture, error) {
		return byClass[class], nil
	}
}

var lobbyCaptures = map[principal.Class]Capture{
	principal.ClassPublic:  {Tools: []string{"web_search", "handoff_to_owner"}, Text: "You help visitors."},
	principal.ClassTrusted: {Tools: []string{"read_file", "write_file", "handoff_to_owner"}, Text: "You help visitors."},
	principal.ClassOwner:   {Tools: []string{"read_file", "write_file", "web_search"}, Text: MarkerOwnerContext},
}

func TestPolicyCaseKindsPassAndFail(t *testing.T) {
	definition := lobbyDefinition()
	cases := map[string]struct {
		c    Case
		want bool
	}{
		"public tools match":           {Case{Kind: KindToolExposure, Class: principal.ClassPublic, Want: []string{"web_search", "handoff_to_owner"}}, true},
		"public tools differ":          {Case{Kind: KindToolExposure, Class: principal.ClassPublic, Want: []string{"web_search"}}, false},
		"trusted tools match":          {Case{Kind: KindToolExposure, Class: principal.ClassTrusted, Want: []string{"read_file", "write_file", "handoff_to_owner"}}, true},
		"owner never gets handoff":     {Case{Kind: KindToolExposure, Class: principal.ClassOwner, Want: []string{"read_file", "write_file", "web_search"}}, true},
		"public sees no owner data":    {Case{Kind: KindOwnerDataAbsent, Class: principal.ClassPublic}, true},
		"trusted effect goes to owner": {Case{Kind: KindApprovalRouted, Class: principal.ClassTrusted, Tool: "write_file"}, true},
		"public effect is tier denied": {Case{Kind: KindApprovalRouted, Class: principal.ClassPublic, Tool: "write_file"}, true},
		"trusted tool outside ceiling": {Case{Kind: KindApprovalRouted, Class: principal.ClassTrusted, Tool: "exec"}, false},
		"fixed reply override":         {Case{Kind: KindFixedReply, Key: auraagent.MessageBudgetExhausted, Text: "Kuota habis."}, true},
		"fixed reply default":          {Case{Kind: KindFixedReply, Key: auraagent.MessageRateLimited, Text: auraagent.PersonaMessageDefaults()[auraagent.MessageRateLimited]}, true},
		"fixed reply mismatch":         {Case{Kind: KindFixedReply, Key: auraagent.MessageBudgetExhausted, Text: "Quota gone."}, false},
	}
	catalog := evalTestCatalog()
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			tc.c.Name = name
			got, err := runCaseWithCatalog(t.Context(), definition, &tc.c, map[principal.Class]*Capture{}, fakeCapturer(lobbyCaptures), catalog)
			if err != nil || got != tc.want {
				t.Fatalf("runCase() = %v, %v; want %v", got, err, tc.want)
			}
		})
	}
	leaked := fakeCapturer(map[principal.Class]Capture{principal.ClassPublic: {Text: "context " + MarkerOwnerContext}})
	if got, err := runCaseWithCatalog(t.Context(), definition, &Case{Name: "leak", Kind: KindOwnerDataAbsent, Class: principal.ClassPublic}, map[principal.Class]*Capture{}, leaked, catalog); err != nil || got {
		t.Fatalf("leaked owner data passed: %v, %v", got, err)
	}
}

func TestRunAddsImplicitCasesAndTheAbuseCorpus(t *testing.T) {
	capturer := fakeCapturer(lobbyCaptures)
	catalog := evalTestCatalog()
	report, err := RunWithCatalog(t.Context(), lobbyDefinition(), []Case{{Name: "budget reply", Kind: KindFixedReply, Key: auraagent.MessageBudgetExhausted, Text: "Kuota habis."}}, capturer, catalog)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !report.Passed() || report.Total < 4 {
		t.Fatalf("report = %+v, want a passing run with implicit and abuse cases", report)
	}
	failing, err := RunWithCatalog(t.Context(), lobbyDefinition(), []Case{{Name: "wrong reply", Kind: KindFixedReply, Key: auraagent.MessageBudgetExhausted, Text: "nope"}}, capturer, catalog)
	if err != nil || failing.Passed() || len(failing.FailedCases) != 1 || failing.FailedCases[0] != "wrong reply" {
		t.Fatalf("failing report = %+v, %v", failing, err)
	}
	if _, err := RunWithCatalog(t.Context(), &auraagent.Definition{ID: "solo", Audience: auraagent.AudienceOwner}, nil, capturer, catalog); err == nil {
		t.Fatal("owner-only persona with no cases produced a result")
	}
}

func TestFixedReplyEvalUsesDeploymentBundle(t *testing.T) {
	registry, err := auraagent.Build([]auraagent.Persona{{
		ID:           "lobby",
		Description:  "Front desk",
		Instructions: "Help visitors.",
		Audience:     auraagent.AudiencePublic,
		Tools:        []string{"web_search"},
		PublicTools:  []string{"web_search"},
	}}, []string{"web_search"}, evalTestCatalog(), []string{"primary"}, "id")
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	definition, ok := registry.Lookup("lobby")
	if !ok {
		t.Fatal("lobby persona not found")
	}
	catalog := evalTestCatalog()
	idCase := Case{Name: "id reply", Kind: KindFixedReply, Key: auraagent.MessageRateLimited, Text: "Pesannya terlalu cepat. Tunggu sebentar, ya."}
	if got, err := runCaseWithCatalog(t.Context(), &definition, &idCase, map[principal.Class]*Capture{}, fakeCapturer(lobbyCaptures), catalog); err != nil || !got {
		t.Fatalf("id fixed reply = %v, %v; want true", got, err)
	}
	enCase := Case{Name: "en reply", Kind: KindFixedReply, Key: auraagent.MessageRateLimited, Text: auraagent.PersonaMessageDefaults()[auraagent.MessageRateLimited]}
	if got, err := runCaseWithCatalog(t.Context(), &definition, &enCase, map[principal.Class]*Capture{}, fakeCapturer(lobbyCaptures), catalog); err != nil || got {
		t.Fatalf("en fixed reply on id deployment = %v, %v; want false", got, err)
	}
}

func TestLoadCasesIsStrict(t *testing.T) {
	dir := t.TempDir()
	write := func(name, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("ok.yaml", "cases:\n  - name: tools\n    kind: tool_exposure\n    class: public\n    want: [web_search]\n")
	cases, err := LoadCases(dir)
	if err != nil || len(cases) != 1 || cases[0].Class != principal.ClassPublic {
		t.Fatalf("LoadCases = %+v, %v", cases, err)
	}
	for name, content := range map[string]string{
		"unknown-field.yaml": "cases:\n  - name: x\n    kind: fixed_reply\n    key: a\n    text: b\n    color: red\n",
		"unknown-kind.yaml":  "cases:\n  - name: x\n    kind: vibes\n",
		"no-tool.yaml":       "cases:\n  - name: x\n    kind: approval_routed\n    class: trusted\n",
		"owner-absent.yaml":  "cases:\n  - name: x\n    kind: owner_data_absent\n    class: owner\n",
	} {
		t.Run(name, func(t *testing.T) {
			bad := t.TempDir()
			if err := os.WriteFile(filepath.Join(bad, name), []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := LoadCases(bad); err == nil || !strings.Contains(err.Error(), string(ErrorCodeInvalid)) {
				t.Fatalf("LoadCases err = %v, want %s", err, ErrorCodeInvalid)
			}
		})
	}
	if cases, err := LoadCases(filepath.Join(dir, "missing")); err != nil || cases != nil {
		t.Fatalf("missing dir = %v, %v; want no cases", cases, err)
	}
}
