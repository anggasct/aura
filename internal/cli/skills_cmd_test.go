package cli

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/config"
)

func writeSkillsCLIConfig(t *testing.T, skillsSection string) string {
	t.Helper()
	dataRoot := t.TempDir()
	path := filepath.Join(t.TempDir(), "config.yaml")
	content := `version: 1
models:
  definitions:
    primary:
      protocol: openai_chat_compat
      model: test-model
      api_key_env: AURA_TEST_MODEL_KEY
      capabilities:
        streaming: true
        tools: true
        context_tokens: 200000
        tokenizer: anthropic
storage:
  path: ` + dataRoot + `
context:
  recent_complete_turns: 5
` + skillsSection
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

func skillsCLIConfig(t *testing.T) string {
	t.Helper()
	roots := t.TempDir()
	return writeSkillsCLIConfig(t, "skills:\n  enabled: true\n  roots: ["+roots+"]\n")
}

func runSkillsCommand(t *testing.T, cfg string, args ...string) (string, error) {
	t.Helper()
	gf := &globalFlags{configPath: cfg}
	cmd := newSkillsCmd(gf)
	out := &bytes.Buffer{}
	cmd.SetOut(out)
	cmd.SetErr(out)
	cmd.SetArgs(args)
	err := cmd.ExecuteContext(t.Context())
	return out.String(), err
}

func TestSkillsListEmpty(t *testing.T) {
	out, err := runSkillsCommand(t, skillsCLIConfig(t), "list")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if strings.TrimSpace(out) != "no skills" {
		t.Errorf("out = %q", out)
	}
}

func TestSkillsCreateReviewAcceptFlow(t *testing.T) {
	cfg := skillsCLIConfig(t)
	out, err := runSkillsCommand(t, cfg, "create", "--name", "flow", "--description", "Flow skill.")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	id := skillOutputValue(t, out, "id: ")
	digest := skillOutputValue(t, out, "digest: ")
	if id == "" || digest == "" {
		t.Fatalf("out = %q", out)
	}

	out, err = runSkillsCommand(t, cfg, "list")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if !strings.Contains(out, "quarantined") {
		t.Errorf("out = %q", out)
	}

	out, err = runSkillsCommand(t, cfg, "show", id)
	if err != nil {
		t.Fatalf("show: %v", err)
	}
	if !strings.Contains(out, "state: quarantined") {
		t.Errorf("out = %q", out)
	}
	if !strings.Contains(out, "description: Flow skill.") {
		t.Errorf("show description missing: %q", out)
	}
	if !strings.Contains(out, "compatibility: ") {
		t.Errorf("show compatibility missing: %q", out)
	}
	if !strings.Contains(out, "requested: ") {
		t.Errorf("show requested missing: %q", out)
	}
	if !strings.Contains(out, "granted: ") {
		t.Errorf("show granted missing: %q", out)
	}
	if !strings.Contains(out, "reviewed: ") {
		t.Errorf("show reviewed missing: %q", out)
	}

	out, err = runSkillsCommand(t, cfg, "review", id)
	if err != nil {
		t.Fatalf("review: %v", err)
	}
	if !strings.Contains(out, "description: Flow skill.") {
		t.Errorf("out = %q", out)
	}

	out, err = runSkillsCommand(t, cfg, "accept", id, "--digest", digest, "--grant", "shell.execute")
	if err != nil {
		t.Fatalf("accept: %v", err)
	}
	if strings.TrimSpace(out) != "accepted: "+id {
		t.Errorf("out = %q", out)
	}

	out, err = runSkillsCommand(t, cfg, "list", "--state", "active")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if !strings.Contains(out, id) {
		t.Errorf("out = %q", out)
	}

	out, err = runSkillsCommand(t, cfg, "disable", id)
	if err != nil {
		t.Fatalf("disable: %v", err)
	}
	if strings.TrimSpace(out) != "disabled: "+id {
		t.Errorf("out = %q", out)
	}

	out, err = runSkillsCommand(t, cfg, "doctor")
	if err != nil {
		t.Fatalf("doctor: %v", err)
	}
	if !strings.Contains(out, "disabled") {
		t.Errorf("out = %q", out)
	}
}

func TestSkillsRejectFlow(t *testing.T) {
	cfg := skillsCLIConfig(t)
	out, err := runSkillsCommand(t, cfg, "create", "--name", "nope", "--description", "Bad skill.")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	id := skillOutputValue(t, out, "id: ")
	out, err = runSkillsCommand(t, cfg, "reject", id, "--reason", "too broad")
	if err != nil {
		t.Fatalf("reject: %v", err)
	}
	if strings.TrimSpace(out) != "rejected: "+id {
		t.Errorf("out = %q", out)
	}
	if _, err := runSkillsCommand(t, cfg, "review", id); err == nil {
		t.Errorf("review of rejected should fail")
	}
}

func TestSkillsRetentionWiringThroughCLIHelper(t *testing.T) {
	root := t.TempDir()
	cfgPath := writeSkillsCLIConfig(t, "skills:\n  enabled: true\n  roots: ["+root+"]\n  quarantine_retention: 1h\n")
	result, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if result.Config.Skills == nil {
		t.Fatalf("skills config is nil")
	}
	db, err := openStorage(t.Context(), result.Config)
	if err != nil {
		t.Fatalf("storage: %v", err)
	}
	defer func() { _ = db.Close() }()
	engine, err := buildSkillsEngine(t.Context(), result.Config.Skills, db, nil)
	if err != nil {
		t.Fatalf("engine: %v", err)
	}
	stale, err := engine.StageDraft(t.Context(), "stale-one", "Stale draft.", root)
	if err != nil {
		t.Fatalf("stage stale: %v", err)
	}
	fresh, err := engine.StageDraft(t.Context(), "fresh-one", "Fresh draft.", root)
	if err != nil {
		t.Fatalf("stage fresh: %v", err)
	}
	if _, err := engine.Reject(t.Context(), stale.ID, "stale"); err != nil {
		t.Fatalf("reject stale: %v", err)
	}
	if _, err := engine.Reject(t.Context(), fresh.ID, "fresh"); err != nil {
		t.Fatalf("reject fresh: %v", err)
	}
	past := time.Now().UTC().Add(-2 * time.Hour).Format(time.RFC3339Nano)
	if _, err := db.ExecContext(t.Context(), `UPDATE skill_package SET reviewed_at = ?, updated_at = ? WHERE id = ?`, past, past, stale.ID); err != nil {
		t.Fatalf("age stale: %v", err)
	}
	if err := engine.Refresh(t.Context()); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "pending", "stale-one")); !os.IsNotExist(err) {
		t.Errorf("expired rejection should collect bytes, stat err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "pending", "fresh-one")); err != nil {
		t.Errorf("fresh rejection should retain bytes: %v", err)
	}
}

func TestSkillsValidateCommand(t *testing.T) {
	cfg := skillsCLIConfig(t)
	dir := t.TempDir()
	document := "---\nname: ok\ndescription: Fine.\n---\nBody.\n"
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(document), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	out, err := runSkillsCommand(t, cfg, "validate", dir)
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	if !strings.HasPrefix(strings.TrimSpace(out), "valid: ") {
		t.Errorf("out = %q", out)
	}
	bad := t.TempDir()
	if err := os.WriteFile(filepath.Join(bad, "SKILL.md"), []byte("---\ndescription: no name\n---\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	out, err = runSkillsCommand(t, cfg, "validate", bad)
	if err == nil {
		t.Fatalf("invalid should fail, out = %q", out)
	}
	if !strings.Contains(out, "name_missing") {
		t.Errorf("out = %q", out)
	}
}

func TestSkillsCommandUsage(t *testing.T) {
	cfg := skillsCLIConfig(t)
	if _, err := runSkillsCommand(t, cfg, "create", "--name", "x"); err == nil {
		t.Errorf("missing description should fail")
	} else {
		var ue *usageError
		if !errors.As(err, &ue) {
			t.Errorf("err = %v, want usage error", err)
		}
	}
	if _, err := runSkillsCommand(t, cfg, "list", "--state", "flying"); err == nil {
		t.Errorf("bad state should fail")
	}
	if _, err := runSkillsCommand(t, cfg, "show", "local/missing@0123456789ab"); err == nil {
		t.Errorf("missing id should fail")
	}
	disabled := writeSkillsCLIConfig(t, "skills:\n  enabled: false\n  roots: [/tmp/aura-skills]\n")
	if _, err := runSkillsCommand(t, disabled, "list"); err == nil {
		t.Errorf("disabled engine should fail")
	}
}

func skillOutputValue(t *testing.T, out, prefix string) string {
	t.Helper()
	for line := range strings.Lines(out) {
		if value, ok := strings.CutPrefix(strings.TrimSpace(line), prefix); ok {
			return value
		}
	}
	return ""
}
