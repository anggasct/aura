package sync

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/anggasct/aura/internal/effect"
	"github.com/anggasct/aura/internal/store"
)

type stubRunner struct {
	journal *effect.Journal
	lastReq *effect.PrepareRequest
}

func (s *stubRunner) Execute(ctx context.Context, req *effect.PrepareRequest, provider effect.Provider) (*effect.Intent, error) {
	s.lastReq = req
	intent, err := s.journal.Prepare(ctx, req)
	if err != nil {
		return nil, err
	}
	if intent.State != effect.StatePrepared {
		return intent, nil
	}
	started, err := s.journal.Start(ctx, intent.ID)
	if err != nil {
		return nil, err
	}
	outcome, err := provider.Invoke(ctx, &effect.Invocation{
		IntentID:       started.ID,
		IdempotencyKey: started.IdempotencyKey,
		Provider:       started.Provider,
		Operation:      started.Operation,
		Classification: started.Classification,
		Request:        started.RequestJSON,
	})
	if err != nil {
		unknown, markErr := s.journal.MarkUnknown(ctx, started.ID)
		return unknown, errors.Join(err, markErr)
	}
	if outcome.Ambiguous {
		return s.journal.MarkUnknown(ctx, started.ID)
	}
	if outcome.Succeeded {
		return s.journal.Succeed(ctx, started.ID, outcome.Receipt)
	}
	return s.journal.Fail(ctx, started.ID, outcome.SafeErrorCode)
}

func (s *stubRunner) Reconcile(ctx context.Context, id string, _ effect.Provider, reconciler effect.Reconciler) (*effect.Intent, error) {
	intent, err := s.journal.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if intent.State != effect.StateUnknown {
		return intent, nil
	}
	evidence, err := reconciler.Reconcile(ctx, intent)
	if err != nil {
		return nil, err
	}
	if !evidence.Definitive {
		return intent, nil
	}
	if evidence.Succeeded {
		return s.journal.Resolve(ctx, id, effect.Resolution{Succeeded: true, Receipt: evidence.Receipt})
	}
	return s.journal.Resolve(ctx, id, effect.Resolution{Succeeded: false, SafeErrorCode: evidence.SafeErrorCode})
}

func openPushJournal(t *testing.T) *effect.Journal {
	t.Helper()
	db, err := store.OpenDB(t.Context(), t.TempDir()+"/push.db")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := store.Migrate(t.Context(), db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	svc := store.NewSessionService(db)
	if err := svc.Create(t.Context(), &store.Session{ID: "session-1", OwnerID: "owner-1"}); err != nil {
		t.Fatalf("seed session: %v", err)
	}
	journal, err := effect.NewJournal(db, effect.Options{})
	if err != nil {
		t.Fatalf("NewJournal: %v", err)
	}
	return journal
}

func TestStartPushIdempotent(t *testing.T) {
	t.Parallel()
	journal := openPushJournal(t)
	runner := &stubRunner{journal: journal}
	first, err := StartPush(t.Context(), runner, "session-1", "ssh://git@example.com/owner/repo.git", "main", "digest-1", "turn-1", "call-1", 1)
	if err != nil {
		t.Fatalf("StartPush: %v", err)
	}
	second, err := StartPush(t.Context(), runner, "session-1", "ssh://git@example.com/owner/repo.git", "main", "digest-1", "turn-1", "call-1", 1)
	if err != nil {
		t.Fatalf("StartPush replay: %v", err)
	}
	if first.ID != second.ID {
		t.Fatalf("replay created %q, want %q", second.ID, first.ID)
	}
	if runner.lastReq == nil || runner.lastReq.Classification != effect.ClassificationIdempotent {
		t.Fatalf("classification = %+v, want idempotent", runner.lastReq)
	}
}

func TestStartPushRejectsDigestChange(t *testing.T) {
	t.Parallel()
	journal := openPushJournal(t)
	runner := &stubRunner{journal: journal}
	if _, err := StartPush(t.Context(), runner, "session-1", "ssh://git@example.com/owner/repo.git", "main", "digest-1", "turn-1", "call-1", 1); err != nil {
		t.Fatalf("StartPush: %v", err)
	}
	if _, err := StartPush(t.Context(), runner, "session-1", "ssh://git@example.com/owner/repo.git", "main", "digest-2", "turn-1", "call-1", 1); err == nil {
		t.Fatal("changed digest must conflict")
	} else if code, ok := store.CodeOf(err); !ok && code == "" {
		t.Fatalf("expected a classified conflict, got %v", err)
	}
}

func TestPushReconcileByRemoteRef(t *testing.T) {
	t.Parallel()
	journal := openPushJournal(t)
	runner := &stubRunner{journal: journal}
	intent, err := StartPush(t.Context(), runner, "session-1", "ssh://git@example.com/owner/repo.git", "main", "digest-1", "turn-1", "call-1", 1)
	if err != nil {
		t.Fatalf("StartPush: %v", err)
	}
	if intent.State != effect.StateUnknown {
		t.Fatalf("state = %q, want unknown without an observer", intent.State)
	}
	reconciler := &PushReconciler{Observe: func(_ context.Context, _, _ string) (string, error) {
		return "ref-abc123", nil
	}}
	resolved, err := runner.Reconcile(t.Context(), intent.ID, &PushProviderAdapter{}, reconciler)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if resolved.State != effect.StateSucceeded {
		t.Fatalf("state = %q, want succeeded after remote ref confirms", resolved.State)
	}
	var receipt PushReceipt
	if err := json.Unmarshal(resolved.ProviderReceipt, &receipt); err != nil {
		t.Fatalf("receipt: %v", err)
	}
	if receipt.Ref != "ref-abc123" || receipt.Digest != "digest-1" {
		t.Fatalf("receipt = %+v, want ref-abc123 and digest-1", receipt)
	}
}

func TestPushReconcileUnknownWithoutObserver(t *testing.T) {
	t.Parallel()
	journal := openPushJournal(t)
	runner := &stubRunner{journal: journal}
	intent, err := StartPush(t.Context(), runner, "session-1", "ssh://git@example.com/owner/repo.git", "main", "digest-1", "turn-1", "call-1", 1)
	if err != nil {
		t.Fatalf("StartPush: %v", err)
	}
	resolved, err := ReconcilePush(t.Context(), runner, intent.ID, "ssh://git@example.com/owner/repo.git", "main")
	if err != nil {
		t.Fatalf("ReconcilePush: %v", err)
	}
	if resolved.State != effect.StateUnknown {
		t.Fatalf("state = %q, want unknown to persist without remote evidence", resolved.State)
	}
}
