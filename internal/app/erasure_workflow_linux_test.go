//go:build linux

package app

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/modules/workflow"
	"github.com/anggasct/aura/internal/store"
)

type recordingCanceller struct {
	mu  sync.Mutex
	ids []string
}

func (c *recordingCanceller) Cancel(_ context.Context, runID string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ids = append(c.ids, runID)
	return nil
}

func (c *recordingCanceller) cancelled() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.ids)
}

func TestForgetStopsLiveRunsBeforeErasing(t *testing.T) {
	t.Run("a server hosting the run stops it before the erasure", func(t *testing.T) {
		restoreDefaultLogger(t)
		t.Setenv("AURA_TEST_MODEL_KEY", "test-key")
		definitions := t.TempDir()
		if err := os.WriteFile(filepath.Join(definitions, "demo.yaml"), []byte(validWorkflowYAML), 0o600); err != nil {
			t.Fatalf("write definition: %v", err)
		}
		path := writeCompositionConfig(t, freeLoopbackAddress(t), compositionModelsYAML, fmt.Sprintf("workflows:\n  definitions_dir: %s\n", definitions))
		srv := buildCompositionServer(t, path)
		srv.run(t)
		command := buildApp(t, commandCompositionOptions(loadCompositionConfig(t, path), &syncBuffer{}, PartWorkflows))
		t.Cleanup(func() { _ = command.Close(t.Context()) })
		summary := startRunAs(t, command, forgottenPrincipal)
		db := compositionDatabase(t, path)
		awaitSchedule(t, "the server has claimed the run", func() bool {
			return countRows(t, db, `SELECT COUNT(*) FROM durable_run WHERE key = ? AND state = 'running' AND owner IS NOT NULL`, summary.DurableKey) == 1
		})

		out, err := runAccessCommand(t, newPrincipalCmd, path, "forget", forgottenPrincipal, "--apply")
		if err != nil || !strings.Contains(out, "cancelled_runs: "+summary.ID) || !strings.Contains(out, "erased "+forgottenPrincipal) {
			t.Fatalf("forget = %q, %v; want the root cancelled and the principal erased", out, err)
		}
		if n := countRows(t, db, `SELECT COUNT(*) FROM workflow_run WHERE id = ?`, summary.ID); n != 0 {
			t.Fatal("the run survived the erasure")
		}
		if n := countRows(t, db, `SELECT COUNT(*) FROM durable_run WHERE key = ?`, summary.DurableKey); n != 0 {
			t.Fatal("the durable run survived the erasure")
		}
	})

	t.Run("with no server it cancels and erases at once", func(t *testing.T) {
		gf := writeWorkflowFixtures(t, validWorkflowYAML)
		command := openCommandWorkflows(t, gf)
		summary := startRunAs(t, command, forgottenPrincipal)
		started := time.Now()

		out, err := runAccessCommand(t, newPrincipalCmd, gf.configPath, "forget", forgottenPrincipal, "--apply")
		if err != nil || !strings.Contains(out, "cancelled_runs: "+summary.ID) || !strings.Contains(out, "erased "+forgottenPrincipal) {
			t.Fatalf("forget = %q, %v; want the root cancelled and the principal erased", out, err)
		}
		if time.Since(started) > erasureRunBound/2 {
			t.Fatal("forget waited for a run that had no host")
		}
		if n := countRows(t, command.db, `SELECT COUNT(*) FROM workflow_run WHERE principal_id = ?`, forgottenPrincipal); n != 0 {
			t.Fatal("the run survived the erasure")
		}
	})

	t.Run("a run that does not stop within the bound refuses and keeps everything", func(t *testing.T) {
		gf := writeWorkflowFixtures(t, validWorkflowYAML)
		db := openAccessDB(t, gf.configPath)
		seedLiveRunRows(t, db, "run-stuck", "stuck", forgottenPrincipal)
		seedIdleSession(t, store.NewSessionService(db), "main/discord:office/dm:601", forgottenPrincipal, time.Now())
		canceller := &recordingCanceller{}
		runs := &erasureRuns{poll: time.Millisecond, bound: 50 * time.Millisecond}
		runs.set(canceller)

		cancelled, _, err := forgetPrincipal(t.Context(), db, runs, forgottenPrincipal, true, store.ErasureOptions{})
		if code, ok := store.CodeOf(err); !ok || code != store.ErrorCodeErasureBlocked || !strings.Contains(err.Error(), "run-stuck") {
			t.Fatalf("forget = %v, want %s naming the run", err, store.ErrorCodeErasureBlocked)
		}
		if len(cancelled) != 1 || len(canceller.cancelled()) != 1 || canceller.cancelled()[0] != "run-stuck" {
			t.Fatalf("cancelled = %v, calls = %v; want the stuck root cancelled once", cancelled, canceller.cancelled())
		}
		if n := countRows(t, db, `SELECT COUNT(*) FROM workflow_run WHERE id = 'run-stuck' AND status = ?`, workflow.RunRunning); n != 1 {
			t.Fatal("the forget path wrote the run's status")
		}
		if !sessionExists(t, db, "main/discord:office/dm:601") {
			t.Fatal("a refused forget deleted a session")
		}
	})
}
