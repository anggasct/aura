package app

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/channels/kit/access"
	"github.com/anggasct/aura/internal/config"
	"github.com/anggasct/aura/internal/modules/workflow"
	"github.com/anggasct/aura/internal/platform/principal"
	"github.com/anggasct/aura/internal/store"
)

const forgottenPrincipal = "discord:601"

func startRunAs(t *testing.T, app *App, principalID string) *workflow.RunSummary {
	t.Helper()
	summary, err := app.interpreter.Start(t.Context(), "demo", nil, &workflow.RunOrigin{
		PrincipalID: principalID, PrincipalClass: string(principal.ClassTrusted), AgentID: "main",
	})
	if err != nil {
		t.Fatalf("Start as %s: %v", principalID, err)
	}
	return summary
}

func openCommandWorkflows(t *testing.T, gf *globalFlags) *App {
	t.Helper()
	restoreDefaultLogger(t)
	app := buildApp(t, commandCompositionOptions(loadCompositionConfig(t, gf.configPath), &syncBuffer{}, PartWorkflows))
	t.Cleanup(func() { _ = app.Close(context.Background()) })
	return app
}

func seedLiveRunRows(t *testing.T, db *sql.DB, runID, definitionID, principalID string) {
	t.Helper()
	const stamp = "2026-11-12T09:00:00Z"
	for _, statement := range []string{
		`INSERT INTO workflow_definition (id, version, goal, source, spec_json, spec_sha256, created_at) VALUES ('` + definitionID + `', 1, 'goal', 'composed', '{}', 'digest', '` + stamp + `')`,
		`INSERT INTO durable_run (key, handler, payload, state, created_at, updated_at) VALUES ('dur-` + runID + `', 'workflow', x'7b7d', 'running', '` + stamp + `', '` + stamp + `')`,
		`INSERT INTO workflow_run (id, definition_id, definition_version, durable_key, goal, input_json, status, created_at, updated_at, principal_id, principal_class, agent_id)
			VALUES ('` + runID + `', '` + definitionID + `', 1, 'dur-` + runID + `', 'goal', '{}', 'running', '` + stamp + `', '` + stamp + `', '` + principalID + `', 'trusted', 'main')`,
	} {
		if _, err := db.ExecContext(t.Context(), statement); err != nil {
			t.Fatalf("seed live run %s: %v", runID, err)
		}
	}
}

func TestForgetMeCancelsThroughTheLateBoundInterpreter(t *testing.T) {
	gf := writeWorkflowFixtures(t, validWorkflowYAML)
	app := openCommandWorkflows(t, gf)
	summary := startRunAs(t, app, forgottenPrincipal)
	runs := &erasureRuns{poll: time.Millisecond, bound: 50 * time.Millisecond}
	eraser := &principalEraser{db: app.db, now: time.Now, runs: runs}

	err := eraser.Forget(t.Context(), forgottenPrincipal)
	if !errors.Is(err, access.ErrForgetRunsStopping) {
		t.Fatalf("Forget with no interpreter = %v, want the runs-stopping refusal", err)
	}
	if code, ok := store.CodeOf(err); !ok || code != store.ErrorCodeErasureBlocked {
		t.Fatalf("Forget with no interpreter = %v, want %s", err, store.ErrorCodeErasureBlocked)
	}
	if n := countRows(t, app.db, `SELECT COUNT(*) FROM workflow_run WHERE id = ?`, summary.ID); n != 1 {
		t.Fatal("a refused forget deleted the run")
	}

	runs.set(app.interpreter)
	if err := eraser.Forget(t.Context(), forgottenPrincipal); err != nil {
		t.Fatalf("Forget with the interpreter set = %v", err)
	}
	if n := countRows(t, app.db, `SELECT COUNT(*) FROM workflow_run WHERE principal_id = ?`, forgottenPrincipal); n != 0 {
		t.Fatalf("workflow runs left after forget = %d, want 0", n)
	}
	if n := countRows(t, app.db, `SELECT COUNT(*) FROM durable_run WHERE key = ?`, summary.DurableKey); n != 0 {
		t.Fatal("the durable run survived forget")
	}
}

func TestRetentionSweepSkipsAPrincipalWithLiveRuns(t *testing.T) {
	cfg := writePrincipalCLIConfig(t)
	db := openAccessDB(t, cfg)
	loaded, err := config.Load(cfg)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)
	sessions := store.NewSessionService(db)
	seedIdleSession(t, sessions, "main/discord:office/dm:21", "discord:21", now.Add(-721*time.Hour))
	seedIdleSession(t, sessions, "main/discord:office/dm:24", "discord:24", now.Add(-721*time.Hour))
	seedLiveRunRows(t, db, "run-live", "busy", "discord:21")

	swept, err := sweepRetention(t.Context(), loaded.Config, db, "", now)
	if err != nil {
		t.Fatalf("sweepRetention: %v", err)
	}
	if swept[principal.ClassPublic] != 1 {
		t.Fatalf("swept = %v, want only the principal without a live run", swept)
	}
	if !sessionExists(t, db, "main/discord:office/dm:21") || sessionExists(t, db, "main/discord:office/dm:24") {
		t.Fatal("the sweep kept the wrong principal")
	}
	if n := countRows(t, db, `SELECT COUNT(*) FROM workflow_run WHERE id = 'run-live'`); n != 1 {
		t.Fatal("the sweep deleted a live run")
	}
}
