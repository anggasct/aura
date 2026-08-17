package effect

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestApprovalTokenIsBoundAndConsumed(t *testing.T) {
	t.Parallel()
	j, db := newTestJournal(t)
	intent := mustPrepare(t, j, validPrepare(1))
	mustStart(t, j, intent.ID)
	intent = mustMarkUnknown(t, j, intent.ID)

	approval, err := j.Approve(context.Background(), &ApprovalRequest{
		IntentID:  intent.ID,
		Action:    ApprovalActionMarkSucceeded,
		Reason:    "provider receipt verified by owner",
		ExpiresIn: time.Minute,
	})
	if err != nil {
		t.Fatalf("approve: %v", err)
	}
	if approval.Token == "" || hashToken(approval.Token) == approval.Token {
		t.Fatal("approval token was not issued as opaque plaintext distinct from its stored digest")
	}
	var storedHash string
	if err := db.QueryRowContext(context.Background(), `SELECT token_hash FROM effect_approval WHERE id = ?`, approval.ID).Scan(&storedHash); err != nil {
		t.Fatalf("read token hash: %v", err)
	}
	if storedHash != hashToken(approval.Token) {
		t.Fatal("stored token hash does not match issued token")
	}

	resolved, err := j.MarkWithApproval(context.Background(), intent.ID, true, approval.Reason, approval.Token)
	if err != nil {
		t.Fatalf("mark with approval: %v", err)
	}
	if resolved.State != StateSucceeded {
		t.Fatalf("state = %s, want succeeded", resolved.State)
	}
	_, err = j.MarkWithApproval(context.Background(), intent.ID, true, approval.Reason, approval.Token)
	assertCode(t, err, ErrorCodeApprovalConsumed)
}

func TestApprovalUsesDefaultExpiry(t *testing.T) {
	t.Parallel()
	j, _ := newTestJournal(t)
	intent := mustPrepare(t, j, validPrepare(1))
	mustStart(t, j, intent.ID)
	mustMarkUnknown(t, j, intent.ID)

	approval, err := j.Approve(context.Background(), &ApprovalRequest{
		IntentID: intent.ID,
		Action:   ApprovalActionMarkSucceeded,
		Reason:   "default expiry test",
	})
	if err != nil {
		t.Fatalf("approve: %v", err)
	}
	want := time.Date(2026, 8, 12, 9, 5, 0, 0, time.UTC)
	if !approval.ExpiresAt.Equal(want) {
		t.Fatalf("expires_at = %s, want %s", approval.ExpiresAt, want)
	}
}

func TestApprovalReasonAndActionAreBound(t *testing.T) {
	t.Parallel()
	j, _ := newTestJournal(t)
	intent := mustPrepare(t, j, validPrepare(1))
	mustStart(t, j, intent.ID)
	mustMarkUnknown(t, j, intent.ID)
	approval, err := j.Approve(context.Background(), &ApprovalRequest{
		IntentID:  intent.ID,
		Action:    ApprovalActionMarkFailed,
		Reason:    "owner reviewed provider response",
		ExpiresIn: time.Minute,
	})
	if err != nil {
		t.Fatalf("approve: %v", err)
	}
	_, err = j.MarkWithApproval(context.Background(), intent.ID, false, "mutated reason", approval.Token)
	assertCode(t, err, ErrorCodeApprovalInvalid)
	_, err = j.MarkWithApproval(context.Background(), intent.ID, true, approval.Reason, approval.Token)
	assertCode(t, err, ErrorCodeApprovalInvalid)
	if _, err := j.MarkWithApproval(context.Background(), intent.ID, false, approval.Reason, approval.Token); err != nil {
		t.Fatalf("mark with matching approval: %v", err)
	}
}

func TestApprovalExpiryIsEnforced(t *testing.T) {
	t.Parallel()
	j, db := newTestJournal(t)
	intent := mustPrepare(t, j, validPrepare(1))
	mustStart(t, j, intent.ID)
	mustMarkUnknown(t, j, intent.ID)
	approval, err := j.Approve(context.Background(), &ApprovalRequest{
		IntentID:  intent.ID,
		Action:    ApprovalActionMarkFailed,
		Reason:    "expired test",
		ExpiresIn: time.Minute,
	})
	if err != nil {
		t.Fatalf("approve: %v", err)
	}
	if _, err := db.ExecContext(context.Background(), `UPDATE effect_approval SET expires_at = ? WHERE id = ?`, "2026-08-12T08:59:59Z", approval.ID); err != nil {
		t.Fatalf("expire approval: %v", err)
	}
	_, err = j.MarkWithApproval(context.Background(), intent.ID, false, approval.Reason, approval.Token)
	assertCode(t, err, ErrorCodeApprovalExpired)
}

func TestApprovalConsumptionIsExactlyOnce(t *testing.T) {
	t.Parallel()
	j, _ := newTestJournal(t)
	intent := mustPrepare(t, j, validPrepare(1))
	mustStart(t, j, intent.ID)
	mustMarkUnknown(t, j, intent.ID)
	approval, err := j.Approve(context.Background(), &ApprovalRequest{
		IntentID:  intent.ID,
		Action:    ApprovalActionMarkFailed,
		Reason:    "one-shot concurrency test",
		ExpiresIn: time.Minute,
	})
	if err != nil {
		t.Fatalf("approve: %v", err)
	}

	const workers = 16
	var succeeded int64
	var wg sync.WaitGroup
	start := make(chan struct{})
	wg.Add(workers)
	for range workers {
		go func() {
			defer wg.Done()
			<-start
			if _, err := j.MarkWithApproval(context.Background(), intent.ID, false, approval.Reason, approval.Token); err == nil {
				atomic.AddInt64(&succeeded, 1)
			}
		}()
	}
	close(start)
	wg.Wait()
	if got := atomic.LoadInt64(&succeeded); got != 1 {
		t.Fatalf("successful approval consumers = %d, want 1", got)
	}
}

func TestRetryApprovalCreatesLinkedIntentAndEvent(t *testing.T) {
	t.Parallel()
	j, db := newTestJournal(t)
	intent := mustPrepare(t, j, validPrepare(1))
	mustStart(t, j, intent.ID)
	mustMarkUnknown(t, j, intent.ID)
	approval, err := j.Approve(context.Background(), &ApprovalRequest{
		IntentID:  intent.ID,
		Action:    ApprovalActionRetry,
		Reason:    "owner approved a fresh attempt",
		ExpiresIn: time.Minute,
	})
	if err != nil {
		t.Fatalf("approve: %v", err)
	}
	retry, err := j.RetryWithApproval(context.Background(), intent.ID, approval.Token)
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if retry.State != StatePrepared || retry.RetryOf != intent.ID {
		t.Fatalf("retry = %+v", retry)
	}
	if retry.IdempotencyKey == intent.IdempotencyKey {
		t.Fatal("retry reused the original idempotency key")
	}
	if string(retry.RequestJSON) != string(intent.RequestJSON) {
		t.Fatalf("retry request = %s, want canonical original %s", retry.RequestJSON, intent.RequestJSON)
	}
	original, err := j.Get(context.Background(), intent.ID)
	if err != nil {
		t.Fatalf("get original: %v", err)
	}
	if original.State != StateUnknown {
		t.Fatalf("original state = %s, want unknown", original.State)
	}
	if got := eventCount(t, db, EventKindToolRequested); got != 2 {
		t.Fatalf("tool.requested events = %d, want 2", got)
	}
	var consumed string
	if err := db.QueryRowContext(context.Background(), `SELECT consumed_at FROM effect_approval WHERE id = ?`, approval.ID).Scan(&consumed); err != nil {
		t.Fatalf("read consumed approval: %v", err)
	}
	if consumed == "" {
		t.Fatal("retry approval was not consumed")
	}
}

func TestRetryRejectsUnsafeStoredRequest(t *testing.T) {
	t.Parallel()
	j, db := newTestJournal(t)
	intent := mustPrepare(t, j, validPrepare(1))
	mustStart(t, j, intent.ID)
	mustMarkUnknown(t, j, intent.ID)
	approval, err := j.Approve(context.Background(), &ApprovalRequest{
		IntentID:  intent.ID,
		Action:    ApprovalActionRetry,
		Reason:    "legacy request safety test",
		ExpiresIn: time.Minute,
	})
	if err != nil {
		t.Fatalf("approve: %v", err)
	}
	if _, err := db.ExecContext(context.Background(), `UPDATE effect_intent SET request_json = ? WHERE id = ?`, `{"token":"secret-canary"}`, intent.ID); err != nil {
		t.Fatalf("seed unsafe request: %v", err)
	}
	_, err = j.RetryWithApproval(context.Background(), intent.ID, approval.Token)
	assertCode(t, err, ErrorCodeRequestUnsafe)
	if got := intentCount(t, db, "retry_of = ?", intent.ID); got != 0 {
		t.Fatalf("unsafe retry persisted %d linked intents", got)
	}
}

func TestRetryRejectsChangedStoredRequest(t *testing.T) {
	t.Parallel()
	j, db := newTestJournal(t)
	intent := mustPrepare(t, j, validPrepare(1))
	mustStart(t, j, intent.ID)
	mustMarkUnknown(t, j, intent.ID)
	approval, err := j.Approve(context.Background(), &ApprovalRequest{
		IntentID:  intent.ID,
		Action:    ApprovalActionRetry,
		Reason:    "request digest binding test",
		ExpiresIn: time.Minute,
	})
	if err != nil {
		t.Fatalf("approve: %v", err)
	}
	if _, err := db.ExecContext(context.Background(), `UPDATE effect_intent SET request_json = ? WHERE id = ?`, `{"chat":"@a","text":"changed"}`, intent.ID); err != nil {
		t.Fatalf("seed changed request: %v", err)
	}
	_, err = j.RetryWithApproval(context.Background(), intent.ID, approval.Token)
	assertCode(t, err, ErrorCodeApprovalInvalid)
	if got := intentCount(t, db, "retry_of = ?", intent.ID); got != 0 {
		t.Fatalf("changed retry persisted %d linked intents", got)
	}
}

func TestRetentionPreservesAuditAndUnknown(t *testing.T) {
	t.Parallel()
	j, db := newTestJournal(t)
	old := "2026-07-01T00:00:00Z"

	pruned := mustPrepare(t, j, validPrepare(1))
	mustStart(t, j, pruned.ID)
	if _, err := j.Succeed(context.Background(), pruned.ID, json.RawMessage(`{"id":"done"}`)); err != nil {
		t.Fatalf("succeed: %v", err)
	}

	audited := mustPrepare(t, j, validPrepare(2))
	mustStart(t, j, audited.ID)
	mustMarkUnknown(t, j, audited.ID)
	approval, err := j.Approve(context.Background(), &ApprovalRequest{
		IntentID:  audited.ID,
		Action:    ApprovalActionMarkFailed,
		Reason:    "audit must survive pruning",
		ExpiresIn: time.Minute,
	})
	if err != nil {
		t.Fatalf("approve: %v", err)
	}
	if _, err := j.MarkWithApproval(context.Background(), audited.ID, false, approval.Reason, approval.Token); err != nil {
		t.Fatalf("mark audited: %v", err)
	}

	unknown := mustPrepare(t, j, validPrepare(3))
	mustStart(t, j, unknown.ID)
	mustMarkUnknown(t, j, unknown.ID)
	if _, err := db.ExecContext(context.Background(), `UPDATE effect_intent SET finished_at = ?, updated_at = ? WHERE id IN (?, ?)`, old, old, pruned.ID, audited.ID); err != nil {
		t.Fatalf("age terminal intents: %v", err)
	}
	report, err := j.Prune(context.Background(), time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if report.Deleted != 1 || report.PreservedAudits != 1 {
		t.Fatalf("report = %+v, want deleted=1 preserved=1", report)
	}
	if got := intentCount(t, db, "id = ?", pruned.ID); got != 0 {
		t.Fatalf("pruned intent rows = %d, want 0", got)
	}
	if got := intentCount(t, db, "id = ?", audited.ID); got != 1 {
		t.Fatalf("audited intent rows = %d, want 1", got)
	}
	if got := intentCount(t, db, "id = ? AND state = ?", unknown.ID, string(StateUnknown)); got != 1 {
		t.Fatalf("unknown intent rows = %d, want 1", got)
	}
}

func TestStatusReportsStatesAndOldestUpdate(t *testing.T) {
	t.Parallel()
	j, _ := newTestJournal(t)
	prepared := mustPrepare(t, j, validPrepare(1))
	started := mustPrepare(t, j, validPrepare(2))
	mustStart(t, j, started.ID)
	unknown := mustPrepare(t, j, validPrepare(3))
	mustStart(t, j, unknown.ID)
	mustMarkUnknown(t, j, unknown.ID)

	status, err := j.Status(context.Background())
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if status.Count(StatePrepared) != 1 || status.Count(StateStarted) != 1 || status.Count(StateUnknown) != 1 {
		t.Fatalf("counts = %+v", status.Counts)
	}
	if status.OldestByState[StatePrepared].IsZero() {
		t.Fatal("prepared oldest timestamp is missing")
	}
	_ = prepared
}

func mustMarkUnknown(t *testing.T, j *Journal, id string) *Intent {
	t.Helper()
	intent, err := j.MarkUnknown(context.Background(), id)
	if err != nil {
		t.Fatalf("mark unknown: %v", err)
	}
	return intent
}

func TestApprovalReasonDoesNotLeakIntoIntentPayload(t *testing.T) {
	t.Parallel()
	j, db := newTestJournal(t)
	intent := mustPrepare(t, j, validPrepare(1))
	mustStart(t, j, intent.ID)
	mustMarkUnknown(t, j, intent.ID)
	reason := "owner reason with no request content"
	approval, err := j.Approve(context.Background(), &ApprovalRequest{IntentID: intent.ID, Action: ApprovalActionMarkFailed, Reason: reason, ExpiresIn: time.Minute})
	if err != nil {
		t.Fatalf("approve: %v", err)
	}
	if _, err := j.MarkWithApproval(context.Background(), intent.ID, false, reason, approval.Token); err != nil {
		t.Fatalf("mark: %v", err)
	}
	var request string
	if err := db.QueryRowContext(context.Background(), `SELECT request_json FROM effect_intent WHERE id = ?`, intent.ID).Scan(&request); err != nil {
		t.Fatalf("read request: %v", err)
	}
	if strings.Contains(request, reason) {
		t.Fatal("approval reason leaked into request JSON")
	}
}
