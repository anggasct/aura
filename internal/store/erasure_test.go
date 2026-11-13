package store

import (
	"context"
	"database/sql"
	"fmt"
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

func seedEmailEffect(t *testing.T, db *sql.DB, id, session, provider, operation, request string) {
	t.Helper()
	const at = "2026-11-06T09:00:00Z"
	if _, err := db.ExecContext(t.Context(), `INSERT INTO effect_intent (id, session_id, turn_id, tool_call_id, idempotency_key, provider, operation, classification, state, request_digest, request_json, prepared_at, updated_at)
		VALUES (?, ?, 'turn-1', ?, ?, ?, ?, 'effectful', 'succeeded', 'digest', ?, ?, ?)`, id, session, id, "key-"+id, provider, operation, request, at, at); err != nil {
		t.Fatalf("seed effect %s: %v", id, err)
	}
}

func TestErasePrincipalRemovesEmailEffectsAddressedToThePrincipal(t *testing.T) {
	const (
		alice        = "email:alice@example.com"
		aliceSession = "lobby/email:desk/thread:aaaaaaaaaaaaaaaaaaaaaaaaaa"
		ownerSession = "main/email:desk/thread:bbbbbbbbbbbbbbbbbbbbbbbbbb"
	)
	cases := []struct {
		name  string
		apply bool
		want  int
	}{
		{name: "dry run counts and keeps every row", apply: false, want: 3},
		{name: "apply removes every row addressed to the principal", apply: true, want: 3},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db := newTestDB(t)
			seedPersonaSession(t, db, aliceSession, alice, "lobby")
			seedPersonaSession(t, db, ownerSession, "local-owner", "main")
			seedEmailEffect(t, db, "reply-alice", aliceSession, "email", "send_reply", `{"to":"alice@example.com","subject":"Re: hi","body":"hello alice"}`)
			seedEmailEffect(t, db, "fresh-alice", ownerSession, "email", "send_message", `{"to":"Alice@Example.com","subject":"news","body":"for alice"}`)
			seedEmailEffect(t, db, "tool-alice", ownerSession, "builtin-tools", "email_send", `{"tool_name":"email_send","arguments":{"to":"alice@example.com","subject":"news","body":"for alice"}}`)
			seedEmailEffect(t, db, "fresh-bob", ownerSession, "email", "send_message", `{"to":"bob@example.com","subject":"news","body":"for bob"}`)
			seedEmailEffect(t, db, "tool-other", ownerSession, "builtin-tools", "write_file", `{"tool_name":"write_file","arguments":{"to":"alice@example.com"}}`)
			if _, err := db.ExecContext(t.Context(), `INSERT INTO effect_approval (id, intent_id, action, request_digest, owner_id, reason, token_hash, issued_at, expires_at)
				VALUES ('ap-1', 'fresh-alice', 'mark_succeeded', 'digest', 'local-owner', 'reconcile', 'hash-1', '2026-11-06T09:00:00Z', '2026-11-06T10:00:00Z')`); err != nil {
				t.Fatalf("seed approval: %v", err)
			}
			counts, err := ErasePrincipal(t.Context(), db, alice, tc.apply, ErasureOptions{})
			if err != nil {
				t.Fatalf("ErasePrincipal: %v", err)
			}
			if counts.EmailEffects != tc.want {
				t.Errorf("EmailEffects = %d, want %d", counts.EmailEffects, tc.want)
			}
			held := countRows(t, db, `SELECT COUNT(*) FROM effect_intent WHERE request_json LIKE '%alice@%' OR request_json LIKE '%Alice@%'`)
			if tc.apply && held != 1 {
				t.Errorf("rows still naming the principal = %d, want only the unrelated write_file row", held)
			}
			if !tc.apply && held != 4 {
				t.Errorf("rows naming the principal after a dry run = %d, want 4", held)
			}
			if got := countRows(t, db, `SELECT COUNT(*) FROM effect_intent WHERE id = 'fresh-bob'`); got != 1 {
				t.Errorf("another recipient's effect rows = %d, want 1", got)
			}
			if tc.apply {
				if got := countRows(t, db, `SELECT COUNT(*) FROM effect_approval`); got != 0 {
					t.Errorf("effect approvals left = %d, want 0", got)
				}
				if got := countRows(t, db, `SELECT COUNT(*) FROM session WHERE owner_id = ?`, alice); got != 0 {
					t.Errorf("principal sessions left = %d, want 0", got)
				}
			}
		})
	}
}

func TestErasePrincipalIgnoresEmailEffectsForOtherChannels(t *testing.T) {
	db := newTestDB(t)
	seedPersonaSession(t, db, "main/email:desk/thread:cccccccccccccccccccccccccc", "local-owner", "main")
	seedEmailEffect(t, db, "fresh-alice", "main/email:desk/thread:cccccccccccccccccccccccccc", "email", "send_message", `{"to":"alice@example.com"}`)
	counts, err := ErasePrincipal(t.Context(), db, "discord:601", true, ErasureOptions{})
	if err != nil || counts.EmailEffects != 0 {
		t.Fatalf("ErasePrincipal = %+v, %v; want no email effects touched", counts, err)
	}
	if got := countRows(t, db, `SELECT COUNT(*) FROM effect_intent`); got != 1 {
		t.Errorf("effect rows = %d, want 1", got)
	}
}

func TestErasePrincipalSucceedsWhenAnOwnedEffectHasAnOperatorApproval(t *testing.T) {
	db := newTestDB(t)
	const session = "lobby/email:desk/thread:dddddddddddddddddddddddddd"
	seedPersonaSession(t, db, session, "email:carol@example.com", "lobby")
	seedEmailEffect(t, db, "reply-carol", session, "email", "send_reply", `{"to":"carol@example.com"}`)
	if _, err := db.ExecContext(t.Context(), `INSERT INTO effect_approval (id, intent_id, action, request_digest, owner_id, reason, token_hash, issued_at, expires_at)
		VALUES ('ap-2', 'reply-carol', 'mark_failed', 'digest', 'local-owner', 'reconcile', 'hash-2', '2026-11-06T09:00:00Z', '2026-11-06T10:00:00Z')`); err != nil {
		t.Fatalf("seed approval: %v", err)
	}
	if _, err := ErasePrincipal(t.Context(), db, "email:carol@example.com", true, ErasureOptions{}); err != nil {
		t.Fatalf("ErasePrincipal: %v", err)
	}
	if got := countRows(t, db, `SELECT (SELECT COUNT(*) FROM effect_intent) + (SELECT COUNT(*) FROM effect_approval)`); got != 0 {
		t.Errorf("effect rows left = %d, want 0", got)
	}
}

type durableRunFixture struct {
	key, handler, payload, state string
	actions                      int
}

func seedDurableRun(t *testing.T, db *sql.DB, run durableRunFixture) {
	t.Helper()
	const at = "2026-11-12T09:00:00Z"
	if _, err := db.ExecContext(t.Context(), `INSERT INTO durable_run (key, handler, payload, state, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?)`,
		run.key, run.handler, []byte(run.payload), run.state, at, at); err != nil {
		t.Fatalf("seed run %s: %v", run.key, err)
	}
	for i := range run.actions {
		if _, err := db.ExecContext(t.Context(), `INSERT INTO durable_action (run_key, action_key, outcome, result, created_at) VALUES (?, ?, 'result', ?, ?)`,
			run.key, fmt.Sprintf("step-%d", i), []byte(`{"text":"model reply"}`), at); err != nil {
			t.Fatalf("seed action of %s: %v", run.key, err)
		}
	}
}

type turnEventFixture struct {
	session, turn, kind string
}

func seedTurnEvent(t *testing.T, db *sql.DB, event turnEventFixture) {
	t.Helper()
	if _, err := db.ExecContext(t.Context(), `INSERT INTO runtime_event (id, session_id, sequence, turn_id, invocation_id, author, kind, schema_version, payload_json, created_at)
		VALUES (?1 || '-' || ?3, ?2, (SELECT COALESCE(MAX(sequence), 0) + 1 FROM runtime_event WHERE session_id = ?2), ?1, '', 'discord:601', ?3, 1, '{}', '2026-11-12T09:00:00Z')`,
		event.turn, event.session, event.kind); err != nil {
		t.Fatalf("seed %s of %s: %v", event.kind, event.turn, err)
	}
}

func TestErasePrincipalDeletesTheTurnRunsOfItsSessions(t *testing.T) {
	const (
		dmSession      = "lobby/discord:owner/dm:601"
		channelSession = "lobby/discord:owner/channel:333/principal:discord:601"
		otherSession   = "lobby/discord:owner/dm:602"
	)
	db := newTestDB(t)
	seedPersonaSession(t, db, dmSession, "discord:601", "lobby")
	seedPersonaSession(t, db, channelSession, "discord:601", "lobby")
	seedPersonaSession(t, db, otherSession, "discord:602", "lobby")
	for _, run := range []durableRunFixture{
		{key: "turn-601-ended", handler: "turn", payload: `{"turn_id":"turn-601-ended","session_id":"` + dmSession + `"}`, state: "succeeded", actions: 2},
		{key: "turn-601-running", handler: "turn", payload: `{"turn_id":"turn-601-running","session_id":"` + channelSession + `"}`, state: "running", actions: 1},
		{key: "turn-602", handler: "turn", payload: `{"turn_id":"turn-602","session_id":"` + otherSession + `"}`, state: "succeeded", actions: 1},
		{key: "turn-left-behind", handler: "turn", payload: `{"turn_id":"turn-left-behind","session_id":"lobby/discord:owner/dm:600"}`, state: "failed", actions: 1},
		{key: "workflow-601", handler: "workflow", payload: `{"session_id":"` + dmSession + `"}`, state: "succeeded", actions: 1},
		{key: "schedule-job", handler: "schedule", payload: `{"job_id":"daily"}`, state: "succeeded", actions: 1},
		{key: "turn-not-json", handler: "turn", payload: `not json`, state: "failed", actions: 1},
		{key: "turn-no-session", handler: "turn", payload: `{"turn_id":"turn-no-session"}`, state: "failed", actions: 1},
	} {
		seedDurableRun(t, db, run)
	}
	seedTurnEvent(t, db, turnEventFixture{session: otherSession, turn: "turn-602", kind: "turn.accepted"})
	if _, err := db.ExecContext(t.Context(), `INSERT INTO durable_signal (run_key, name, seq, payload, created_at) VALUES ('turn-601-ended', 'approval', 0, ?, '2026-11-12T09:00:00Z')`, []byte(`{}`)); err != nil {
		t.Fatalf("seed signal: %v", err)
	}
	const allRuns, allActions = 8, 9
	want := ErasureCounts{Sessions: 2}
	dry, err := ErasePrincipal(t.Context(), db, "discord:601", false, ErasureOptions{})
	if err != nil || dry != want {
		t.Fatalf("dry run = %+v, %v; want %+v", dry, err, want)
	}
	for query, expect := range map[string]int{
		`SELECT COUNT(*) FROM durable_run`:    allRuns,
		`SELECT COUNT(*) FROM durable_action`: allActions,
		`SELECT COUNT(*) FROM durable_signal`: 1,
	} {
		if got := countRows(t, db, query); got != expect {
			t.Errorf("after a dry run %s = %d, want %d", query, got, expect)
		}
	}
	applied, err := ErasePrincipal(t.Context(), db, "discord:601", true, ErasureOptions{})
	if err != nil || applied != want {
		t.Fatalf("apply = %+v, %v; want %+v", applied, err, want)
	}
	kept := `('turn-602', 'workflow-601', 'schedule-job', 'turn-not-json', 'turn-no-session')`
	for query, expect := range map[string]int{
		`SELECT COUNT(*) FROM durable_run WHERE key IN ('turn-601-ended', 'turn-601-running', 'turn-left-behind')`:        0,
		`SELECT COUNT(*) FROM durable_action WHERE run_key IN ('turn-601-ended', 'turn-601-running', 'turn-left-behind')`: 0,
		`SELECT COUNT(*) FROM durable_signal`:                          0,
		`SELECT COUNT(*) FROM durable_run WHERE key IN ` + kept:        5,
		`SELECT COUNT(*) FROM durable_action WHERE run_key IN ` + kept: 5,
	} {
		if got := countRows(t, db, query); got != expect {
			t.Errorf("after the apply %s = %d, want %d", query, got, expect)
		}
	}
	if _, err := ErasePrincipal(t.Context(), db, "discord:602", true, ErasureOptions{}); err != nil {
		t.Fatalf("erase the last principal: %v", err)
	}
	unattributable := `('workflow-601', 'schedule-job', 'turn-not-json', 'turn-no-session')`
	for query, expect := range map[string]int{
		`SELECT COUNT(*) FROM session`:                                           0,
		`SELECT COUNT(*) FROM durable_run`:                                       4,
		`SELECT COUNT(*) FROM durable_run WHERE key IN ` + unattributable:        4,
		`SELECT COUNT(*) FROM durable_action WHERE run_key IN ` + unattributable: 4,
	} {
		if got := countRows(t, db, query); got != expect {
			t.Errorf("with no session left %s = %d, want %d", query, got, expect)
		}
	}
}

func TestDeleteOrphanedTurnRuns(t *testing.T) {
	const (
		liveSession     = "lobby/discord:owner/dm:601"
		returnedSession = "lobby/discord:owner/dm:600"
		goneSession     = "lobby/discord:owner/dm:599"
	)
	db := newTestDB(t)
	seedPersonaSession(t, db, liveSession, "discord:601", "lobby")
	seedPersonaSession(t, db, returnedSession, "discord:600", "lobby")
	for _, run := range []durableRunFixture{
		{key: "turn-gone-running", handler: "turn", payload: `{"turn_id":"turn-gone-running","session_id":"` + goneSession + `"}`, state: "running", actions: 2},
		{key: "turn-returned-running", handler: "turn", payload: `{"turn_id":"turn-returned-running","session_id":"` + returnedSession + `"}`, state: "running", actions: 1},
		{key: "turn-returned-failed", handler: "turn", payload: `{"turn_id":"turn-returned-failed","session_id":"` + returnedSession + `"}`, state: "failed", actions: 1},
		{key: "turn-live", handler: "turn", payload: `{"turn_id":"turn-live","session_id":"` + liveSession + `"}`, state: "succeeded", actions: 1},
		{key: "workflow-gone", handler: "workflow", payload: `{"session_id":"` + goneSession + `"}`, state: "succeeded", actions: 1},
		{key: "turn-not-json", handler: "turn", payload: `not json`, state: "failed", actions: 1},
		{key: "turn-no-session", handler: "turn", payload: `{"turn_id":"turn-no-session"}`, state: "failed", actions: 1},
	} {
		seedDurableRun(t, db, run)
	}
	for _, event := range []turnEventFixture{
		{session: liveSession, turn: "turn-live", kind: "turn.accepted"},
		{session: liveSession, turn: "turn-returned-running", kind: "turn.accepted"},
		{session: returnedSession, turn: "turn-returned-failed", kind: "turn.cancelled"},
		{session: returnedSession, turn: "turn-returned-later", kind: "turn.accepted"},
	} {
		seedTurnEvent(t, db, event)
	}
	deleted, err := DeleteOrphanedTurnRuns(t.Context(), db)
	if err != nil || deleted != 3 {
		t.Fatalf("DeleteOrphanedTurnRuns = %d, %v; want 3", deleted, err)
	}
	orphans := `('turn-gone-running', 'turn-returned-running', 'turn-returned-failed')`
	kept := `('turn-live', 'workflow-gone', 'turn-not-json', 'turn-no-session')`
	for query, expect := range map[string]int{
		`SELECT COUNT(*) FROM durable_run WHERE key IN ` + orphans:        0,
		`SELECT COUNT(*) FROM durable_action WHERE run_key IN ` + orphans: 0,
		`SELECT COUNT(*) FROM durable_run WHERE key IN ` + kept:           4,
		`SELECT COUNT(*) FROM durable_action WHERE run_key IN ` + kept:    4,
		`SELECT COUNT(*) FROM durable_run`:                                4,
	} {
		if got := countRows(t, db, query); got != expect {
			t.Errorf("%s = %d, want %d", query, got, expect)
		}
	}
	if again, err := DeleteOrphanedTurnRuns(t.Context(), db); err != nil || again != 0 {
		t.Errorf("second DeleteOrphanedTurnRuns = %d, %v; want 0", again, err)
	}
	if _, err := DeleteOrphanedTurnRuns(t.Context(), nil); !hasCode(err, ErrorCodeInvalidArgument) {
		t.Errorf("DeleteOrphanedTurnRuns without a database = %v, want %s", err, ErrorCodeInvalidArgument)
	}
}

func TestErasureCountsRunSessionsApart(t *testing.T) {
	const (
		chat = "lobby/discord:owner/dm:601"
		run  = "wf-run-601"
	)
	t.Run("a chat session and a run session are counted apart", func(t *testing.T) {
		db := newTestDB(t)
		seedPersonaSession(t, db, chat, "discord:601", "lobby")
		seedPersonaSession(t, db, run, "discord:601", "lobby")
		want := ErasureCounts{Sessions: 1, RunSessions: 1}
		dry, err := ErasePrincipal(t.Context(), db, "discord:601", false, ErasureOptions{})
		if err != nil || dry != want {
			t.Fatalf("dry run = %+v, %v; want %+v", dry, err, want)
		}
		applied, err := ErasePrincipal(t.Context(), db, "discord:601", true, ErasureOptions{})
		if err != nil || applied != want {
			t.Fatalf("apply = %+v, %v; want %+v", applied, err, want)
		}
		if got := countRows(t, db, `SELECT COUNT(*) FROM session`); got != 0 {
			t.Fatalf("sessions left = %d, want 0", got)
		}
	})

	t.Run("a principal with only a run session still has it, its events and its intents erased", func(t *testing.T) {
		db := newTestDB(t)
		seedPersonaSession(t, db, run, "discord:601", "lobby")
		seedTurnEvent(t, db, turnEventFixture{session: run, turn: "turn-run", kind: "tool.requested"})
		seedEmailEffect(t, db, "run-effect", run, "builtin-tools", "write_file", `{}`)
		if _, err := db.ExecContext(t.Context(), `INSERT INTO effect_approval (id, intent_id, action, request_digest, owner_id, reason, token_hash, issued_at, expires_at)
			VALUES ('ap-run', 'run-effect', 'mark_failed', 'digest', 'local-owner', 'reconcile', 'hash-run', '2026-11-06T09:00:00Z', '2026-11-06T10:00:00Z')`); err != nil {
			t.Fatalf("seed approval: %v", err)
		}
		root := t.TempDir()
		artifacts := NewArtifactStore(db, root, DefaultArtifactQuotaBytes)
		if _, err := artifacts.Put(t.Context(), strings.NewReader("run output"), &ArtifactMetadata{ID: "art-run", SessionID: run, Filename: "out.txt", MediaType: "text/plain"}); err != nil {
			t.Fatalf("Put: %v", err)
		}
		applied, err := ErasePrincipal(t.Context(), db, "discord:601", true, ErasureOptions{ArtifactRoot: root})
		if err != nil || applied != (ErasureCounts{RunSessions: 1, Blobs: 1}) {
			t.Fatalf("apply = %+v, %v; want one run session and its blob", applied, err)
		}
		if got := countRows(t, db, `SELECT (SELECT COUNT(*) FROM session) + (SELECT COUNT(*) FROM runtime_event) + (SELECT COUNT(*) FROM effect_intent) + (SELECT COUNT(*) FROM effect_approval)`); got != 0 {
			t.Fatalf("rows left of the run session = %d, want 0", got)
		}
	})
}
