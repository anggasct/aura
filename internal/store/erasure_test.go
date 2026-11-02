package store

import (
	"context"
	"database/sql"
	"strings"
	"testing"
)

func countRows(t *testing.T, db *sql.DB, query string, args ...any) int {
	t.Helper()
	var n int
	if err := db.QueryRowContext(t.Context(), query, args...).Scan(&n); err != nil {
		t.Fatalf("count %q: %v", query, err)
	}
	return n
}

func seedErasureFixture(t *testing.T, db *sql.DB, root string) {
	t.Helper()
	ctx := t.Context()
	seedPersonaSession(t, db, "lobby/discord:owner/dm:601", "discord:601", "lobby")
	seedPersonaSession(t, db, "lobby/discord:owner/channel:333/principal:discord:601", "discord:601", "lobby")
	seedPersonaSession(t, db, "lobby/discord:owner/thread:7001", "discord:602", "lobby")
	seedPersonaSession(t, db, "main/discord:owner/dm:777", "local-owner", "main")
	memories := NewMemoryStore(db)
	for id, doc := range map[string]struct {
		owner, session string
		sequence       int64
	}{
		"mem-601-dm":     {"discord:601", "lobby/discord:owner/dm:601", 1},
		"mem-601-thread": {"discord:601", "lobby/discord:owner/thread:7001", 1},
		"mem-602-thread": {"discord:602", "lobby/discord:owner/thread:7001", 2},
		"mem-owner":      {"local-owner", "main/discord:owner/dm:777", 1},
	} {
		document := testMemoryDocument(id, doc.session, MemoryKindEventText)
		document.OwnerID = doc.owner
		document.FromSequence, document.ToSequence = doc.sequence, doc.sequence
		if _, err := memories.UpsertDocument(ctx, document); err != nil {
			t.Fatalf("UpsertDocument(%s): %v", id, err)
		}
	}
	artifacts := NewArtifactStore(db, root, DefaultArtifactQuotaBytes)
	if _, err := artifacts.Put(ctx, strings.NewReader("parcel photo"), &ArtifactMetadata{ID: "art-601", SessionID: "lobby/discord:owner/dm:601", Filename: "parcel.png", MediaType: "image/png"}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if _, err := artifacts.Put(ctx, strings.NewReader("owner photo"), &ArtifactMetadata{ID: "art-owner", SessionID: "main/discord:owner/dm:777", Filename: "owner.png", MediaType: "image/png"}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	now := "2026-11-02T09:00:00Z"
	for _, stmt := range []string{
		`INSERT INTO principal_grant (principal_id, account, class, granted_at, granted_by) VALUES ('discord:601', 'owner', 'trusted', '` + now + `', 'local-owner')`,
		`INSERT INTO standing_grant (id, principal_id, agent_id, tool_name, granted_at, expires_at, granted_by) VALUES ('sg-1', 'discord:601', 'lobby', 'read_file', '` + now + `', '2026-11-09T09:00:00Z', 'local-owner')`,
		`INSERT INTO pairing_request (code_sha256, principal_id, account, created_at, expires_at) VALUES ('c1', 'discord:601', 'owner', '` + now + `', '2026-11-02T10:00:00Z')`,
		`INSERT INTO usage_reservation (id, invocation_id, attempt, model_definition_id, window_day, window_month, reserved_cost_micros, state, expires_at, created_at, updated_at, principal_id, principal_class)
			VALUES ('r-1', 'inv-1', 0, 'primary', '2026-11-02', '2026-11', 10, 'active', '` + now + `', '` + now + `', '` + now + `', 'discord:601', 'public')`,
	} {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
}

func TestErasePrincipalDryRunCountsAndApplyDeletes(t *testing.T) {
	db := newTestDB(t)
	root := t.TempDir()
	seedErasureFixture(t, db, root)
	ctx := context.Background()
	want := ErasureCounts{Sessions: 2, MemoryDocuments: 2, Grants: 1, StandingGrants: 1, PairingRequests: 1, UsageLinks: 1}
	dry, err := ErasePrincipal(ctx, db, "discord:601", false, ErasureOptions{ArtifactRoot: root})
	if err != nil || dry != want {
		t.Fatalf("dry run = %+v, %v; want %+v", dry, err, want)
	}
	if countRows(t, db, `SELECT COUNT(*) FROM session WHERE owner_id = 'discord:601'`) != 2 {
		t.Fatal("dry run deleted sessions")
	}
	applied, err := ErasePrincipal(ctx, db, "discord:601", true, ErasureOptions{ArtifactRoot: root})
	want.Blobs = 1
	if err != nil || applied != want {
		t.Fatalf("apply = %+v, %v; want %+v", applied, err, want)
	}
	for query, expect := range map[string]int{
		`SELECT COUNT(*) FROM session WHERE owner_id = 'discord:601'`:                            0,
		`SELECT COUNT(*) FROM memory_document WHERE owner_id = 'discord:601'`:                    0,
		`SELECT COUNT(*) FROM principal_grant WHERE principal_id = 'discord:601'`:                0,
		`SELECT COUNT(*) FROM standing_grant WHERE principal_id = 'discord:601'`:                 0,
		`SELECT COUNT(*) FROM pairing_request WHERE principal_id = 'discord:601'`:                0,
		`SELECT COUNT(*) FROM usage_reservation WHERE principal_id = 'discord:601'`:              0,
		`SELECT COUNT(*) FROM usage_reservation WHERE id = 'r-1' AND principal_class = 'public'`: 1,
		`SELECT COUNT(*) FROM artifact_ref WHERE id = 'art-601'`:                                 0,
		`SELECT COUNT(*) FROM session WHERE id = 'lobby/discord:owner/thread:7001'`:              1,
		`SELECT COUNT(*) FROM memory_document WHERE id IN ('mem-602-thread', 'mem-owner')`:       2,
		`SELECT COUNT(*) FROM artifact_ref WHERE id = 'art-owner'`:                               1,
		`SELECT COUNT(*) FROM blob`: 1,
	} {
		if got := countRows(t, db, query); got != expect {
			t.Errorf("%s = %d, want %d", query, got, expect)
		}
	}
	again, err := ErasePrincipal(ctx, db, "discord:601", true, ErasureOptions{ArtifactRoot: root})
	if err != nil || again != (ErasureCounts{}) {
		t.Fatalf("second apply = %+v, %v; want nothing left", again, err)
	}
}

func TestErasePrincipalCanKeepGrantsAndRefusesTheOwner(t *testing.T) {
	db := newTestDB(t)
	seedErasureFixture(t, db, t.TempDir())
	counts, err := ErasePrincipal(t.Context(), db, "discord:601", true, ErasureOptions{KeepGrants: true})
	if err != nil || counts.Grants != 0 || counts.Sessions != 2 {
		t.Fatalf("keep-grants erase = %+v, %v", counts, err)
	}
	if countRows(t, db, `SELECT COUNT(*) FROM principal_grant WHERE principal_id = 'discord:601'`) != 1 {
		t.Fatal("pairing grant removed despite KeepGrants")
	}
	for _, id := range []string{"local-owner", ""} {
		if _, err := ErasePrincipal(t.Context(), db, id, true, ErasureOptions{}); err == nil {
			t.Fatalf("erasing %q accepted", id)
		}
	}
}
