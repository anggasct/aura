package cli

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	auraagent "github.com/anggasct/aura/internal/agent"
	"github.com/anggasct/aura/internal/channel/terminal"
	"github.com/anggasct/aura/internal/config"
	"github.com/anggasct/aura/internal/runtime"
	"github.com/anggasct/aura/internal/store"
)

func writePersonaCLIConfig(t *testing.T, soul string) string {
	t.Helper()
	path := writeAgentsConfig(t, `storage:
  path: `+t.TempDir()+`
agents:
  definitions:
    - id: kuma
      description: Casual assistant
      soul_file: personas/kuma.md
      tools: [read_file]
`)
	if err := os.MkdirAll(filepath.Join(filepath.Dir(path), "personas"), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(filepath.Dir(path), "personas", "kuma.md"), []byte(soul), 0o600); err != nil {
		t.Fatalf("write soul: %v", err)
	}
	return path
}

func personaRegistry(t *testing.T, configPath string) *auraagent.Registry {
	t.Helper()
	result, err := config.Load(configPath)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	registry, err := buildAgentRegistry(result.Config)
	if err != nil {
		t.Fatalf("buildAgentRegistry: %v", err)
	}
	return registry
}

func TestPersonaListShowsSoulSourceAndDigest(t *testing.T) {
	gf := &globalFlags{configPath: writePersonaCLIConfig(t, "You are Kuma.")}
	cmd := newPersonaCmd(gf)
	out := &bytes.Buffer{}
	cmd.SetOut(out)
	cmd.SetErr(out)
	cmd.SetArgs([]string{"list"})
	if err := cmd.ExecuteContext(t.Context()); err != nil {
		t.Fatalf("persona list: %v", err)
	}
	digest := auraagent.Definition{Instructions: "You are Kuma."}
	want := regexp.MustCompile(`(?m)^kuma\s+personas/kuma\.md\s+` + digest.Fingerprint().SoulSHA256[:soulDigestDisplayLength] + `\s+runtime default\s*$`)
	if !want.MatchString(out.String()) {
		t.Fatalf("persona list output lacks the kuma soul row:\n%s", out)
	}
	for _, row := range []string{`(?m)^main\s+\(none\)`, `(?m)^engineer\s+inline`} {
		if !regexp.MustCompile(row).MatchString(out.String()) {
			t.Errorf("persona list output lacks %s:\n%s", row, out)
		}
	}
}

func TestPersonaFingerprintFollowsSoulFile(t *testing.T) {
	path := writePersonaCLIConfig(t, "You are Kuma.")
	before, ok := registryPersonas{registry: personaRegistry(t, path)}.Fingerprint("kuma")
	if !ok {
		t.Fatal("kuma has no fingerprint")
	}
	if err := os.WriteFile(filepath.Join(filepath.Dir(path), "personas", "kuma.md"), []byte("You are Kuma, now formal."), 0o600); err != nil {
		t.Fatal(err)
	}
	after, _ := registryPersonas{registry: personaRegistry(t, path)}.Fingerprint("kuma")
	if after.SoulSHA256 == before.SoulSHA256 || after.ToolsSHA256 != before.ToolsSHA256 || after.AgentID != "kuma" {
		t.Fatalf("fingerprint before %+v after %+v, want only the soul digest to change", before, after)
	}
	if _, ok := (registryPersonas{registry: personaRegistry(t, path)}).Fingerprint("ghost"); ok {
		t.Fatal("unknown persona has a fingerprint")
	}
}

func TestResolveChatPersona(t *testing.T) {
	registry := personaRegistry(t, writePersonaCLIConfig(t, "You are Kuma."))
	db := discordTestDB(t)
	sessions := store.NewSessionService(db)
	now := time.Now().UTC()
	if err := sessions.Create(t.Context(), &store.Session{ID: "term-kuma", OwnerID: "local-owner", AgentID: "kuma", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name, session, requested, want string
		code                           runtime.ErrorCode
	}{
		{name: "default", want: auraagent.DefaultID},
		{name: "requested", requested: "engineer", want: "engineer"},
		{name: "unknown", requested: "ghost", code: runtime.ErrorCodePersonaNotFound},
		{name: "resume keeps session persona", session: "term-kuma", want: "kuma"},
		{name: "resume with same persona", session: "term-kuma", requested: "kuma", want: "kuma"},
		{name: "resume with other persona", session: "term-kuma", requested: "engineer", code: runtime.ErrorCodePersonaMismatch},
		{name: "unknown session falls back", session: "term-new", requested: "engineer", want: "engineer"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolveChatPersona(t.Context(), registry, sessions, tc.session, tc.requested)
			if tc.code != "" {
				if code, ok := runtime.CodeOf(err); !ok || code != tc.code {
					t.Fatalf("error = %v, want %s", err, tc.code)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("persona = %q, %v; want %q", got, err, tc.want)
			}
		})
	}
}

func TestTerminalRunsAndRecordsChosenPersona(t *testing.T) {
	engine := &captureEngine{}
	runner := &terminalRunner{engine: engine, agentID: "engineer"}
	for range runner.Run(t.Context(), &terminal.Request{SessionID: "sess-1", PrincipalID: "local-owner", Parts: []terminal.Input{{Text: "hi"}}}) {
	}
	if engine.got == nil || engine.got.AgentID != "engineer" {
		t.Fatalf("terminal turn = %+v, want persona engineer", engine.got)
	}

	db := discordTestDB(t)
	svc := store.NewSessionService(db)
	sessions := &terminalSessions{sessions: svc, agentID: "engineer"}
	created, err := sessions.Create(t.Context(), "local-owner")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	stored, err := svc.Get(t.Context(), created.ID)
	if err != nil || stored.AgentID != "engineer" {
		t.Fatalf("stored session persona = %q, %v; want engineer", stored.AgentID, err)
	}
}

func TestChatRejectsUnknownPersona(t *testing.T) {
	t.Setenv("AURA_TEST_MODEL_KEY", "test-key")
	path := writePersonaCLIConfig(t, "You are Kuma.")
	result, err := config.Load(path)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	err = runChat(t.Context(), result.Config, path, slog.New(slog.DiscardHandler), strings.NewReader(""), &bytes.Buffer{}, &bytes.Buffer{}, "", "ghost", chatPresentation{plain: true})
	if code, ok := runtime.CodeOf(err); !ok || code != runtime.ErrorCodePersonaNotFound {
		t.Fatalf("chat --persona ghost error = %v, want persona_not_found", err)
	}
}
