package store

import (
	"database/sql"
	"slices"
	"strings"
	"testing"
)

const erasureWorkflowStamp = "2026-11-12T09:00:00Z"

type seededRun struct {
	id, definition, principal, status, durableKey, parent, originSession string
	steps                                                                []string
}

func seedWorkflowDefinition(t *testing.T, db *sql.DB, id string) {
	t.Helper()
	if _, err := db.ExecContext(t.Context(), `INSERT INTO workflow_definition (id, version, goal, source, spec_json, spec_sha256, created_at)
		VALUES (?, 1, 'goal', 'composed', '{}', 'digest', ?)`, id, erasureWorkflowStamp); err != nil {
		t.Fatalf("seed definition %s: %v", id, err)
	}
}

func seedWorkflowRun(t *testing.T, db *sql.DB, run *seededRun) {
	t.Helper()
	if _, err := db.ExecContext(t.Context(), `INSERT INTO workflow_run (id, definition_id, definition_version, durable_key, goal, input_json, status, created_at, updated_at, principal_id, principal_class, agent_id, origin_session_id, reply_account, reply_channel_id, parent_run_id)
		VALUES (?1, ?2, 1, NULLIF(?3, ''), 'goal', '{"objective":"private"}', ?4, ?5, ?5, ?6, 'trusted', 'main', NULLIF(?7, ''), 'acct', 'chan', NULLIF(?8, ''))`,
		run.id, run.definition, run.durableKey, run.status, erasureWorkflowStamp, run.principal, run.originSession, run.parent); err != nil {
		t.Fatalf("seed run %s: %v", run.id, err)
	}
	for _, step := range run.steps {
		if _, err := db.ExecContext(t.Context(), `INSERT INTO workflow_step_run (run_id, step_id, status, attempt, updated_at) VALUES (?, ?, 'succeeded', 1, ?)`,
			run.id, step, erasureWorkflowStamp); err != nil {
			t.Fatalf("seed step %s/%s: %v", run.id, step, err)
		}
	}
}

func seedProposalApproval(t *testing.T, db *sql.DB, proposal, session, definition string) {
	t.Helper()
	ctx := t.Context()
	if _, err := db.ExecContext(ctx, `INSERT INTO workflow_proposal (id, session_id, agent_id, principal_id, spec_json, spec_sha256, status, created_at)
		VALUES (?, ?, 'main', 'local-owner', '{}', 'digest', 'approved', ?)`, proposal, session, erasureWorkflowStamp); err != nil {
		t.Fatalf("seed proposal %s: %v", proposal, err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO workflow_approval (definition_id, definition_version, spec_sha256, kind, agent_id, approver_id, proposal_id, approved_at)
		VALUES (?, 1, 'digest', 'proposal', 'main', 'local-owner', ?, ?)`, definition, proposal, erasureWorkflowStamp); err != nil {
		t.Fatalf("seed proposal approval %s: %v", proposal, err)
	}
}

func seedRunEvidence(t *testing.T, db *sql.DB, run *seededRun) {
	t.Helper()
	seedDurableRun(t, db, durableRunFixture{key: run.durableKey, handler: "workflow", payload: `{}`, state: "succeeded", actions: 1})
	if _, err := db.ExecContext(t.Context(), `INSERT INTO durable_signal (run_key, name, seq, payload, created_at) VALUES (?, 'approval', 0, ?, ?)`,
		run.durableKey, []byte(`{}`), erasureWorkflowStamp); err != nil {
		t.Fatalf("seed signal: %v", err)
	}
}

func seedLegacyStepSession(t *testing.T, db *sql.DB, runID, stepID string) string {
	t.Helper()
	id := legacyStepSessionID(runID, stepID)
	if _, err := db.ExecContext(t.Context(), `INSERT INTO session (id, owner_id, metadata_json, created_at, updated_at) VALUES (?, 'workflow', '{}', ?, ?)`,
		id, erasureWorkflowStamp, erasureWorkflowStamp); err != nil {
		t.Fatalf("seed legacy session: %v", err)
	}
	seedTurnEvent(t, db, turnEventFixture{session: id, turn: "turn-" + id, kind: "turn.accepted"})
	seedEmailEffect(t, db, "effect-"+id, id, "builtin-tools", "write_file", `{"arguments":{}}`)
	if _, err := db.ExecContext(t.Context(), `INSERT INTO effect_approval (id, intent_id, action, request_digest, owner_id, reason, token_hash, issued_at, expires_at)
		VALUES (?, ?, 'retry', 'digest', 'local-owner', 'r', ?, ?, ?)`, "approval-"+id, "effect-"+id, "token-"+id, erasureWorkflowStamp, erasureWorkflowStamp); err != nil {
		t.Fatalf("seed effect approval: %v", err)
	}
	return id
}

func workflowDataRows(t *testing.T, db *sql.DB) map[string]int {
	t.Helper()
	rows := map[string]int{}
	for _, table := range []string{"session", "runtime_event", "effect_intent", "workflow_run", "workflow_step_run", "workflow_proposal", "workflow_approval", "workflow_definition", "durable_run", "durable_action", "durable_signal"} {
		rows[table] = countRows(t, db, `SELECT COUNT(*) FROM `+table)
	}
	return rows
}

func requireStoreErasureCode(t *testing.T, err error, want ErrorCode) {
	t.Helper()
	if code, ok := CodeOf(err); !ok || code != want {
		t.Fatalf("error = %v, want code %s", err, want)
	}
}

func TestErasureRefusesWhileARunItTouchesIsLive(t *testing.T) {
	const (
		principal = "discord:601"
		thread    = "lobby/discord:owner/thread:7001"
	)
	cases := []struct {
		name     string
		seed     func(t *testing.T, db *sql.DB)
		wantLive []string
	}{
		{name: "a running root of the principal", wantLive: []string{"run-root"}, seed: func(t *testing.T, db *sql.DB) {
			seedWorkflowDefinition(t, db, "own")
			seedDurableRun(t, db, durableRunFixture{key: "dur-root", handler: "workflow", payload: `{}`, state: "running", actions: 1})
			seedWorkflowRun(t, db, &seededRun{id: "run-root", definition: "own", principal: principal, status: "running", durableKey: "dur-root", steps: []string{"a"}})
		}},
		{name: "a child of a live root is not counted on its own", wantLive: []string{"run-root"}, seed: func(t *testing.T, db *sql.DB) {
			seedWorkflowDefinition(t, db, "own")
			seedDurableRun(t, db, durableRunFixture{key: "dur-root", handler: "workflow", payload: `{}`, state: "running"})
			seedWorkflowRun(t, db, &seededRun{id: "run-root", definition: "own", principal: principal, status: "running", durableKey: "dur-root"})
			seedWorkflowRun(t, db, &seededRun{id: "run-child", definition: "own", principal: principal, status: "running", parent: "run-root"})
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
			seedWorkflowRun(t, db, &seededRun{id: "run-root", definition: "own", principal: principal, status: "running", durableKey: "dur-root"})
		}},
		{name: "a terminal status over a running durable run is live", wantLive: []string{"run-root"}, seed: func(t *testing.T, db *sql.DB) {
			seedWorkflowDefinition(t, db, "own")
			seedDurableRun(t, db, durableRunFixture{key: "dur-root", handler: "workflow", payload: `{}`, state: "running"})
			seedWorkflowRun(t, db, &seededRun{id: "run-root", definition: "own", principal: principal, status: "succeeded", durableKey: "dur-root"})
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db := newTestDB(t)
			seedPersonaSession(t, db, thread, principal, "lobby")
			seedPersonaSession(t, db, "lobby/discord:owner/dm:601", principal, "lobby")
			tc.seed(t, db)
			before := workflowDataRows(t, db)

			live, err := LiveWorkflowRuns(t.Context(), db, principal)
			if err != nil || !slices.Equal(live, tc.wantLive) {
				t.Fatalf("LiveWorkflowRuns = %v, %v; want %v", live, err, tc.wantLive)
			}
			dry, err := ErasePrincipal(t.Context(), db, principal, false, ErasureOptions{})
			if err != nil || dry.LiveWorkflowRuns != len(tc.wantLive) {
				t.Fatalf("dry run = %+v, %v; want %d live runs", dry, err, len(tc.wantLive))
			}
			if len(tc.wantLive) == 0 {
				return
			}
			counts, err := ErasePrincipal(t.Context(), db, principal, true, ErasureOptions{})
			requireStoreErasureCode(t, err, ErrorCodeErasureBlocked)
			if !strings.Contains(err.Error(), tc.wantLive[0]) || counts.LiveWorkflowRuns != len(tc.wantLive) {
				t.Fatalf("refusal = %v, counts %+v; want it to name %v", err, counts, tc.wantLive)
			}
			after := workflowDataRows(t, db)
			for table, rows := range before {
				if after[table] != rows {
					t.Errorf("after a refused erasure %s has %d rows, want %d", table, after[table], rows)
				}
			}
			if countRows(t, db, `SELECT COUNT(*) FROM session WHERE owner_id = ?`, principal) != 2 {
				t.Error("a refused erasure deleted sessions")
			}
		})
	}
}

func TestErasureReconcilesStaleRunRows(t *testing.T) {
	const principal = "discord:601"
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
			seedPersonaSession(t, db, "lobby/discord:owner/dm:601", principal, "lobby")
			seedWorkflowDefinition(t, db, "own")
			key := ""
			if tc.durable != "" {
				key = "dur-root"
				seedDurableRun(t, db, durableRunFixture{key: key, handler: "workflow", payload: `{}`, state: tc.durable})
			}
			seedWorkflowRun(t, db, &seededRun{id: "run-root", definition: "own", principal: principal, status: tc.status, durableKey: key})
			seedWorkflowRun(t, db, &seededRun{id: "run-child", definition: "own", principal: principal, status: "running", parent: "run-root"})
			seedWorkflowRun(t, db, &seededRun{id: "run-child-done", definition: "own", principal: principal, status: "succeeded", parent: "run-root"})

			live, err := LiveWorkflowRuns(t.Context(), db, principal)
			if err != nil || len(live) != 0 {
				t.Fatalf("LiveWorkflowRuns = %v, %v; want none", live, err)
			}
			for run, want := range map[string]string{"run-root": tc.want, "run-child": tc.want, "run-child-done": "succeeded"} {
				if got := countRows(t, db, `SELECT COUNT(*) FROM workflow_run WHERE id = ? AND status = ?`, run, want); got != 1 {
					t.Errorf("run %s is not %s", run, want)
				}
			}
			counts, err := ErasePrincipal(t.Context(), db, principal, true, ErasureOptions{})
			if err != nil || counts.WorkflowRuns != 3 {
				t.Fatalf("erasure = %+v, %v; want it to remove the three settled runs", counts, err)
			}
		})
	}
}

func TestErasureDeletesThePrincipalsWorkflowRuns(t *testing.T) {
	const (
		principal    = "discord:601"
		other        = "discord:602"
		dm           = "lobby/discord:owner/dm:601"
		runSession   = "wf-run-1"
		otherSession = "lobby/discord:owner/dm:602"
	)
	db := newTestDB(t)
	seedErasureFixture(t, db, t.TempDir())
	seedPersonaSession(t, db, runSession, principal, "main")
	seedPersonaSession(t, db, otherSession, other, "lobby")
	seedWorkflowDefinition(t, db, "shared")
	seedWorkflowDefinition(t, db, "owners")
	run1 := &seededRun{id: "run-1", definition: "shared", principal: principal, status: "succeeded", durableKey: "dur-1", steps: []string{"fetch", "write"}}
	child1 := &seededRun{id: "run-1-child", definition: "shared", principal: principal, status: "failed", parent: "run-1", steps: []string{"fetch"}}
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

	want := ErasureCounts{Sessions: 3, MemoryDocuments: 2, Grants: 1, StandingGrants: 1, PairingRequests: 1, UsageLinks: 1, WorkflowRuns: 2, LegacyStepSessions: 3}
	dry, err := ErasePrincipal(t.Context(), db, principal, false, ErasureOptions{})
	if err != nil || dry != want {
		t.Fatalf("dry run = %+v, %v; want %+v", dry, err, want)
	}
	if countRows(t, db, `SELECT COUNT(*) FROM workflow_run`) != 4 {
		t.Fatal("dry run deleted workflow runs")
	}

	applied, err := ErasePrincipal(t.Context(), db, principal, true, ErasureOptions{})
	if err != nil || applied != want {
		t.Fatalf("apply = %+v, %v; want %+v", applied, err, want)
	}
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
		principal = "discord:111"
		thread    = "lobby/discord:owner/thread:9001"
		ownerDM   = "main/discord:owner/dm:777"
	)
	db := newTestDB(t)
	seedPersonaSession(t, db, thread, principal, "lobby")
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

	counts, err := ErasePrincipal(t.Context(), db, principal, true, ErasureOptions{})
	if err != nil {
		t.Fatalf("ErasePrincipal: %v", err)
	}
	if counts.WorkflowApprovals != 2 || counts.Sessions != 1 {
		t.Fatalf("counts = %+v, want 2 approvals and 1 session", counts)
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
