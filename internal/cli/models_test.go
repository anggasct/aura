package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/config"
	"github.com/anggasct/aura/internal/model"
	"github.com/anggasct/aura/internal/store"
)

func writeModelsConfig(t *testing.T, extraRoutes string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	dbPath := filepath.Join(dir, "aura.db")
	base := `version: 1
storage:
  path: ` + dbPath + `
models:
  definitions:
    cand1:
      protocol: openai_chat_compat
      model: test-cand1
      api_key_env: AURA_TEST_MODEL_KEY
      capabilities:
        context_tokens: 128000
        tokenizer: cl100k_base
    cand2:
      protocol: openai_chat_compat
      model: test-cand2
      api_key_env: AURA_TEST_MODEL_KEY
      capabilities:
        context_tokens: 128000
        tokenizer: cl100k_base
`
	if extraRoutes != "" {
		base += extraRoutes
	}
	if err := os.WriteFile(path, []byte(base), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

func runModelsCommand(t *testing.T, gf *globalFlags, args ...string) (string, error) {
	t.Helper()
	cmd := newModelsCmd(gf)
	out := &bytes.Buffer{}
	cmd.SetOut(out)
	cmd.SetErr(out)
	cmd.SetArgs(args)
	err := cmd.ExecuteContext(t.Context())
	return out.String(), err
}

func TestModelsRoutesCmd(t *testing.T) {
	routesConfig := `model_routes:
  primary:
    candidates: [cand1, cand2]
    max_provider_attempts: 4
    retry_delay_budget: 15s
    cost_budget_usd: 1.50
`
	gf := &globalFlags{configPath: writeModelsConfig(t, routesConfig)}
	out, err := runModelsCommand(t, gf, "routes")
	if err != nil {
		t.Fatalf("models routes: %v", err)
	}

	headers := []string{"ROUTE", "CANDIDATES", "MAX ATTEMPTS", "DELAY BUDGET", "COST BUDGET"}
	for _, h := range headers {
		if !strings.Contains(out, h) {
			t.Errorf("output lacks header %q:\n%s", h, out)
		}
	}
	if !strings.Contains(out, "primary") || !strings.Contains(out, "cand1, cand2") || !strings.Contains(out, "$1.50") {
		t.Errorf("output lacks expected route row:\n%s", out)
	}
}

func TestModelsCircuitsAndResetCmd(t *testing.T) {
	gf := &globalFlags{configPath: writeModelsConfig(t, "")}
	ctx := context.Background()

	// Initial circuits query
	out, err := runModelsCommand(t, gf, "circuits")
	if err != nil {
		t.Fatalf("models circuits: %v", err)
	}
	if !strings.Contains(out, "CIRCUIT KEY") || !strings.Contains(out, "cand1") {
		t.Fatalf("initial circuits output missing expected content:\n%s", out)
	}

	// Now insert an open circuit checkpoint directly into store
	loadRes, err := config.Load(gf.configPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	db, err := openStorage(ctx, loadRes.Config)
	if err != nil {
		t.Fatalf("open storage: %v", err)
	}
	checkpointStore := store.NewCircuitCheckpointStore(db)
	openUntil := time.Now().Add(5 * time.Minute)
	candDef := loadRes.Config.Models.Definitions["cand1"]
	err = checkpointStore.Save(ctx, &store.CircuitCheckpoint{
		CircuitKey:          "cand1",
		ConfigDigest:        model.ComputeConfigDigest(&candDef),
		State:               string(model.CircuitStateOpen),
		ConsecutiveFailures: 3,
		OpenUntil:           &openUntil,
		UpdatedAt:           time.Now(),
	})
	if err != nil {
		t.Fatalf("save checkpoint: %v", err)
	}
	_ = db.Close()

	// Run circuits command and verify open state is listed
	out, err = runModelsCommand(t, gf, "circuits")
	if err != nil {
		t.Fatalf("models circuits: %v", err)
	}
	if !strings.Contains(out, "open") {
		t.Errorf("circuits output lacks open state:\n%s", out)
	}

	// Reset the circuit for cand1
	resetOut, err := runModelsCommand(t, gf, "circuit-reset", "cand1")
	if err != nil {
		t.Fatalf("models circuit-reset cand1: %v", err)
	}
	if !strings.Contains(resetOut, "circuit reset for cand1") {
		t.Errorf("circuit-reset output lacks success message:\n%s", resetOut)
	}

	// Reset nonexistent circuit fails
	_, err = runModelsCommand(t, gf, "circuit-reset", "nonexistent-model")
	if err == nil {
		t.Fatal("expected circuit-reset nonexistent-model to fail")
	}
}
