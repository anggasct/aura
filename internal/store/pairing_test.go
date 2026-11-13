package store

import (
	"encoding/json"
	"testing"
	"time"
)

func pairingIssue(code, principalID, account string, now time.Time) *PairingIssue {
	return &PairingIssue{
		CodeSHA256: code, PrincipalID: principalID, Account: account, Now: now,
		TTL: time.Hour, Cooldown: 10 * time.Minute, MaxPerAccount: 2,
	}
}

func TestPairingIssueEnforcesCooldownAndAccountCap(t *testing.T) {
	db := newTestDB(t)
	ctx := t.Context()
	pairings := NewPairingStore(db)
	now := time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)
	steps := []struct {
		name  string
		issue *PairingIssue
		want  string
	}{
		{"first request", pairingIssue("c1", "discord:1", "office", now), PairingIssued},
		{"again inside the cooldown", pairingIssue("c1b", "discord:1", "office", now.Add(5*time.Minute)), PairingPending},
		{"second principal", pairingIssue("c2", "discord:2", "office", now.Add(time.Minute)), PairingIssued},
		{"cap reached", pairingIssue("c3", "discord:3", "office", now.Add(2*time.Minute)), PairingPending},
		{"other account has room", pairingIssue("c4", "discord:3", "home", now.Add(2*time.Minute)), PairingIssued},
		{"after the cooldown replaces the old code", pairingIssue("c1c", "discord:1", "office", now.Add(11*time.Minute)), PairingIssued},
		{"expired codes free the cap", pairingIssue("c5", "discord:3", "office", now.Add(62*time.Minute)), PairingIssued},
	}
	for _, step := range steps {
		got, err := pairings.Issue(ctx, step.issue)
		if err != nil || got != step.want {
			t.Fatalf("%s: Issue() = %q, %v; want %q", step.name, got, err, step.want)
		}
	}
	pending, err := pairings.Pending(ctx, now.Add(62*time.Minute))
	if err != nil {
		t.Fatalf("Pending: %v", err)
	}
	got := map[string]bool{}
	for _, request := range pending {
		got[request.Account+"/"+request.PrincipalID] = true
	}
	if len(pending) != 2 || !got["office/discord:1"] || !got["office/discord:3"] {
		t.Fatalf("pending = %+v, want discord:1 (reissued) and discord:3 on office", pending)
	}
	var codes int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM pairing_request WHERE principal_id = 'discord:1'`).Scan(&codes); err != nil || codes != 1 {
		t.Fatalf("discord:1 rows = %d, %v; want the old code dropped", codes, err)
	}
	if _, err := pairings.Issue(ctx, &PairingIssue{PrincipalID: "discord:1"}); err == nil {
		t.Fatal("incomplete issue accepted")
	}
}

func TestPairingApproveGrantsOnceAndReactivatesRevokedGrants(t *testing.T) {
	db := newTestDB(t)
	ctx := t.Context()
	pairings := NewPairingStore(db)
	grants := NewPrincipalGrantStore(db)
	now := time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)
	if _, err := pairings.Issue(ctx, pairingIssue("code-a", "discord:1", "office", now)); err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if _, found, err := pairings.Approve(ctx, "discord", "wrong", "local-owner", now); err != nil || found {
		t.Fatalf("wrong code Approve() found = %v, %v", found, err)
	}
	request, found, err := pairings.Approve(ctx, "discord", "code-a", "local-owner", now.Add(time.Minute))
	if err != nil || !found || request.PrincipalID != "discord:1" || request.Account != "office" {
		t.Fatalf("Approve() = %+v, %v, %v", request, found, err)
	}
	if _, found, _ := pairings.Approve(ctx, "discord", "code-a", "local-owner", now.Add(2*time.Minute)); found {
		t.Fatal("code approved twice")
	}
	if active, err := grants.Active(ctx, "discord:1", "office"); err != nil || !active {
		t.Fatalf("Active() = %v, %v", active, err)
	}
	if active, _ := grants.Active(ctx, "discord:1", "home"); active {
		t.Fatal("grant leaked to another account")
	}
	accounts, err := grants.Revoke(ctx, "discord:1", now.Add(3*time.Minute))
	if err != nil || len(accounts) != 1 || accounts[0] != "office" {
		t.Fatalf("Revoke() = %v, %v", accounts, err)
	}
	if active, _ := grants.Active(ctx, "discord:1", "office"); active {
		t.Fatal("revoked grant still active")
	}
	if accounts, _ := grants.Revoke(ctx, "discord:1", now.Add(4*time.Minute)); len(accounts) != 0 {
		t.Fatalf("second Revoke() = %v, want nothing", accounts)
	}
	if _, err := pairings.Issue(ctx, pairingIssue("code-b", "discord:1", "office", now.Add(20*time.Minute))); err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if _, found, _ := pairings.Approve(ctx, "discord", "code-b", "local-owner", now.Add(21*time.Minute)); !found {
		t.Fatal("re-approval failed")
	}
	list, err := grants.List(ctx)
	if err != nil || len(list) != 1 || list[0].PrincipalID != "discord:1" || list[0].Class != "trusted" {
		t.Fatalf("List() = %+v, %v", list, err)
	}
}

func TestPairingExpiredCodeIsNotApproved(t *testing.T) {
	db := newTestDB(t)
	pairings := NewPairingStore(db)
	now := time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)
	if _, err := pairings.Issue(t.Context(), pairingIssue("code-x", "discord:1", "office", now)); err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if _, found, _ := pairings.Approve(t.Context(), "discord", "code-x", "local-owner", now.Add(time.Hour)); found {
		t.Fatal("expired code approved")
	}
	if cleared, err := pairings.ClearPending(t.Context()); err != nil || cleared != 1 {
		t.Fatalf("ClearPending() = %d, %v", cleared, err)
	}
}

func TestPairingFailedApprovalsReadOwnerActions(t *testing.T) {
	db := newTestDB(t)
	ctx := t.Context()
	seedPersonaSession(t, db, "profile-local-owner", "local-owner", "main")
	events := NewEventStore(db)
	base := time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)
	for i, entry := range []struct {
		action, channel string
		at              time.Time
	}{
		{AccessActionPairingInvalid, "discord", base},
		{AccessActionPairingInvalid, "discord", base.Add(time.Minute)},
		{AccessActionPairingInvalid, "telegram", base.Add(2 * time.Minute)},
		{"principal_grant", "discord", base.Add(3 * time.Minute)},
		{AccessActionPairingInvalid, "discord", base.Add(-3 * time.Hour)},
	} {
		payload, err := json.Marshal(map[string]string{"action": entry.action, "channel": entry.channel})
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if _, _, err := events.UpsertEvent(ctx, &RuntimeEvent{
			ID: "ev-" + string(rune('a'+i)), SessionID: "profile-local-owner", TurnID: "owner-action", InvocationID: "owner-action",
			Author: "local-owner", Kind: AccessOwnerActionEventKind, SchemaVersion: 1, Payload: payload, CreatedAt: entry.at,
		}); err != nil {
			t.Fatalf("UpsertEvent: %v", err)
		}
	}
	failures, err := NewPairingStore(db).FailedApprovals(ctx, "discord", base.Add(-time.Hour))
	if err != nil || len(failures) != 2 || !failures[0].Equal(base) || !failures[1].Equal(base.Add(time.Minute)) {
		t.Fatalf("FailedApprovals() = %v, %v", failures, err)
	}
}

func TestListPrincipalActivityAggregatesSessions(t *testing.T) {
	db := newTestDB(t)
	seedPersonaSession(t, db, "kantor/discord:office/dm:1", "discord:1", "kantor")
	seedPersonaSession(t, db, "kantor/discord:office/channel:9", "discord:1", "kantor")
	seedPersonaSession(t, db, "main/discord:office/dm:777", "local-owner", "main")
	activity, err := ListPrincipalActivity(t.Context(), db)
	if err != nil || len(activity) != 1 || activity[0].PrincipalID != "discord:1" || activity[0].Sessions != 2 || activity[0].LastActive.IsZero() {
		t.Fatalf("ListPrincipalActivity() = %+v, %v", activity, err)
	}
}

func TestPairingApproveNeverConsumesAnotherChannelsCode(t *testing.T) {
	db := newTestDB(t)
	ctx := t.Context()
	pairings := NewPairingStore(db)
	grants := NewPrincipalGrantStore(db)
	now := time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)
	if _, err := pairings.Issue(ctx, pairingIssue("code-t", "telegram:7", "pocket", now)); err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if _, found, err := pairings.Approve(ctx, "discord", "code-t", "local-owner", now.Add(time.Minute)); err != nil || found {
		t.Fatalf("cross-channel Approve() found = %v, %v; want not found", found, err)
	}
	if active, _ := grants.Active(ctx, "telegram:7", "pocket"); active {
		t.Fatal("a cross-channel approval granted access")
	}
	var pending int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM pairing_request WHERE code_sha256 = 'code-t'`).Scan(&pending); err != nil || pending != 1 {
		t.Fatalf("pending rows = %d, %v; want the code kept for its own channel", pending, err)
	}
	request, found, err := pairings.Approve(ctx, "telegram", "code-t", "local-owner", now.Add(2*time.Minute))
	if err != nil || !found || request.PrincipalID != "telegram:7" {
		t.Fatalf("Approve() = %+v, %v, %v", request, found, err)
	}
	if active, _ := grants.Active(ctx, "telegram:7", "pocket"); !active {
		t.Fatal("approval did not grant access")
	}
	if _, _, err := pairings.Approve(ctx, "", "code-t", "local-owner", now); err == nil {
		t.Fatal("approval without a channel accepted")
	}
}

func TestRunSessionsAreNotPrincipalActivity(t *testing.T) {
	db := newTestDB(t)
	seedPersonaSession(t, db, "wf-run-only", "discord:2", "kantor")
	activity, err := ListPrincipalActivity(t.Context(), db)
	if err != nil || len(activity) != 0 {
		t.Fatalf("ListPrincipalActivity() with only a run session = %+v, %v; want none", activity, err)
	}
	seedPersonaSession(t, db, "kantor/discord:office/dm:1", "discord:1", "kantor")
	if _, err := db.ExecContext(t.Context(), `UPDATE session SET updated_at = '2026-11-01T09:00:00Z' WHERE id = 'kantor/discord:office/dm:1'`); err != nil {
		t.Fatalf("age the chat session: %v", err)
	}
	seedPersonaSession(t, db, "wf-run-later", "discord:1", "kantor")
	activity, err = ListPrincipalActivity(t.Context(), db)
	if err != nil || len(activity) != 1 {
		t.Fatalf("ListPrincipalActivity() = %+v, %v; want one principal", activity, err)
	}
	if activity[0].Sessions != 1 || activity[0].LastActive.Format("2006-01-02") != "2026-11-01" {
		t.Fatalf("activity = %+v, want the chat session only, last active 2026-11-01", activity[0])
	}
}
