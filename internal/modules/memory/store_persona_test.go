package memory

import (
	"database/sql"
	"slices"
	"testing"

	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/principal"
	"github.com/anggasct/aura/internal/platform/sqlite"
	"github.com/anggasct/aura/internal/platform/sqlite/sqlitetest"
	turnstore "github.com/anggasct/aura/internal/platform/turn/store"
)

func seedPersonaSession(t *testing.T, db *sql.DB, sessionID, ownerID, agentID string) {
	t.Helper()
	if _, err := db.ExecContext(t.Context(), `INSERT INTO session (id, owner_id, agent_id, metadata_json, created_at, updated_at) VALUES (?,?,?,?,?,?)`,
		sessionID, ownerID, agentID, `{}`, "2026-09-12T12:00:00Z", "2026-09-12T12:00:00Z"); err != nil {
		t.Fatalf("seed session: %v", err)
	}
}

func TestMemoryStore_DocumentTakesItsSessionPersona(t *testing.T) {
	db, s := newTestSQLiteStore(t)
	ctx := t.Context()
	seedPersonaSession(t, db, "kuma/discord:home/dm:1", "owner-1", "kuma")
	if _, err := s.UpsertDocument(ctx, testMemoryDocument("mem-kuma", "kuma/discord:home/dm:1", KindEventText)); err != nil {
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
	db, s := newTestSQLiteStore(t)
	ctx := t.Context()
	for _, seed := range []struct{ session, agent, doc string }{
		{"kuma/s", "kuma", "mem-kuma"},
		{"nara/s", "nara", "mem-nara"},
		{"solo/s", "solo", "mem-solo"},
	} {
		seedPersonaSession(t, db, seed.session, "owner-1", seed.agent)
		if _, err := s.UpsertDocument(ctx, testMemoryDocument(seed.doc, seed.session, KindEventText)); err != nil {
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
			hits, err := s.Search(ctx, &StoredQuery{OwnerID: "owner-1", AgentIDs: tc.agents, Terms: "nightly backup", Limit: 10})
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

func TestMemoryPersonaBaselineSchemaDefaultsAndIndex(t *testing.T) {
	ctx := t.Context()
	db := sqlitetest.OpenMigrated(t, turnstore.Schema(), Schema())
	seedPersonaSession(t, db, "main-session", "owner-1", "main")
	if _, err := db.ExecContext(ctx, `INSERT INTO memory_document (id, owner_id, session_id, kind, source_from_sequence, source_to_sequence, content, trust_label, created_at) VALUES ('mem-default', 'owner-1', 'main-session', 'event_text', 1, 1, 'default agent text', 'owner_input', '2026-09-12T12:00:00Z')`); err != nil {
		t.Fatalf("seed document: %v", err)
	}
	s, err := NewSQLiteStore(db, turnstore.NewSessionService(db))
	if err != nil {
		t.Fatalf("NewSQLiteStore: %v", err)
	}
	document, found, err := s.GetDocument(ctx, "mem-default")
	if err != nil || !found {
		t.Fatalf("GetDocument = %v, %v", found, err)
	}
	if document.AgentID != "main" {
		t.Errorf("default agent_id = %q, want main", document.AgentID)
	}
	var index string
	if err := db.QueryRowContext(ctx, `SELECT name FROM sqlite_master WHERE type = 'index' AND name = 'memory_document_owner_agent_idx'`).Scan(&index); err != nil {
		t.Fatalf("persona index missing: %v", err)
	}
}

func TestMemoryTrustLabelBaselineConstraintAndSearch(t *testing.T) {
	ctx := t.Context()
	db := sqlitetest.OpenMigrated(t, turnstore.Schema(), Schema())
	rows := []struct{ id, owner, trust string }{
		{"mem-owner", principal.OwnerID, "owner_input"},
		{"mem-public", "http:abcdefabcdef", "untrusted_external"},
		{"mem-derived", "http:abcdefabcdef", "derived_untrusted"},
	}
	for _, row := range rows {
		session := "sess-" + row.id
		seedPersonaSession(t, db, session, row.owner, "main")
		if _, err := db.ExecContext(ctx, `INSERT INTO memory_document (id, owner_id, session_id, kind, source_from_sequence, source_to_sequence, content, trust_label, created_at) VALUES (?, ?, ?, 'event_text', 1, 1, 'nightly backup text', ?, '2026-09-12T12:00:00Z')`,
			row.id, row.owner, session, row.trust); err != nil {
			t.Fatalf("seed %s: %v", row.id, err)
		}
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO memory_document (id, owner_id, session_id, kind, source_from_sequence, source_to_sequence, content, trust_label, created_at) VALUES ('mem-bad-trust', 'owner-1', 'sess-mem-owner', 'event_text', 2, 2, 'bad', 'invalid_trust', '2026-09-12T12:00:00Z')`); !fault.HasCode(sqlite.Classify(err), sqlite.ErrorCodeConstraintCheck) {
		t.Fatalf("invalid trust_label err = %v, want check violation", err)
	}
	s, err := NewSQLiteStore(db, turnstore.NewSessionService(db))
	if err != nil {
		t.Fatalf("NewSQLiteStore: %v", err)
	}
	for _, row := range rows {
		t.Run(row.id, func(t *testing.T) {
			document, found, err := s.GetDocument(ctx, row.id)
			if err != nil || !found {
				t.Fatalf("GetDocument = %v, %v", found, err)
			}
			if document.TrustLabel != row.trust {
				t.Errorf("trust_label = %q, want %q", document.TrustLabel, row.trust)
			}
		})
	}
	hits, err := s.Search(ctx, &StoredQuery{OwnerID: "http:abcdefabcdef", Terms: "nightly", Limit: 10})
	if err != nil || len(hits) != 2 {
		t.Fatalf("search = %d hits, %v; want 2", len(hits), err)
	}
}
