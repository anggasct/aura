package app

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/anggasct/aura/internal/platform/principal"
	"github.com/anggasct/aura/internal/platform/storage"
	"github.com/anggasct/aura/internal/store"
)

func requireErasureReport(t *testing.T, got principal.ErasureReport, want map[string]int) {
	t.Helper()
	expected, err := principal.NewErasureReport(want)
	if err != nil {
		t.Fatalf("NewErasureReport: %v", err)
	}
	if !slices.Equal(got.Lines(), expected.Lines()) {
		t.Fatalf("report = %v, want %v", got.Lines(), expected.Lines())
	}
}

func TestErasePrincipalDryRunCountsAndApplyDeletes(t *testing.T) {
	db := newTestDB(t)
	root := t.TempDir()
	seedErasureFixture(t, db, root)
	ctx := context.Background()
	artifacts := storage.NewArtifactStore(db, root, storage.DefaultArtifactQuotaBytes)
	orphanMeta, err := artifacts.Put(ctx, strings.NewReader("orphan blob"), &storage.ArtifactMetadata{ID: "art-orphan", SessionID: "main/discord:owner/dm:777", Filename: "orphan.txt", MediaType: "text/plain"})
	if err != nil {
		t.Fatalf("Put orphan: %v", err)
	}
	if _, err := db.ExecContext(ctx, `DELETE FROM artifact_ref WHERE id = 'art-orphan'`); err != nil {
		t.Fatalf("unreference orphan: %v", err)
	}
	var orphanPath string
	if err := db.QueryRowContext(ctx, `SELECT relative_path FROM blob WHERE digest = ?`, orphanMeta.BlobDigest).Scan(&orphanPath); err != nil {
		t.Fatalf("read orphan path: %v", err)
	}
	want := map[string]int{
		principal.ErasureLineSessions:        2,
		principal.ErasureLineMemoryDocuments: 2,
		principal.ErasureLineGrants:          1,
		principal.ErasureLineStandingGrants:  1,
		principal.ErasureLinePairingRequests: 1,
		principal.ErasureLineUsageLinks:      1,
	}
	dry, dryBlobs, err := erasePrincipal(ctx, db, principal.ErasureScope{PrincipalID: "discord:601", Apply: false}, root)
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	requireErasureReport(t, dry, want)
	if dryBlobs != 0 {
		t.Fatalf("dry run deleted %d blobs, want 0", dryBlobs)
	}
	if _, err := os.Stat(filepath.Join(root, orphanPath)); err != nil {
		t.Fatalf("dry run removed unreferenced blob file: %v", err)
	}
	if countRows(t, db, `SELECT COUNT(*) FROM session WHERE owner_id = 'discord:601'`) != 2 {
		t.Fatal("dry run deleted sessions")
	}
	applied, appliedBlobs, err := erasePrincipal(ctx, db, principal.ErasureScope{PrincipalID: "discord:601", Apply: true}, root)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	requireErasureReport(t, applied, want)
	if appliedBlobs != 2 {
		t.Fatalf("apply blobs = %d, want 2", appliedBlobs)
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
	again, againBlobs, err := erasePrincipal(ctx, db, principal.ErasureScope{PrincipalID: "discord:601", Apply: true}, root)
	if err != nil {
		t.Fatalf("second apply: %v", err)
	}
	requireErasureReport(t, again, nil)
	if againBlobs != 0 {
		t.Fatalf("second apply blobs = %d, want 0", againBlobs)
	}
}

func TestErasePrincipalCanKeepGrantsAndRefusesTheOwner(t *testing.T) {
	db := newTestDB(t)
	seedErasureFixture(t, db, t.TempDir())
	report, _, err := erasePrincipal(t.Context(), db, principal.ErasureScope{PrincipalID: "discord:601", Apply: true, KeepGrants: true}, "")
	if err != nil || report.Count(principal.ErasureLineGrants) != 0 || report.Count(principal.ErasureLineSessions) != 2 {
		t.Fatalf("keep-grants erase = %v, %v", report.Lines(), err)
	}
	if countRows(t, db, `SELECT COUNT(*) FROM principal_grant WHERE principal_id = 'discord:601'`) != 1 {
		t.Fatal("pairing grant removed despite KeepGrants")
	}
	for _, id := range []string{"local-owner", ""} {
		if _, _, err := erasePrincipal(t.Context(), db, principal.ErasureScope{PrincipalID: id, Apply: true}, ""); err == nil {
			t.Fatalf("erasing %q accepted", id)
		}
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
			report, _, err := erasePrincipal(t.Context(), db, principal.ErasureScope{PrincipalID: alice, Apply: tc.apply}, "")
			if err != nil {
				t.Fatalf("erasePrincipal: %v", err)
			}
			if got := report.Count(principal.ErasureLineEmailEffects); got != tc.want {
				t.Errorf("EmailEffects = %d, want %d", got, tc.want)
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
	report, _, err := erasePrincipal(t.Context(), db, principal.ErasureScope{PrincipalID: "discord:601", Apply: true}, "")
	if err != nil || report.Count(principal.ErasureLineEmailEffects) != 0 {
		t.Fatalf("erasePrincipal = %v, %v; want no email effects touched", report.Lines(), err)
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
	if _, _, err := erasePrincipal(t.Context(), db, principal.ErasureScope{PrincipalID: "email:carol@example.com", Apply: true}, ""); err != nil {
		t.Fatalf("erasePrincipal: %v", err)
	}
	if got := countRows(t, db, `SELECT (SELECT COUNT(*) FROM effect_intent) + (SELECT COUNT(*) FROM effect_approval)`); got != 0 {
		t.Errorf("effect rows left = %d, want 0", got)
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
	want := map[string]int{principal.ErasureLineSessions: 2}
	dry, _, err := erasePrincipal(t.Context(), db, principal.ErasureScope{PrincipalID: "discord:601", Apply: false}, "")
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	requireErasureReport(t, dry, want)
	for query, expect := range map[string]int{
		`SELECT COUNT(*) FROM durable_run`:    allRuns,
		`SELECT COUNT(*) FROM durable_action`: allActions,
		`SELECT COUNT(*) FROM durable_signal`: 1,
	} {
		if got := countRows(t, db, query); got != expect {
			t.Errorf("after a dry run %s = %d, want %d", query, got, expect)
		}
	}
	applied, _, err := erasePrincipal(t.Context(), db, principal.ErasureScope{PrincipalID: "discord:601", Apply: true}, "")
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	requireErasureReport(t, applied, want)
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
	if _, _, err := erasePrincipal(t.Context(), db, principal.ErasureScope{PrincipalID: "discord:602", Apply: true}, ""); err != nil {
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

func TestErasureCountsRunSessionsApart(t *testing.T) {
	const (
		chat = "lobby/discord:owner/dm:601"
		run  = "wf-run-601"
	)
	t.Run("a chat session and a run session are counted apart", func(t *testing.T) {
		db := newTestDB(t)
		seedPersonaSession(t, db, chat, "discord:601", "lobby")
		seedPersonaSession(t, db, run, "discord:601", "lobby")
		want := map[string]int{principal.ErasureLineSessions: 1, principal.ErasureLineRunSessions: 1}
		dry, _, err := erasePrincipal(t.Context(), db, principal.ErasureScope{PrincipalID: "discord:601", Apply: false}, "")
		if err != nil {
			t.Fatalf("dry run: %v", err)
		}
		requireErasureReport(t, dry, want)
		applied, _, err := erasePrincipal(t.Context(), db, principal.ErasureScope{PrincipalID: "discord:601", Apply: true}, "")
		if err != nil {
			t.Fatalf("apply: %v", err)
		}
		requireErasureReport(t, applied, want)
		if got := countRows(t, db, `SELECT COUNT(*) FROM session`); got != 0 {
			t.Fatalf("sessions left = %d, want 0", got)
		}
	})

	t.Run("an upper-case WF- chat session is a chat session", func(t *testing.T) {
		db := newTestDB(t)
		seedPersonaSession(t, db, "WF-chat-601", "discord:601", "lobby")
		got, _, err := erasePrincipal(t.Context(), db, principal.ErasureScope{PrincipalID: "discord:601", Apply: false}, "")
		if err != nil {
			t.Fatalf("dry run: %v", err)
		}
		requireErasureReport(t, got, map[string]int{principal.ErasureLineSessions: 1})
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
		artifacts := storage.NewArtifactStore(db, root, storage.DefaultArtifactQuotaBytes)
		if _, err := artifacts.Put(t.Context(), strings.NewReader("run output"), &storage.ArtifactMetadata{ID: "art-run", SessionID: run, Filename: "out.txt", MediaType: "text/plain"}); err != nil {
			t.Fatalf("Put: %v", err)
		}
		applied, blobs, err := erasePrincipal(t.Context(), db, principal.ErasureScope{PrincipalID: "discord:601", Apply: true}, root)
		if err != nil {
			t.Fatalf("apply: %v", err)
		}
		requireErasureReport(t, applied, map[string]int{principal.ErasureLineRunSessions: 1})
		if blobs != 1 {
			t.Fatalf("apply blobs = %d, want 1", blobs)
		}
		if got := countRows(t, db, `SELECT (SELECT COUNT(*) FROM session) + (SELECT COUNT(*) FROM runtime_event) + (SELECT COUNT(*) FROM effect_intent) + (SELECT COUNT(*) FROM effect_approval)`); got != 0 {
			t.Fatalf("rows left of the run session = %d, want 0", got)
		}
	})
}

func TestErasureRefusesWhileARunItTouchesIsLive(t *testing.T) {
	const (
		principalID = "discord:601"
		thread      = "lobby/discord:owner/thread:7001"
	)
	cases := []struct {
		name     string
		seed     func(t *testing.T, db *sql.DB)
		wantLive []string
	}{
		{name: "a running root of the principal", wantLive: []string{"run-root"}, seed: func(t *testing.T, db *sql.DB) {
			seedWorkflowDefinition(t, db, "own")
			seedDurableRun(t, db, durableRunFixture{key: "dur-root", handler: "workflow", payload: `{}`, state: "running", actions: 1})
			seedWorkflowRun(t, db, &seededRun{id: "run-root", definition: "own", principal: principalID, status: "running", durableKey: "dur-root", steps: []string{"a"}})
		}},
		{name: "a child of a live root is not counted on its own", wantLive: []string{"run-root"}, seed: func(t *testing.T, db *sql.DB) {
			seedWorkflowDefinition(t, db, "own")
			seedDurableRun(t, db, durableRunFixture{key: "dur-root", handler: "workflow", payload: `{}`, state: "running"})
			seedWorkflowRun(t, db, &seededRun{id: "run-root", definition: "own", principal: principalID, status: "running", durableKey: "dur-root"})
			seedWorkflowRun(t, db, &seededRun{id: "run-child", definition: "own", principal: principalID, status: "running", parent: "run-root"})
		}},
		{name: "a live run of the owner on a proposal held in the principal's session", wantLive: []string{"run-owner"}, seed: func(t *testing.T, db *sql.DB) {
			seedWorkflowDefinition(t, db, "proposed")
			seedProposalApproval(t, db, "prop-1", thread, "proposed")
			seedDurableRun(t, db, durableRunFixture{key: "dur-owner", handler: "workflow", payload: `{}`, state: "running"})
			seedWorkflowRun(t, db, &seededRun{id: "run-owner", definition: "proposed", principal: "local-owner", status: "running", durableKey: "dur-owner"})
		}},
		{name: "a status that says running while the durable run is ended is not live", wantLive: nil, seed: func(t *testing.T, db *sql.DB) {
			seedWorkflowDefinition(t, db, "own")
			seedDurableRun(t, db, durableRunFixture{key: "dur-root", handler: "workflow", payload: `{}`, state: "cancelled"})
			seedWorkflowRun(t, db, &seededRun{id: "run-root", definition: "own", principal: principalID, status: "running", durableKey: "dur-root"})
		}},
		{name: "a terminal status over a running durable run is live", wantLive: []string{"run-root"}, seed: func(t *testing.T, db *sql.DB) {
			seedWorkflowDefinition(t, db, "own")
			seedDurableRun(t, db, durableRunFixture{key: "dur-root", handler: "workflow", payload: `{}`, state: "running"})
			seedWorkflowRun(t, db, &seededRun{id: "run-root", definition: "own", principal: principalID, status: "succeeded", durableKey: "dur-root"})
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db := newTestDB(t)
			seedPersonaSession(t, db, thread, principalID, "lobby")
			seedPersonaSession(t, db, "lobby/discord:owner/dm:601", principalID, "lobby")
			tc.seed(t, db)
			before := workflowDataRows(t, db)

			live, err := store.LiveWorkflowRuns(t.Context(), db, principalID)
			if err != nil || !slices.Equal(live, tc.wantLive) {
				t.Fatalf("LiveWorkflowRuns = %v, %v; want %v", live, err, tc.wantLive)
			}
			dry, _, err := erasePrincipal(t.Context(), db, principal.ErasureScope{PrincipalID: principalID, Apply: false}, "")
			if err != nil || dry.Count(principal.ErasureLineLiveWorkflowRuns) != len(tc.wantLive) {
				t.Fatalf("dry run = %v, %v; want %d live runs", dry.Lines(), err, len(tc.wantLive))
			}
			if len(tc.wantLive) == 0 {
				return
			}
			report, _, err := erasePrincipal(t.Context(), db, principal.ErasureScope{PrincipalID: principalID, Apply: true}, "")
			requireStoreErasureCode(t, err, principal.ErrorCodeErasureBlocked)
			if !strings.Contains(err.Error(), tc.wantLive[0]) || report.Count(principal.ErasureLineLiveWorkflowRuns) != len(tc.wantLive) {
				t.Fatalf("refusal = %v, counts %v; want it to name %v", err, report.Lines(), tc.wantLive)
			}
			after := workflowDataRows(t, db)
			for table, rows := range before {
				if after[table] != rows {
					t.Errorf("after a refused erasure %s has %d rows, want %d", table, after[table], rows)
				}
			}
			if countRows(t, db, `SELECT COUNT(*) FROM session WHERE owner_id = ?`, principalID) != 2 {
				t.Error("a refused erasure deleted sessions")
			}
		})
	}
}

func TestErasureReconcilesStaleRunRows(t *testing.T) {
	const principalID = "discord:601"
	cases := []struct {
		name    string
		durable string
		status  string
		want    string
	}{
		{name: "queued with no durable run", durable: "", status: "queued", want: "failed"},
		{name: "running over a cancelled durable run", durable: "cancelled", status: "running", want: "cancelled"},
		{name: "suspended over a succeeded durable run", durable: "succeeded", status: "suspended", want: "succeeded"},
		{name: "running over a failed durable run", durable: "failed", status: "running", want: "failed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db := newTestDB(t)
			seedPersonaSession(t, db, "lobby/discord:owner/dm:601", principalID, "lobby")
			seedWorkflowDefinition(t, db, "own")
			key := ""
			if tc.durable != "" {
				key = "dur-root"
				seedDurableRun(t, db, durableRunFixture{key: key, handler: "workflow", payload: `{}`, state: tc.durable})
			}
			seedWorkflowRun(t, db, &seededRun{id: "run-root", definition: "own", principal: principalID, status: tc.status, durableKey: key})
			seedWorkflowRun(t, db, &seededRun{id: "run-child", definition: "own", principal: principalID, status: "running", parent: "run-root"})
			seedWorkflowRun(t, db, &seededRun{id: "run-child-done", definition: "own", principal: principalID, status: "succeeded", parent: "run-root"})

			live, err := store.LiveWorkflowRuns(t.Context(), db, principalID)
			if err != nil || len(live) != 0 {
				t.Fatalf("LiveWorkflowRuns = %v, %v; want none", live, err)
			}
			for run, want := range map[string]string{"run-root": tc.want, "run-child": tc.want, "run-child-done": "succeeded"} {
				if got := countRows(t, db, `SELECT COUNT(*) FROM workflow_run WHERE id = ? AND status = ?`, run, want); got != 1 {
					t.Errorf("run %s is not %s", run, want)
				}
			}
			report, _, err := erasePrincipal(t.Context(), db, principal.ErasureScope{PrincipalID: principalID, Apply: true}, "")
			if err != nil || report.Count(principal.ErasureLineWorkflowRuns) != 3 {
				t.Fatalf("erasure = %v, %v; want it to remove the three settled runs", report.Lines(), err)
			}
		})
	}
}

func TestErasureDeletesThePrincipalsWorkflowRuns(t *testing.T) {
	const (
		principalID  = "discord:601"
		other        = "discord:602"
		dm           = "lobby/discord:owner/dm:601"
		runSession   = "wf-run-1"
		otherSession = "lobby/discord:owner/dm:602"
	)
	db := newTestDB(t)
	seedErasureFixture(t, db, t.TempDir())
	seedPersonaSession(t, db, runSession, principalID, "main")
	seedPersonaSession(t, db, otherSession, other, "lobby")
	seedWorkflowDefinition(t, db, "shared")
	seedWorkflowDefinition(t, db, "owners")
	run1 := &seededRun{id: "run-1", definition: "shared", principal: principalID, status: "succeeded", durableKey: "dur-1", steps: []string{"fetch", "write"}}
	child1 := &seededRun{id: "run-1-child", definition: "shared", principal: principalID, status: "failed", parent: "run-1", steps: []string{"fetch"}}
	run2 := &seededRun{id: "run-2", definition: "shared", principal: other, status: "succeeded", durableKey: "dur-2", steps: []string{"fetch"}}
	ownerRun := &seededRun{id: "run-owner", definition: "owners", principal: "local-owner", status: "succeeded", durableKey: "dur-owner", originSession: dm}
	for _, run := range []*seededRun{run1, child1, run2, ownerRun} {
		seedWorkflowRun(t, db, run)
	}
	for _, run := range []*seededRun{run1, run2, ownerRun} {
		seedRunEvidence(t, db, run)
	}
	legacy := []string{
		seedLegacyStepSession(t, db, "run-1", "fetch"),
		seedLegacyStepSession(t, db, "run-1", "write"),
		seedLegacyStepSession(t, db, "run-1-child", "fetch"),
	}
	keptLegacy := seedLegacyStepSession(t, db, "run-2", "fetch")

	want := map[string]int{
		principal.ErasureLineSessions:           2,
		principal.ErasureLineRunSessions:        1,
		principal.ErasureLineMemoryDocuments:    2,
		principal.ErasureLineGrants:             1,
		principal.ErasureLineStandingGrants:     1,
		principal.ErasureLinePairingRequests:    1,
		principal.ErasureLineUsageLinks:         1,
		principal.ErasureLineWorkflowRuns:       2,
		principal.ErasureLineLegacyStepSessions: 3,
	}
	dry, _, err := erasePrincipal(t.Context(), db, principal.ErasureScope{PrincipalID: principalID, Apply: false}, "")
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	requireErasureReport(t, dry, want)
	if countRows(t, db, `SELECT COUNT(*) FROM workflow_run`) != 4 {
		t.Fatal("dry run deleted workflow runs")
	}

	applied, _, err := erasePrincipal(t.Context(), db, principal.ErasureScope{PrincipalID: principalID, Apply: true}, "")
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	requireErasureReport(t, applied, want)
	for query, expect := range map[string]int{
		`SELECT COUNT(*) FROM workflow_run WHERE principal_id = 'discord:601'`:                                                                          0,
		`SELECT COUNT(*) FROM workflow_step_run WHERE run_id IN ('run-1', 'run-1-child')`:                                                               0,
		`SELECT COUNT(*) FROM durable_run WHERE key = 'dur-1'`:                                                                                          0,
		`SELECT COUNT(*) FROM durable_action WHERE run_key = 'dur-1'`:                                                                                   0,
		`SELECT COUNT(*) FROM durable_signal WHERE run_key = 'dur-1'`:                                                                                   0,
		`SELECT COUNT(*) FROM session WHERE id IN ('` + strings.Join(legacy, `','`) + `')`:                                                              0,
		`SELECT COUNT(*) FROM runtime_event WHERE session_id IN ('` + strings.Join(legacy, `','`) + `')`:                                                0,
		`SELECT COUNT(*) FROM effect_intent WHERE session_id IN ('` + strings.Join(legacy, `','`) + `')`:                                                0,
		`SELECT COUNT(*) FROM session WHERE id = 'wf-run-1'`:                                                                                            0,
		`SELECT COUNT(*) FROM workflow_run WHERE id IN ('run-2', 'run-owner')`:                                                                          2,
		`SELECT COUNT(*) FROM workflow_step_run WHERE run_id = 'run-2'`:                                                                                 1,
		`SELECT COUNT(*) FROM durable_run WHERE key IN ('dur-2', 'dur-owner')`:                                                                          2,
		`SELECT COUNT(*) FROM durable_action WHERE run_key IN ('dur-2', 'dur-owner')`:                                                                   2,
		`SELECT COUNT(*) FROM session WHERE id = '` + keptLegacy + `'`:                                                                                  1,
		`SELECT COUNT(*) FROM effect_intent WHERE session_id = '` + keptLegacy + `'`:                                                                    1,
		`SELECT COUNT(*) FROM workflow_run WHERE id = 'run-owner' AND origin_session_id IS NULL AND reply_account IS NULL AND reply_channel_id IS NULL`: 1,
		`SELECT COUNT(*) FROM workflow_run WHERE id = 'run-2' AND reply_account = 'acct' AND reply_channel_id = 'chan'`:                                 1,
		`SELECT COUNT(*) FROM usage_reservation WHERE id = 'r-1' AND principal_id IS NULL AND principal_class = 'public'`:                               1,
	} {
		if got := countRows(t, db, query); got != expect {
			t.Errorf("%s = %d, want %d", query, got, expect)
		}
	}
}

func TestErasingTheOwnerOfAProposalsSessionSucceeds(t *testing.T) {
	const (
		principalID = "discord:111"
		thread      = "lobby/discord:owner/thread:9001"
		ownerDM     = "main/discord:owner/dm:777"
	)
	db := newTestDB(t)
	seedPersonaSession(t, db, thread, principalID, "lobby")
	seedPersonaSession(t, db, ownerDM, "local-owner", "main")
	seedWorkflowDefinition(t, db, "unreferenced")
	seedWorkflowDefinition(t, db, "referenced")
	seedWorkflowDefinition(t, db, "routine-copy")
	seedProposalApproval(t, db, "prop-free", thread, "unreferenced")
	seedProposalApproval(t, db, "prop-used", thread, "referenced")
	seedProposalApproval(t, db, "prop-elsewhere", ownerDM, "routine-copy")
	if _, err := db.ExecContext(t.Context(), `INSERT INTO workflow_definition (id, version, goal, source, spec_json, spec_sha256, created_at) VALUES ('routine', 1, 'goal', 'composed', '{}', 'digest', ?)`, erasureWorkflowStamp); err != nil {
		t.Fatalf("seed routine definition: %v", err)
	}
	if _, err := db.ExecContext(t.Context(), `INSERT INTO workflow_approval (definition_id, definition_version, spec_sha256, kind, agent_id, approver_id, source_definition_id, source_definition_version, approved_at)
		VALUES ('routine', 1, 'digest', 'routine', 'main', 'local-owner', 'unreferenced', 1, ?)`, erasureWorkflowStamp); err != nil {
		t.Fatalf("seed routine approval: %v", err)
	}
	seedDurableRun(t, db, durableRunFixture{key: "dur-done", handler: "workflow", payload: `{}`, state: "succeeded"})
	seedWorkflowRun(t, db, &seededRun{id: "run-done", definition: "referenced", principal: "local-owner", status: "succeeded", durableKey: "dur-done"})

	report, _, err := erasePrincipal(t.Context(), db, principal.ErasureScope{PrincipalID: principalID, Apply: true}, "")
	if err != nil {
		t.Fatalf("erasePrincipal: %v", err)
	}
	if report.Count(principal.ErasureLineWorkflowApprovals) != 2 || report.Count(principal.ErasureLineSessions) != 1 {
		t.Fatalf("counts = %v, want 2 approvals and 1 session", report.Lines())
	}
	for query, expect := range map[string]int{
		`SELECT COUNT(*) FROM workflow_proposal WHERE session_id = '` + thread + `'`:                  0,
		`SELECT COUNT(*) FROM workflow_approval WHERE proposal_id IN ('prop-free', 'prop-used')`:      0,
		`SELECT COUNT(*) FROM workflow_definition WHERE id = 'unreferenced'`:                          0,
		`SELECT COUNT(*) FROM workflow_definition WHERE id = 'referenced'`:                            1,
		`SELECT COUNT(*) FROM workflow_run WHERE id = 'run-done'`:                                     1,
		`SELECT COUNT(*) FROM workflow_approval WHERE kind = 'routine' AND definition_id = 'routine'`: 1,
		`SELECT COUNT(*) FROM workflow_definition WHERE id = 'routine'`:                               1,
		`SELECT COUNT(*) FROM workflow_proposal WHERE id = 'prop-elsewhere'`:                          1,
		`SELECT COUNT(*) FROM workflow_approval WHERE proposal_id = 'prop-elsewhere'`:                 1,
		`SELECT COUNT(*) FROM workflow_definition WHERE id = 'routine-copy'`:                          1,
	} {
		if got := countRows(t, db, query); got != expect {
			t.Errorf("%s = %d, want %d", query, got, expect)
		}
	}
}

func TestErasureWithoutSessionsKeepsOtherOrphanedTurnRuns(t *testing.T) {
	db := newTestDB(t)
	ctx := t.Context()
	seedPersonaSession(t, db, "lobby/discord:owner/thread:7001", "discord:602", "lobby")
	doc := testMemoryDocument("mem-601", "lobby/discord:owner/thread:7001", store.MemoryKindEventText)
	doc.OwnerID = "discord:601"
	if _, err := store.NewMemoryStore(db).UpsertDocument(ctx, doc); err != nil {
		t.Fatalf("UpsertDocument: %v", err)
	}
	seedDurableRun(t, db, durableRunFixture{
		key:     "turn-other-orphan",
		handler: "turn",
		payload: `{"turn_id":"turn-other-orphan","session_id":"lobby/discord:owner/dm:999"}`,
		state:   "failed",
		actions: 1,
	})
	report, _, err := erasePrincipal(ctx, db, principal.ErasureScope{PrincipalID: "discord:601", Apply: true}, "")
	if err != nil {
		t.Fatalf("erasePrincipal: %v", err)
	}
	requireErasureReport(t, report, map[string]int{principal.ErasureLineMemoryDocuments: 1})
	if got := countRows(t, db, `SELECT COUNT(*) FROM durable_run WHERE key = 'turn-other-orphan'`); got != 1 {
		t.Fatalf("other orphaned turn run count = %d, want 1", got)
	}
}
