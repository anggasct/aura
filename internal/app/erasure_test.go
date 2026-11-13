package app

import (
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/config"
	"github.com/anggasct/aura/internal/platform/principal"
	"github.com/anggasct/aura/internal/store"
)

func seedIdleSession(t *testing.T, sessions store.SessionService, id, owner string, at time.Time) {
	t.Helper()
	if err := sessions.Create(t.Context(), &store.Session{ID: id, OwnerID: owner, CreatedAt: at, UpdatedAt: at, Metadata: []byte(`{}`)}); err != nil {
		t.Fatalf("Create session %s: %v", id, err)
	}
}

func sessionExists(t *testing.T, db *sql.DB, id string) bool {
	t.Helper()
	var n int
	if err := db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM session WHERE id = ?`, id).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n == 1
}

func TestPrincipalForgetDryRunThenApply(t *testing.T) {
	cfg := writePrincipalCLIConfig(t)
	db := openAccessDB(t, cfg)
	now := time.Now().UTC()
	sessions := store.NewSessionService(db)
	seedIdleSession(t, sessions, "main/discord:office/dm:9", "discord:9", now)
	seedIdleSession(t, sessions, "main/discord:office/dm:777", principal.OwnerID, now)
	if _, err := newPairingService(db, nil).Request(t.Context(), "discord:9", "office"); err != nil {
		t.Fatalf("Request: %v", err)
	}
	out, err := runAccessCommand(t, newPrincipalCmd, cfg, "forget", "discord:9")
	if err != nil || !strings.Contains(out, "sessions: 1") || !strings.Contains(out, "pairing_requests: 1") || !strings.Contains(out, "dry run") {
		t.Fatalf("dry run = %q, %v", out, err)
	}
	if exists := sessionExists(t, db, "main/discord:office/dm:9"); !exists {
		t.Fatal("dry run erased the session")
	}
	out, err = runAccessCommand(t, newPrincipalCmd, cfg, "forget", "discord:9", "--apply")
	if err != nil || !strings.Contains(out, "erased discord:9") {
		t.Fatalf("apply = %q, %v", out, err)
	}
	if exists := sessionExists(t, db, "main/discord:office/dm:9"); exists {
		t.Fatal("apply kept the session")
	}
	if exists := sessionExists(t, db, "main/discord:office/dm:777"); !exists {
		t.Fatal("apply touched the owner session")
	}
	if accessEventCount(t, cfg, accessActionPrincipalForget) != 1 {
		t.Fatal("forget was not recorded as an owner action")
	}
	for _, id := range []string{"discord:777", "local-owner", "nobody"} {
		if _, err := runAccessCommand(t, newPrincipalCmd, cfg, "forget", id, "--apply"); err == nil {
			t.Fatalf("forget %q accepted", id)
		}
	}
}

func TestRetentionSweepErasesIdlePrincipalsByClass(t *testing.T) {
	cfg := writePrincipalCLIConfig(t)
	db := openAccessDB(t, cfg)
	loaded, err := config.Load(cfg)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)
	sessions := store.NewSessionService(db)
	seedIdleSession(t, sessions, "main/discord:office/dm:21", "discord:21", now.Add(-721*time.Hour))
	seedIdleSession(t, sessions, "main/discord:office/dm:22", "discord:22", now.Add(-719*time.Hour))
	seedIdleSession(t, sessions, "main/discord:office/dm:5", "discord:5", now.Add(-721*time.Hour))
	seedIdleSession(t, sessions, "main/discord:office/dm:23", "discord:23", now.Add(-4321*time.Hour))
	seedIdleSession(t, sessions, "main/discord:office/dm:777", principal.OwnerID, now.Add(-9000*time.Hour))
	if _, err := db.ExecContext(t.Context(), `INSERT INTO principal_grant (principal_id, account, class, granted_at, granted_by) VALUES ('discord:23', 'office', 'trusted', ?, 'local-owner')`, now.Add(-5000*time.Hour).Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	swept, err := sweepRetention(t.Context(), loaded.Config, db, "", now)
	if err != nil {
		t.Fatalf("sweepRetention: %v", err)
	}
	if swept[principal.ClassPublic] != 1 || swept[principal.ClassTrusted] != 1 {
		t.Fatalf("swept = %v, want one public and one trusted", swept)
	}
	for id, want := range map[string]bool{
		"main/discord:office/dm:21":  false,
		"main/discord:office/dm:22":  true,
		"main/discord:office/dm:5":   true,
		"main/discord:office/dm:23":  false,
		"main/discord:office/dm:777": true,
	} {
		if exists := sessionExists(t, db, id); exists != want {
			t.Errorf("session %s exists = %v, want %v", id, exists, want)
		}
	}
	if granted, _ := store.NewPrincipalGrantStore(db).Active(t.Context(), "discord:23", "office"); !granted {
		t.Fatal("retention removed the pairing grant")
	}
	again, err := sweepRetention(t.Context(), loaded.Config, db, "", now)
	if err != nil || again[principal.ClassPublic]+again[principal.ClassTrusted] != 0 {
		t.Fatalf("second sweep = %v, %v; want idempotent", again, err)
	}
}

func TestErasureLinesPrintRunSessionsAfterSessions(t *testing.T) {
	lines := erasureLines(&store.ErasureCounts{Sessions: 2, RunSessions: 5})
	if len(lines) < 2 || lines[0] != "sessions: 2" || lines[1] != "run_sessions: 5" {
		t.Fatalf("erasure lines = %v, want sessions then run_sessions", lines)
	}
}
