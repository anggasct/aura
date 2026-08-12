package effect

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/anggasct/aura/internal/store"
)

func TestPrepare_RecordsIntentAndRequestedEvent(t *testing.T) {
	t.Parallel()
	j, db := newTestJournal(t)

	intent := mustPrepare(t, j, validPrepare(1))

	if intent.State != StatePrepared {
		t.Fatalf("state = %s, want prepared", intent.State)
	}
	if intent.RequestDigest == "" {
		t.Fatal("request digest is empty")
	}
	if got := intentCount(t, db, "id = ?", intent.ID); got != 1 {
		t.Fatalf("intent rows = %d, want 1", got)
	}
	if got := eventCount(t, db, EventKindToolRequested); got != 1 {
		t.Fatalf("tool.requested events = %d, want 1", got)
	}
}

func TestPrepare_AtomicIntentAndEvent(t *testing.T) {
	t.Parallel()
	j, db := newTestJournal(t)

	mustPrepare(t, j, validPrepare(1))

	dup := validPrepare(2)
	dup.EventSequence = 1
	_, err := j.Prepare(context.Background(), dup)
	if err == nil {
		t.Fatal("expected event-sequence conflict error, got nil")
	}

	if got := intentCount(t, db, "idempotency_key = ?", dup.IdempotencyKey); got != 0 {
		t.Fatalf("rolled-back intent persisted: rows = %d, want 0", got)
	}
	if got := eventCount(t, db, EventKindToolRequested); got != 1 {
		t.Fatalf("events = %d, want 1 (only the first prepare)", got)
	}
}

func TestPrepare_IdempotentReplayReturnsExisting(t *testing.T) {
	t.Parallel()
	j, _ := newTestJournal(t)

	first := mustPrepare(t, j, validPrepare(1))
	req := validPrepare(1)
	req.EventID = "different-event-id"
	req.EventSequence = 99

	second, err := j.Prepare(context.Background(), req)
	if err != nil {
		t.Fatalf("idempotent replay: %v", err)
	}
	if second.ID != first.ID {
		t.Fatalf("replay returned %s, want existing %s", second.ID, first.ID)
	}
}

func TestPrepare_DifferentDigestIsConflict(t *testing.T) {
	t.Parallel()
	j, _ := newTestJournal(t)

	mustPrepare(t, j, validPrepare(1))

	req := validPrepare(1)
	req.Request = json.RawMessage(`{"chat":"@a","text":"different body"}`)
	_, err := j.Prepare(context.Background(), req)
	assertCode(t, err, ErrorCodeIdempotencyConflict)
}

func TestPrepare_InvalidClassification(t *testing.T) {
	t.Parallel()
	j, _ := newTestJournal(t)

	req := validPrepare(1)
	req.Classification = "magic"
	_, err := j.Prepare(context.Background(), req)
	assertCode(t, err, ErrorCodeClassificationMissing)
}

func TestPrepare_MissingClassification(t *testing.T) {
	t.Parallel()
	j, _ := newTestJournal(t)

	req := validPrepare(1)
	req.Classification = ""
	_, err := j.Prepare(context.Background(), req)
	assertCode(t, err, ErrorCodeClassificationMissing)
}

func TestPrepare_InvalidRequestJSON(t *testing.T) {
	t.Parallel()
	j, _ := newTestJournal(t)

	req := validPrepare(1)
	req.Request = json.RawMessage(`{not json`)
	_, err := j.Prepare(context.Background(), req)
	assertCode(t, err, ErrorCodeInvalidArgument)
}

func TestPrepare_NilPointerDB(t *testing.T) {
	t.Parallel()
	_, err := NewJournal(nil, Options{})
	assertCode(t, err, ErrorCodeInvalidArgument)
}

func TestPrepare_NilRequest(t *testing.T) {
	t.Parallel()
	j, _ := newTestJournal(t)
	_, err := j.Prepare(context.Background(), nil)
	assertCode(t, err, ErrorCodeInvalidArgument)
}

func TestStart_PersistsStartedBeforeReturn(t *testing.T) {
	t.Parallel()
	j, db := newTestJournal(t)

	intent := mustPrepare(t, j, validPrepare(1))
	started := mustStart(t, j, intent.ID)

	if started.State != StateStarted {
		t.Fatalf("returned state = %s, want started", started.State)
	}
	if started.StartedAt == nil {
		t.Fatal("started_at not set")
	}
	if got := intentCount(t, db, "id = ? AND state = ?", intent.ID, string(StateStarted)); got != 1 {
		t.Fatalf("durable started rows = %d, want 1", got)
	}
}

func TestTransition_FullHappyPath(t *testing.T) {
	t.Parallel()
	j, _ := newTestJournal(t)

	intent := mustPrepare(t, j, validPrepare(1))
	mustStart(t, j, intent.ID)
	succeeded, err := j.Succeed(context.Background(), intent.ID, json.RawMessage(`{"message_id":42}`))
	if err != nil {
		t.Fatalf("succeed: %v", err)
	}
	if succeeded.State != StateSucceeded {
		t.Fatalf("state = %s, want succeeded", succeeded.State)
	}
	if succeeded.ProviderReceipt == nil {
		t.Fatal("receipt not recorded")
	}
	if succeeded.FinishedAt == nil {
		t.Fatal("finished_at not set")
	}
}

func TestFail_FromPrepared(t *testing.T) {
	t.Parallel()
	j, _ := newTestJournal(t)

	intent := mustPrepare(t, j, validPrepare(1))
	failed, err := j.Fail(context.Background(), intent.ID, "policy_denied")
	if err != nil {
		t.Fatalf("fail from prepared: %v", err)
	}
	if failed.State != StateFailed || failed.SafeErrorCode != "policy_denied" {
		t.Fatalf("failed = %+v", failed)
	}
}

func TestFail_FromStarted(t *testing.T) {
	t.Parallel()
	j, _ := newTestJournal(t)

	intent := mustPrepare(t, j, validPrepare(1))
	mustStart(t, j, intent.ID)
	failed, err := j.Fail(context.Background(), intent.ID, "provider_4xx")
	if err != nil {
		t.Fatalf("fail from started: %v", err)
	}
	if failed.State != StateFailed {
		t.Fatalf("state = %s, want failed", failed.State)
	}
}

func TestMarkUnknown_FromStarted(t *testing.T) {
	t.Parallel()
	j, _ := newTestJournal(t)

	intent := mustPrepare(t, j, validPrepare(1))
	mustStart(t, j, intent.ID)
	unknown, err := j.MarkUnknown(context.Background(), intent.ID)
	if err != nil {
		t.Fatalf("mark unknown: %v", err)
	}
	if unknown.State != StateUnknown {
		t.Fatalf("state = %s, want unknown", unknown.State)
	}
}

func TestTransition_TerminalImmutable(t *testing.T) {
	t.Parallel()
	j, _ := newTestJournal(t)

	intent := mustPrepare(t, j, validPrepare(1))
	mustStart(t, j, intent.ID)
	if _, err := j.Succeed(context.Background(), intent.ID, json.RawMessage(`{}`)); err != nil {
		t.Fatalf("succeed: %v", err)
	}

	if _, err := j.Start(context.Background(), intent.ID); err == nil {
		t.Fatal("Start on succeeded must fail")
	}
	if _, err := j.Succeed(context.Background(), intent.ID, nil); err == nil {
		t.Fatal("Succeed on succeeded must fail")
	}
	if _, err := j.MarkUnknown(context.Background(), intent.ID); err == nil {
		t.Fatal("MarkUnknown on succeeded must fail")
	}
}

func TestTransition_StartRejectsNonPrepared(t *testing.T) {
	t.Parallel()
	j, _ := newTestJournal(t)

	intent := mustPrepare(t, j, validPrepare(1))
	mustStart(t, j, intent.ID)
	_, err := j.Start(context.Background(), intent.ID)
	assertCode(t, err, ErrorCodeTransitionInvalid)
}

func TestTransition_SucceedRejectsNonStarted(t *testing.T) {
	t.Parallel()
	j, _ := newTestJournal(t)

	intent := mustPrepare(t, j, validPrepare(1))
	_, err := j.Succeed(context.Background(), intent.ID, nil)
	assertCode(t, err, ErrorCodeTransitionInvalid)
}

func TestTransition_MarkUnknownRejectsNonStarted(t *testing.T) {
	t.Parallel()
	j, _ := newTestJournal(t)

	intent := mustPrepare(t, j, validPrepare(1))
	_, err := j.MarkUnknown(context.Background(), intent.ID)
	assertCode(t, err, ErrorCodeTransitionInvalid)
}

func TestTransition_NotFound(t *testing.T) {
	t.Parallel()
	j, _ := newTestJournal(t)

	_, err := j.Start(context.Background(), "nope")
	assertCode(t, err, ErrorCodeNotFound)
}

func TestGet_NotFound(t *testing.T) {
	t.Parallel()
	j, _ := newTestJournal(t)

	_, err := j.Get(context.Background(), "nope")
	assertCode(t, err, ErrorCodeNotFound)
}

func TestListByState_NegativeLimitRejected(t *testing.T) {
	t.Parallel()
	j, _ := newTestJournal(t)

	_, err := j.ListByState(context.Background(), StatePrepared, -1)
	assertCode(t, err, ErrorCodeInvalidArgument)
}

func TestClaim_ConcurrentExactlyOnce(t *testing.T) {
	t.Parallel()
	j, _ := newTestJournal(t)

	intent := mustPrepare(t, j, validPrepare(1))
	mustStart(t, j, intent.ID)

	const workers = 16
	var won int64
	var wg sync.WaitGroup
	wg.Add(workers)
	start := make(chan struct{})
	for range workers {
		go func() {
			defer wg.Done()
			<-start
			claimed, err := j.Claim(context.Background(), intent.ID)
			if err != nil {
				t.Errorf("claim: %v", err)
				return
			}
			if claimed {
				atomic.AddInt64(&won, 1)
			}
		}()
	}
	close(start)
	wg.Wait()

	if got := atomic.LoadInt64(&won); got != 1 {
		t.Fatalf("claims won = %d, want exactly 1", got)
	}
	final, err := j.Get(context.Background(), intent.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if final.State != StateUnknown {
		t.Fatalf("final state = %s, want unknown", final.State)
	}
}

func TestRecover_ConcurrentNoDoubleClaim(t *testing.T) {
	t.Parallel()
	j, db := newTestJournal(t)

	const intents = 40
	for n := 1; n <= intents; n++ {
		req := validPrepare(n)
		req.EventSequence = uint64(n)
		intent := mustPrepare(t, j, req)
		mustStart(t, j, intent.ID)
	}

	const workers = 8
	var totalClaimed int64
	var wg sync.WaitGroup
	wg.Add(workers)
	start := make(chan struct{})
	for range workers {
		go func() {
			defer wg.Done()
			<-start
			report, err := j.Recover(context.Background())
			if err != nil {
				t.Errorf("recover: %v", err)
				return
			}
			atomic.AddInt64(&totalClaimed, int64(report.Claimed))
		}()
	}
	close(start)
	wg.Wait()

	if got := atomic.LoadInt64(&totalClaimed); got != intents {
		t.Fatalf("total claimed = %d, want %d (double-claim or miss)", got, intents)
	}
	if got := intentCount(t, db, "state = ?", string(StateUnknown)); got != intents {
		t.Fatalf("unknown rows = %d, want %d", got, intents)
	}
	if got := intentCount(t, db, "state = ?", string(StateStarted)); got != 0 {
		t.Fatalf("started rows = %d, want 0", got)
	}
}

func TestRecover_LeavesOtherStatesAlone(t *testing.T) {
	t.Parallel()
	j, db := newTestJournal(t)

	preparedOnly := mustPrepare(t, j, validPrepare(1))

	started := mustPrepare(t, j, validPrepare(2))
	mustStart(t, j, started.ID)

	report, err := j.Recover(context.Background())
	if err != nil {
		t.Fatalf("recover: %v", err)
	}
	if report.Scanned != 1 || report.Claimed != 1 {
		t.Fatalf("report = %+v, want scanned=1 claimed=1", report)
	}
	if got := intentCount(t, db, "id = ? AND state = ?", preparedOnly.ID, string(StatePrepared)); got != 1 {
		t.Fatalf("prepared-only intent was touched: %d", got)
	}
}

func TestPrepare_ReplayDoesNotDoubleAppendEvent(t *testing.T) {
	t.Parallel()
	j, db := newTestJournal(t)

	mustPrepare(t, j, validPrepare(1))
	replay := validPrepare(1)
	if _, err := j.Prepare(context.Background(), replay); err != nil {
		t.Fatalf("replay: %v", err)
	}
	if got := eventCount(t, db, EventKindToolRequested); got != 1 {
		t.Fatalf("events after replay = %d, want 1", got)
	}
}

func TestSucceed_InvalidReceiptRejected(t *testing.T) {
	t.Parallel()
	j, _ := newTestJournal(t)

	intent := mustPrepare(t, j, validPrepare(1))
	mustStart(t, j, intent.ID)
	_, err := j.Succeed(context.Background(), intent.ID, json.RawMessage(`{bad`))
	assertCode(t, err, ErrorCodeInvalidArgument)
}

func TestPrepare_EventPayloadIsSafe(t *testing.T) {
	t.Parallel()
	j, db := newTestJournal(t)

	intent := mustPrepare(t, j, validPrepare(1))

	var payload []byte
	if err := db.QueryRowContext(context.Background(),
		`SELECT payload_json FROM runtime_event WHERE kind = ? AND session_id = ?`,
		EventKindToolRequested, intent.SessionID,
	).Scan(&payload); err != nil {
		t.Fatalf("read event payload: %v", err)
	}
	if bytes.Contains(payload, []byte("hi")) {
		t.Fatalf("event payload leaks request body: %s", payload)
	}
	var p toolRequestedPayload
	if err := json.Unmarshal(payload, &p); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	if p.EffectIntentID != intent.ID {
		t.Fatalf("payload effect_intent_id = %s, want %s", p.EffectIntentID, intent.ID)
	}
}

func TestStorePackageContract(t *testing.T) {
	t.Parallel()
	err := store.AppendEventTx(context.Background(), nil, &store.RuntimeEvent{})
	if err == nil {
		t.Fatal("expected nil-tx error")
	}
}

func TestCodeOf_Unwrapped(t *testing.T) {
	t.Parallel()
	if _, ok := CodeOf(errors.New("plain")); ok {
		t.Fatal("plain error must not yield a code")
	}
}

func TestPrepare_ConcurrentSameKeyReturnsSameIntent(t *testing.T) {
	t.Parallel()
	j, db := newTestJournal(t)

	const workers = 16
	req := validPrepare(1)
	var wg sync.WaitGroup
	wg.Add(workers)
	start := make(chan struct{})
	results := make([]*Intent, workers)
	errs := make([]error, workers)
	for i := range workers {
		go func() {
			defer wg.Done()
			<-start
			intent, err := j.Prepare(context.Background(), req)
			results[i] = intent
			errs[i] = err
		}()
	}
	close(start)
	wg.Wait()

	var winnerID string
	var successes int64
	for i := range workers {
		if errs[i] != nil {
			t.Fatalf("worker %d error: %v", i, errs[i])
		}
		if results[i] == nil {
			t.Fatalf("worker %d got nil intent with nil error", i)
		}
		if results[i].ID == "" {
			t.Fatalf("worker %d got empty intent id", i)
		}
		if winnerID == "" {
			winnerID = results[i].ID
		} else if results[i].ID != winnerID {
			t.Fatalf("worker %d intent %s differs from winner %s", i, results[i].ID, winnerID)
		}
		if results[i].State == StatePrepared {
			atomic.AddInt64(&successes, 1)
		}
	}
	if got := intentCount(t, db, "id = ?", winnerID); got != 1 {
		t.Fatalf("intent rows = %d, want 1", got)
	}
	if got := eventCount(t, db, EventKindToolRequested); got != 1 {
		t.Fatalf("tool.requested events = %d, want 1", got)
	}
	if successes != int64(workers) {
		t.Fatalf("prepared-state successes = %d, want %d", successes, workers)
	}
}

func TestTransition_ConcurrentStartSucceedSerializes(t *testing.T) {
	t.Parallel()
	j, db := newTestJournal(t)

	intent := mustPrepare(t, j, validPrepare(1))

	const workers = 16
	var wg sync.WaitGroup
	wg.Add(workers)
	start := make(chan struct{})
	receipt := json.RawMessage(`{"message_id":7}`)
	for range workers {
		go func() {
			defer wg.Done()
			<-start
			_, _ = j.Start(context.Background(), intent.ID)
			_, _ = j.Succeed(context.Background(), intent.ID, receipt)
		}()
	}
	close(start)
	wg.Wait()

	final, err := j.Get(context.Background(), intent.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if final.State != StateSucceeded {
		t.Fatalf("final state = %s, want succeeded", final.State)
	}
	if final.StartedAt == nil {
		t.Fatal("started_at not set")
	}
	if final.FinishedAt == nil {
		t.Fatal("finished_at not set")
	}
	var receiptCount int
	if err := db.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM effect_intent WHERE id = ? AND provider_receipt_json IS NOT NULL`,
		intent.ID).Scan(&receiptCount); err != nil {
		t.Fatalf("count receipts: %v", err)
	}
	if receiptCount != 1 {
		t.Fatalf("receipt rows = %d, want 1", receiptCount)
	}
}
