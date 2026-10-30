package store

import (
	"database/sql"
	"path/filepath"
	"slices"
	"testing"
)

func seedPersonaSession(t *testing.T, db *sql.DB, sessionID, ownerID, agentID string) {
	t.Helper()
	if _, err := db.ExecContext(t.Context(), `INSERT INTO session (id, owner_id, agent_id, metadata_json, created_at, updated_at) VALUES (?,?,?,?,?,?)`,
		sessionID, ownerID, agentID, `{}`, "2026-09-12T12:00:00Z", "2026-09-12T12:00:00Z"); err != nil {
		t.Fatalf("seed session: %v", err)
	}
}

func TestMemoryStore_DocumentTakesItsSessionPersona(t *testing.T) {
	db := newTestDB(t)
	s := NewMemoryStore(db)
	ctx := t.Context()
	seedPersonaSession(t, db, "kuma/discord:home/dm:1", "owner-1", "kuma")
	if _, err := s.UpsertDocument(ctx, testMemoryDocument("mem-kuma", "kuma/discord:home/dm:1", MemoryKindEventText)); err != nil {
		t.Fatalf("UpsertDocument: %v", err)
	}
	document, found, err := s.GetDocument(ctx, "mem-kuma")
	if err != nil || !found {
		t.Fatalf("GetDocument = %v, %v", found, err)
	}
	if document.AgentID != "kuma" {
		t.Fatalf("agent_id = %q, want the session persona kuma", document.AgentID)
	}
}

func TestMemoryStore_SearchFiltersByReadablePersonas(t *testing.T) {
	db := newTestDB(t)
	s := NewMemoryStore(db)
	ctx := t.Context()
	for _, seed := range []struct{ session, agent, doc string }{
		{"kuma/s", "kuma", "mem-kuma"},
		{"nara/s", "nara", "mem-nara"},
		{"solo/s", "solo", "mem-solo"},
	} {
		seedPersonaSession(t, db, seed.session, "owner-1", seed.agent)
		if _, err := s.UpsertDocument(ctx, testMemoryDocument(seed.doc, seed.session, MemoryKindEventText)); err != nil {
			t.Fatalf("UpsertDocument(%s): %v", seed.doc, err)
		}
	}
	cases := map[string]struct {
		agents []string
		want   []string
	}{
		"single persona":  {agents: []string{"solo"}, want: []string{"mem-solo"}},
		"shared personas": {agents: []string{"kuma", "nara"}, want: []string{"mem-kuma", "mem-nara"}},
		"operator view":   {want: []string{"mem-kuma", "mem-nara", "mem-solo"}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			hits, err := s.Search(ctx, &MemoryQuery{OwnerID: "owner-1", AgentIDs: tc.agents, Terms: "nightly backup", Limit: 10})
			if err != nil {
				t.Fatalf("Search: %v", err)
			}
			got := make([]string, 0, len(hits))
			for i := range hits {
				got = append(got, hits[i].ID)
			}
			slices.Sort(got)
			if !slices.Equal(got, tc.want) {
				t.Fatalf("hits = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestMemoryPersonaMigrationBackfillsFromSession(t *testing.T) {
	ctx := t.Context()
	db, err := OpenDB(ctx, filepath.Join(t.TempDir(), "aura.db"))
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.ExecContext(ctx, bootstrapSchemaMigrationTableSQL); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	for _, m := range migrations[:20] {
		if err := applyMigration(ctx, db, m); err != nil {
			t.Fatalf("apply v%d: %v", m.version, err)
		}
	}
	seedPersonaSession(t, db, "engineer-session", "owner-1", "engineer")
	seedPersonaSession(t, db, "main-session", "owner-1", "main")
	for _, row := range []struct{ id, session string }{{"mem-engineer", "engineer-session"}, {"mem-main", "main-session"}} {
		if _, err := db.ExecContext(ctx, `INSERT INTO memory_document (id, owner_id, session_id, kind, source_from_sequence, source_to_sequence, content, trust_label, created_at) VALUES (?, 'owner-1', ?, 'event_text', 1, 1, 'pre-upgrade text', 'owner_input', '2026-09-12T12:00:00Z')`,
			row.id, row.session); err != nil {
			t.Fatalf("seed legacy document: %v", err)
		}
	}
	if err := Migrate(ctx, db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	s := NewMemoryStore(db)
	for id, want := range map[string]string{"mem-engineer": "engineer", "mem-main": "main"} {
		document, found, err := s.GetDocument(ctx, id)
		if err != nil || !found {
			t.Fatalf("GetDocument(%s) = %v, %v", id, found, err)
		}
		if document.AgentID != want {
			t.Errorf("%s agent_id = %q, want %q", id, document.AgentID, want)
		}
	}
	var index string
	if err := db.QueryRowContext(ctx, `SELECT name FROM sqlite_master WHERE type = 'index' AND name = 'memory_document_owner_agent_idx'`).Scan(&index); err != nil {
		t.Fatalf("persona index missing: %v", err)
	}
}
