package effect

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
)

// fakeProvider is a deterministic provider with controllable outcomes,
// idempotency, and reconciliation. It stands in for any channel, webhook,
// broadcaster, or scheduler adapter: the contract it implements carries no
// domain terms.
type fakeProvider struct {
	mu                  sync.Mutex
	supportsIdempotency bool
	outcome             Outcome
	invokeErr           error
	invokeCount         int
	reconcileEvidence   Evidence
	reconcileErr        error
	reconcileCount      int
	// lastKey captures the idempotency key the provider was invoked with, so a
	// test can assert it is stable across retries.
	lastKey string
}

func (f *fakeProvider) SupportsIdempotency() bool { return f.supportsIdempotency }

func (f *fakeProvider) Invoke(_ context.Context, inv *Invocation) (Outcome, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.invokeCount++
	f.lastKey = inv.IdempotencyKey
	if f.invokeErr != nil {
		return Outcome{}, f.invokeErr
	}
	return f.outcome, nil
}

func (f *fakeProvider) Reconcile(_ context.Context, _ *Intent) (Evidence, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reconcileCount++
	if f.reconcileErr != nil {
		return Evidence{}, f.reconcileErr
	}
	return f.reconcileEvidence, nil
}

func newExecutor(t *testing.T) (*Executor, *Journal) {
	t.Helper()
	j, _ := newTestJournal(t)
	exec, err := NewExecutor(j, ExecutorOptions{})
	if err != nil {
		t.Fatalf("new executor: %v", err)
	}
	return exec, j
}

func TestExecute_DefiniteSuccessSettlesSucceeded(t *testing.T) {
	t.Parallel()
	exec, _ := newExecutor(t)
	p := &fakeProvider{
		supportsIdempotency: true,
		outcome:             Outcome{Succeeded: true, Receipt: json.RawMessage(`{"id":1}`)},
	}

	intent, err := exec.Execute(context.Background(), validPrepare(1), p)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if intent.State != StateSucceeded {
		t.Fatalf("state = %s, want succeeded", intent.State)
	}
	if intent.ProviderReceipt == nil {
		t.Fatal("receipt not recorded")
	}
	if p.invokeCount != 1 {
		t.Fatalf("invoke count = %d, want 1", p.invokeCount)
	}
}

func TestExecute_DefiniteFailureSettlesFailed(t *testing.T) {
	t.Parallel()
	exec, _ := newExecutor(t)
	p := &fakeProvider{outcome: Outcome{Succeeded: false, SafeErrorCode: "provider_4xx"}}

	intent, err := exec.Execute(context.Background(), validPrepare(1), p)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if intent.State != StateFailed {
		t.Fatalf("state = %s, want failed", intent.State)
	}
	if intent.SafeErrorCode != "provider_4xx" {
		t.Fatalf("safe error code = %q", intent.SafeErrorCode)
	}
}

// A lost response, connection loss, or commit failure after the provider may
// have observed the request becomes unknown, never prepared or auto-success.
func TestExecute_AmbiguousOutcomeBecomesUnknown(t *testing.T) {
	t.Parallel()
	exec, _ := newExecutor(t)
	p := &fakeProvider{outcome: Outcome{Ambiguous: true}}

	intent, err := exec.Execute(context.Background(), validPrepare(1), p)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if intent.State != StateUnknown {
		t.Fatalf("state = %s, want unknown", intent.State)
	}
}

func TestExecute_InvokeErrorBecomesUnknown(t *testing.T) {
	t.Parallel()
	exec, _ := newExecutor(t)
	p := &fakeProvider{invokeErr: errors.New("dial reset mid-send")}

	intent, err := exec.Execute(context.Background(), validPrepare(1), p)
	if err == nil {
		t.Fatal("expected invoke error to propagate")
	}
	if intent.State != StateUnknown {
		t.Fatalf("state = %s, want unknown after unclassifiable invoke", intent.State)
	}
}

// A replayed execute returns the existing intent and does not invoke again.
func TestExecute_ReplayIsIdempotent(t *testing.T) {
	t.Parallel()
	exec, _ := newExecutor(t)
	p := &fakeProvider{supportsIdempotency: true, outcome: Outcome{Succeeded: true, Receipt: json.RawMessage(`{}`)}}

	first, err := exec.Execute(context.Background(), validPrepare(1), p)
	if err != nil {
		t.Fatalf("first execute: %v", err)
	}
	second, err := exec.Execute(context.Background(), validPrepare(1), p)
	if err != nil {
		t.Fatalf("second execute: %v", err)
	}
	if second.ID != first.ID {
		t.Fatalf("replay returned %s, want %s", second.ID, first.ID)
	}
	if p.invokeCount != 1 {
		t.Fatalf("invoke count after replay = %d, want 1", p.invokeCount)
	}
}

// The idempotency key is stable across safe retries: re-invocation during
// recovery reuses the same key.
func TestReconcile_StableIdempotencyKeyOnRetry(t *testing.T) {
	t.Parallel()
	exec, _ := newExecutor(t)
	// First attempt is ambiguous -> unknown.
	ambiguous := &fakeProvider{supportsIdempotency: true, outcome: Outcome{Ambiguous: true}}
	intent, err := exec.Execute(context.Background(), validPrepare(1), ambiguous)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if intent.State != StateUnknown {
		t.Fatalf("setup state = %s, want unknown", intent.State)
	}

	// Recovery re-invokes (idempotent classification) with a definite outcome.
	retry := &fakeProvider{supportsIdempotency: true, outcome: Outcome{Succeeded: true, Receipt: json.RawMessage(`{"id":7}`)}}
	resolved, err := exec.Reconcile(context.Background(), intent.ID, retry, nil)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if resolved.State != StateSucceeded {
		t.Fatalf("state = %s, want succeeded", resolved.State)
	}
	if retry.lastKey != intent.IdempotencyKey {
		t.Fatalf("retry key %q != intent key %q", retry.lastKey, intent.IdempotencyKey)
	}
}

// Non-idempotent unknown effects are never automatically retried. An
// irreversible unknown intent with no reconciler stays unknown and reports
// that reconciliation is unsupported.
func TestReconcile_IrreversibleNeverAutoRetried(t *testing.T) {
	t.Parallel()
	exec, _ := newExecutor(t)

	req := validPrepare(1)
	req.Classification = ClassificationIrreversible
	intent, err := exec.Execute(context.Background(), req, &fakeProvider{outcome: Outcome{Ambiguous: true}})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if intent.State != StateUnknown {
		t.Fatalf("setup state = %s, want unknown", intent.State)
	}

	p := &fakeProvider{supportsIdempotency: true, outcome: Outcome{Succeeded: true}}
	_, err = exec.Reconcile(context.Background(), intent.ID, p, nil)
	assertCode(t, err, ErrorCodeReconciliationUnsupported)
	if p.invokeCount != 0 {
		t.Fatalf("irreversible was re-invoked %d times", p.invokeCount)
	}
	final, _ := exec.Journal().Get(context.Background(), intent.ID)
	if final.State != StateUnknown {
		t.Fatalf("state = %s, want unknown (no auto-retry)", final.State)
	}
}

// A non-idempotent unknown effect with a reconciler is reconciled but never
// re-invoked, even when reconciliation is non-definitive.
func TestReconcile_EffectfulWithoutIdempotencyNotReinvoked(t *testing.T) {
	t.Parallel()
	exec, _ := newExecutor(t)

	req := validPrepare(1)
	req.Classification = ClassificationEffectful
	intent, err := exec.Execute(context.Background(), req, &fakeProvider{supportsIdempotency: false, outcome: Outcome{Ambiguous: true}})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}

	p := &fakeProvider{supportsIdempotency: false}
	r := &fakeProvider{reconcileEvidence: Evidence{Definitive: false}}
	resolved, err := exec.Reconcile(context.Background(), intent.ID, p, r)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if resolved.State != StateUnknown {
		t.Fatalf("state = %s, want unknown", resolved.State)
	}
	if p.invokeCount != 0 {
		t.Fatalf("non-idempotent effectful was re-invoked %d times", p.invokeCount)
	}
}

// Reconciliation transitions unknown only when provider evidence is
// definitive. Non-definitive evidence leaves the intent unknown.
func TestReconcile_DefinitiveEvidenceResolves(t *testing.T) {
	t.Parallel()
	exec, _ := newExecutor(t)
	intent := mustPrepare(t, exec.Journal(), validPrepare(1))
	mustStart(t, exec.Journal(), intent.ID)
	if _, err := exec.Journal().MarkUnknown(context.Background(), intent.ID); err != nil {
		t.Fatalf("mark unknown: %v", err)
	}

	r := &fakeProvider{reconcileEvidence: Evidence{
		Definitive: true,
		Succeeded:  true,
		Receipt:    json.RawMessage(`{"message_id":42}`),
	}}
	resolved, err := exec.Reconcile(context.Background(), intent.ID, &fakeProvider{}, r)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if resolved.State != StateSucceeded {
		t.Fatalf("state = %s, want succeeded", resolved.State)
	}
	if resolved.ReconciledAt == nil {
		t.Fatal("reconciled_at not set")
	}
}

func TestReconcile_NonDefinitiveEvidenceStaysUnknown(t *testing.T) {
	t.Parallel()
	exec, _ := newExecutor(t)
	intent := mustPrepare(t, exec.Journal(), validPrepare(1))
	mustStart(t, exec.Journal(), intent.ID)
	if _, err := exec.Journal().MarkUnknown(context.Background(), intent.ID); err != nil {
		t.Fatalf("mark unknown: %v", err)
	}

	r := &fakeProvider{reconcileEvidence: Evidence{Definitive: false}}
	resolved, err := exec.Reconcile(context.Background(), intent.ID, &fakeProvider{}, r)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if resolved.State != StateUnknown {
		t.Fatalf("state = %s, want unknown", resolved.State)
	}
}

func TestReconcile_ReconcilerFailureIsTyped(t *testing.T) {
	t.Parallel()
	exec, _ := newExecutor(t)
	intent := mustPrepare(t, exec.Journal(), validPrepare(1))
	mustStart(t, exec.Journal(), intent.ID)
	if _, err := exec.Journal().MarkUnknown(context.Background(), intent.ID); err != nil {
		t.Fatalf("mark unknown: %v", err)
	}

	r := &fakeProvider{reconcileErr: errors.New("upstream 500")}
	_, err := exec.Reconcile(context.Background(), intent.ID, &fakeProvider{}, r)
	assertCode(t, err, ErrorCodeReconciliationFailed)
}

func TestReconcile_OnlyUnknownIntentsReconcile(t *testing.T) {
	t.Parallel()
	exec, _ := newExecutor(t)
	intent := mustPrepare(t, exec.Journal(), validPrepare(1))
	_, err := exec.Reconcile(context.Background(), intent.ID, &fakeProvider{}, nil)
	assertCode(t, err, ErrorCodeTransitionInvalid)
}

// CanAutoRetry policy across classifications and provider capability.
func TestCanAutoRetry(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name        string
		class       Classification
		idempotency bool
		want        bool
	}{
		{"read_only always", ClassificationReadOnly, false, true},
		{"idempotent always", ClassificationIdempotent, false, true},
		{"effectful with key", ClassificationEffectful, true, true},
		{"effectful without key", ClassificationEffectful, false, false},
		{"irreversible never", ClassificationIrreversible, true, false},
		{"unknown classification", Classification("magic"), true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := CanAutoRetry(tc.class, tc.idempotency); got != tc.want {
				t.Fatalf("CanAutoRetry(%s, idempot=%v) = %v, want %v", tc.class, tc.idempotency, got, tc.want)
			}
		})
	}
}

// Recover sweeps unknown intents: idempotent ones are resolved by
// re-invocation, non-idempotent ones without a reconciler are left unknown.
func TestRecover_MixedClassifications(t *testing.T) {
	t.Parallel()
	exec, _ := newExecutor(t)

	// Two idempotent unknown intents (recoverable by re-invoke) and one
	// irreversible unknown intent (no auto-retry, no reconciler).
	idem1 := mustPrepare(t, exec.Journal(), validPrepare(1))
	mustStart(t, exec.Journal(), idem1.ID)
	if _, err := exec.Journal().MarkUnknown(context.Background(), idem1.ID); err != nil {
		t.Fatal(err)
	}
	idem2Req := validPrepare(2)
	idem2 := mustPrepare(t, exec.Journal(), idem2Req)
	mustStart(t, exec.Journal(), idem2.ID)
	if _, err := exec.Journal().MarkUnknown(context.Background(), idem2.ID); err != nil {
		t.Fatal(err)
	}
	irrReq := validPrepare(3)
	irrReq.Classification = ClassificationIrreversible
	irr := mustPrepare(t, exec.Journal(), irrReq)
	mustStart(t, exec.Journal(), irr.ID)
	if _, err := exec.Journal().MarkUnknown(context.Background(), irr.ID); err != nil {
		t.Fatal(err)
	}

	// Recovery provider re-invokes idempotent ops successfully; the
	// irreversible op has no reconciler and no idempotency, so it is left.
	p := &fakeProvider{supportsIdempotency: true, outcome: Outcome{Succeeded: true, Receipt: json.RawMessage(`{}`)}}
	summary, err := exec.Recover(context.Background(), p, nil)
	if err != nil {
		t.Fatalf("recover: %v", err)
	}
	if summary.Scanned != 3 {
		t.Fatalf("scanned = %d, want 3", summary.Scanned)
	}
	if summary.Resolved != 2 {
		t.Fatalf("resolved = %d, want 2", summary.Resolved)
	}
	if summary.Unsupported != 1 {
		t.Fatalf("unsupported = %d, want 1", summary.Unsupported)
	}
}

func TestNewExecutor_NilJournal(t *testing.T) {
	t.Parallel()
	_, err := NewExecutor(nil, ExecutorOptions{})
	assertCode(t, err, ErrorCodeInvalidArgument)
}

func TestExecute_NilProvider(t *testing.T) {
	t.Parallel()
	exec, _ := newExecutor(t)
	_, err := exec.Execute(context.Background(), validPrepare(1), nil)
	assertCode(t, err, ErrorCodeInvalidArgument)
}

// Smoke test that the deterministic fake provider exercises the same protocol
// a real channel/webhook/broadcaster/scheduler adapter would: the Provider
// contract is domain-neutral.
func TestExecute_ProtocolIsDomainNeutral(t *testing.T) {
	t.Parallel()
	exec, _ := newExecutor(t)

	var sawReadOnly, sawEffectful bool
	providers := map[Classification]*fakeProvider{
		ClassificationReadOnly: {outcome: Outcome{Succeeded: true, Receipt: json.RawMessage(`{}`)}},
		ClassificationEffectful: {
			supportsIdempotency: true,
			outcome:             Outcome{Succeeded: true, Receipt: json.RawMessage(`{}`)},
		},
	}
	for class, p := range providers {
		req := validPrepare(int(class[0]))
		req.Classification = class
		req.IdempotencyKey = string(class)
		intent, err := exec.Execute(context.Background(), req, p)
		if err != nil {
			t.Fatalf("execute %s: %v", class, err)
		}
		if intent.State != StateSucceeded {
			t.Fatalf("%s state = %s, want succeeded", class, intent.State)
		}
		if class == ClassificationReadOnly {
			sawReadOnly = true
		}
		if class == ClassificationEffectful {
			sawEffectful = true
		}
	}
	if !sawReadOnly || !sawEffectful {
		t.Fatal("did not exercise both classifications")
	}
}

func TestExecute_ConcurrentSameIntentSerializes(t *testing.T) {
	t.Parallel()
	exec, _ := newExecutor(t)

	const workers = 12
	req := validPrepare(1)
	// A single shared provider records every invocation; idempotent Execute
	// must invoke it exactly once no matter how many callers race.
	p := &fakeProvider{
		supportsIdempotency: true,
		outcome:             Outcome{Succeeded: true, Receipt: json.RawMessage(`{}`)},
	}
	var wg sync.WaitGroup
	wg.Add(workers)
	start := make(chan struct{})
	for range workers {
		go func() {
			defer wg.Done()
			<-start
			_, _ = exec.Execute(context.Background(), req, p)
		}()
	}
	close(start)
	wg.Wait()

	p.mu.Lock()
	invokes := p.invokeCount
	p.mu.Unlock()
	if invokes != 1 {
		t.Fatalf("provider invokes = %d, want exactly 1", invokes)
	}
	// Prepare is idempotent, so it resolves the single shared intent id.
	intent, err := exec.Journal().Prepare(context.Background(), req)
	if err != nil {
		t.Fatalf("resolve intent id: %v", err)
	}
	if intent.State != StateSucceeded {
		t.Fatalf("final state = %s, want succeeded", intent.State)
	}
}
