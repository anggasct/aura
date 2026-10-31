package store

import (
	"path/filepath"
	"testing"
)

func TestConversationSessionPointerLifecycle(t *testing.T) {
	db := newTestDB(t)
	ctx := t.Context()
	s := NewConversationSessionStore(db)
	key := "main/discord:owner/dm:111"
	seedPersonaSession(t, db, key, "local-owner", "main")
	seedPersonaSession(t, db, key+"~aaaa", "local-owner", "main")
	seedPersonaSession(t, db, key+"~bbbb", "local-owner", "main")

	if id, found, err := s.Resolve(ctx, key); err != nil || found || id != "" {
		t.Fatalf("Resolve before pointer = %q %v %v", id, found, err)
	}
	for _, target := range []string{key + "~aaaa", key + "~bbbb"} {
		if err := s.Point(ctx, key, target); err != nil {
			t.Fatalf("Point(%s): %v", target, err)
		}
		if id, found, err := s.Resolve(ctx, key); err != nil || !found || id != target {
			t.Fatalf("Resolve = %q %v %v, want %s", id, found, err, target)
		}
	}
	var rows int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM conversation_session`).Scan(&rows); err != nil || rows != 1 {
		t.Fatalf("pointer rows = %d %v, want one upserted row", rows, err)
	}
	if err := s.Clear(ctx, key); err != nil {
		t.Fatalf("Clear: %v", err)
	}
	if _, found, _ := s.Resolve(ctx, key); found {
		t.Fatal("pointer survived Clear")
	}
	if err := s.Clear(ctx, key); err != nil {
		t.Fatalf("Clear without pointer: %v", err)
	}
}

func TestConversationSessionPointerCascadesAndRejectsUnknownSessions(t *testing.T) {
	db := newTestDB(t)
	ctx := t.Context()
	s := NewConversationSessionStore(db)
	key := "main/discord:owner/thread:777"
	seedPersonaSession(t, db, key, "local-owner", "main")
	seedPersonaSession(t, db, key+"~cccc", "local-owner", "main")
	if err := s.Point(ctx, key, key+"~missing"); err == nil {
		t.Fatal("pointer to a missing session accepted")
	} else if code, ok := CodeOf(err); !ok || code != ErrorCodeSessionNotFound {
		t.Fatalf("Point(missing) error = %v, want session_not_found", err)
	}
	if err := s.Point(ctx, key, key+"~cccc"); err != nil {
		t.Fatalf("Point: %v", err)
	}
	if _, err := db.ExecContext(ctx, `DELETE FROM session WHERE id = ?`, key+"~cccc"); err != nil {
		t.Fatalf("delete pointed session: %v", err)
	}
	if id, found, err := s.Resolve(ctx, key); err != nil || found {
		t.Fatalf("Resolve after cascade = %q %v %v, want no pointer", id, found, err)
	}
	for _, call := range []func() error{
		func() error { _, _, err := s.Resolve(ctx, ""); return err },
		func() error { return s.Point(ctx, "", key) },
		func() error { return s.Point(ctx, key, "") },
		func() error { return s.Clear(ctx, "") },
	} {
		if err := call(); err == nil {
			t.Error("empty argument accepted")
		}
	}
}

func TestConversationSessionMigrationAppliesOnV21(t *testing.T) {
	ctx := t.Context()
	db, err := OpenDB(ctx, filepath.Join(t.TempDir(), "aura.db"))
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.ExecContext(ctx, bootstrapSchemaMigrationTableSQL); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	for _, m := range migrations[:21] {
		if err := applyMigration(ctx, db, m); err != nil {
			t.Fatalf("apply v%d: %v", m.version, err)
		}
	}
	seedPersonaSession(t, db, "main/discord:owner/dm:111", "local-owner", "main")
	if err := Migrate(ctx, db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	applied, latest, err := SchemaVersions(ctx, db)
	if err != nil || applied != 22 || latest != 22 {
		t.Fatalf("schema = applied %d latest %d %v, want 22", applied, latest, err)
	}
	if _, found, err := NewConversationSessionStore(db).Resolve(ctx, "main/discord:owner/dm:111"); err != nil || found {
		t.Fatalf("existing conversation resolved to a pointer: %v %v", found, err)
	}
	rows, err := db.QueryContext(ctx, `PRAGMA foreign_key_check`)
	if err != nil {
		t.Fatalf("foreign_key_check: %v", err)
	}
	defer func() { _ = rows.Close() }()
	if rows.Next() {
		t.Error("foreign_key_check reported violations")
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("foreign_key_check rows: %v", err)
	}
	var index string
	if err := db.QueryRowContext(ctx, `SELECT name FROM sqlite_master WHERE type = 'index' AND name = 'conversation_session_session_idx'`).Scan(&index); err != nil {
		t.Fatalf("pointer index missing: %v", err)
	}
}
