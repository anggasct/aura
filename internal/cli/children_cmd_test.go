package cli

import (
	"bytes"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/config"
	"github.com/anggasct/aura/internal/store"
)

func writeChildrenCLIConfig(t *testing.T) string {
	t.Helper()
	dataRoot := t.TempDir()
	path := dataRoot + "/config.yaml"
	content := `version: 1
storage:
  path: ` + dataRoot + `
tools:
  workspace: ` + dataRoot + `
skills:
  roots: [` + dataRoot + `]
context:
  recent_complete_turns: 5
profile:
  prompt_version: v1
children:
  recovery: interrupt
`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func writeChildrenDisabledConfig(t *testing.T) string {
	t.Helper()
	dataRoot := t.TempDir()
	path := dataRoot + "/config.yaml"
	content := `version: 1
storage:
  path: ` + dataRoot + `
tools:
  workspace: ` + dataRoot + `
skills:
  roots: [` + dataRoot + `]
context:
  recent_complete_turns: 5
profile:
  prompt_version: v1
children:
  enabled: false
  recovery: interrupt
`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func runChildrenCommand(t *testing.T, cfg string, args ...string) (string, error) {
	t.Helper()
	gf := &globalFlags{configPath: cfg}
	cmd := newChildrenCmd(gf)
	out := &bytes.Buffer{}
	cmd.SetOut(out)
	cmd.SetErr(out)
	cmd.SetArgs(args)
	err := cmd.ExecuteContext(t.Context())
	return out.String(), err
}

func seedChildRow(t *testing.T, cfg string) string {
	t.Helper()
	loaded, err := config.Load(cfg)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	db, err := openStorage(t.Context(), loaded.Config)
	if err != nil {
		t.Fatalf("openStorage: %v", err)
	}
	defer func() { _ = db.Close() }()
	now := time.Now().UTC()
	sessions := store.NewSessionService(db)
	for _, id := range []string{"sess-parent", "sess-child-1"} {
		if err := sessions.Create(t.Context(), &store.Session{ID: id, OwnerID: "owner-1", CreatedAt: now, UpdatedAt: now, Metadata: []byte(`{}`)}); err != nil {
			t.Fatalf("Create session %s: %v", id, err)
		}
	}
	children := store.NewChildStore(db)
	run := &store.ChildRun{
		ID: "ch-1", IdempotencyKey: "key-1",
		ParentSessionID: "sess-parent", ParentTurnID: "turn-1", ParentInvocation: "inv-1",
		ChildSessionID: "sess-child-1", DurableKey: "child/ch-1",
		ContextDigest: "digest-1", GrantsJSON: `[{"capability":"search"}]`,
		BudgetJSON: `{"max_tokens":1000}`, State: "queued",
		Deadline: now.Add(time.Hour), CreatedAt: now, UpdatedAt: now,
	}
	if err := children.InsertRun(t.Context(), run); err != nil {
		t.Fatalf("InsertRun: %v", err)
	}
	completed := now.Add(time.Minute)
	if err := children.SetResult(t.Context(), "ch-1", &store.ChildResult{
		Status: "completed", Output: "summary", ArtifactsJSON: `[]`,
		TokensUsed: 12, CostMicros: 34, CompletedAt: completed,
		Provenance: "child=ch-1 session=sess-child-1 digest=digest-1 durable=child/ch-1",
		ChildID:    "ch-1", SessionID: "sess-child-1", ContextDigest: "digest-1", DurableKey: "child/ch-1",
		SourceRange: "inv-1", Model: "child-default", PromptVersion: "v1", Trust: "derived_untrusted",
	}, completed); err != nil {
		t.Fatalf("SetResult: %v", err)
	}
	return "ch-1"
}

func TestChildrenCLIListEmpty(t *testing.T) {
	out, err := runChildrenCommand(t, writeChildrenCLIConfig(t), "list")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if strings.TrimSpace(out) != "no children" {
		t.Errorf("out = %q", out)
	}
}

func TestChildrenCLIListShowCancel(t *testing.T) {
	cfg := writeChildrenCLIConfig(t)
	id := seedChildRow(t, cfg)
	out, err := runChildrenCommand(t, cfg, "list")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if !strings.Contains(out, id) || !strings.Contains(out, "state=queued") {
		t.Errorf("list out = %q", out)
	}
	out, err = runChildrenCommand(t, cfg, "list", "--state", "running")
	if err != nil {
		t.Fatalf("list --state: %v", err)
	}
	if strings.TrimSpace(out) != "no children" {
		t.Errorf("filtered list out = %q", out)
	}
	if _, err := runChildrenCommand(t, cfg, "list", "--state", "bogus"); err == nil {
		t.Error("expected bogus state rejection")
	}
	out, err = runChildrenCommand(t, cfg, "show", id)
	if err != nil {
		t.Fatalf("show: %v", err)
	}
	for _, want := range []string{
		"id: " + id,
		"durable_key: child/ch-1",
		"parent_invocation: inv-1",
		"grants: ",
		"budget: ",
		"budget_usage: tokens=12 cost=34",
		"result_status: completed",
		"result_provenance: child=ch-1 session=sess-child-1 digest=digest-1 durable=child/ch-1",
		"result_model: child-default",
		"result_trust: derived_untrusted",
		"context_digest: digest-1",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("show out = %q, want %q", out, want)
		}
	}
	if !strings.Contains(out, `"capability":"search"`) {
		t.Errorf("show must carry attenuated grants, out = %q", out)
	}
	if !strings.Contains(out, "max_tokens") {
		t.Errorf("show must carry budget allocation, out = %q", out)
	}
	if !strings.Contains(out, "state: queued") {
		t.Errorf("show must carry terminal status, out = %q", out)
	}
	if strings.Contains(out, "summarize the logs") || strings.Contains(out, "tool_payload") {
		t.Errorf("show must never print task content or tool payloads, out = %q", out)
	}
	if _, err := runChildrenCommand(t, cfg, "show", "missing"); err == nil {
		t.Error("expected missing rejection")
	}
	out, err = runChildrenCommand(t, cfg, "cancel", id)
	if err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if !strings.Contains(out, "state=cancelled") {
		t.Errorf("cancel out = %q", out)
	}
	out, err = runChildrenCommand(t, cfg, "cancel", id)
	if err != nil {
		t.Fatalf("cancel terminal: %v", err)
	}
	if !strings.Contains(out, "state=cancelled") {
		t.Errorf("cancel terminal out = %q", out)
	}
	if _, err := runChildrenCommand(t, cfg, "cancel", "missing"); err == nil {
		t.Error("expected cancel missing rejection")
	}
}

func TestChildrenCLIRequiresEnabled(t *testing.T) {
	cfg := writeChildrenDisabledConfig(t)
	if _, err := runChildrenCommand(t, cfg, "list"); err == nil {
		t.Error("expected disabled rejection")
	}
}
