package effect

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/platform/durable/durabletest"
	"github.com/anggasct/aura/internal/platform/fault"
)

type heldProvider struct {
	entered chan struct{}
	release chan struct{}
	outcome Outcome
	panics  bool

	mu        sync.Mutex
	cancelled bool
}

func newHeldProvider(outcome Outcome) *heldProvider {
	return &heldProvider{entered: make(chan struct{}, 8), release: make(chan struct{}), outcome: outcome}
}

func (h *heldProvider) SupportsIdempotency() bool { return false }

func (h *heldProvider) Invoke(ctx context.Context, _ *Invocation) (Outcome, error) {
	h.entered <- struct{}{}
	select {
	case <-ctx.Done():
		h.mu.Lock()
		h.cancelled = true
		h.mu.Unlock()
		return Outcome{}, ctx.Err()
	case <-h.release:
	}
	if h.panics {
		panic("provider exploded")
	}
	return h.outcome, nil
}

func (h *heldProvider) wasCancelled() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.cancelled
}

type takeoverHeldProvider struct {
	*heldProvider
	takeover bool
}

func (p *takeoverHeldProvider) SupportsTakeover() bool { return p.takeover }

type executeResult struct {
	intent *Intent
	err    error
}

func executeInBackground(ctx context.Context, e *Executor, req *PrepareRequest, p Provider) <-chan executeResult {
	out := make(chan executeResult, 1)
	go func() {
		intent, err := e.Execute(ctx, req, p)
		out <- executeResult{intent: intent, err: err}
	}()
	return out
}

func leaseColumns(t *testing.T, db *sql.DB, id string) (state State, token, expiresAt string) {
	t.Helper()
	var tokenCol, expiresCol sql.NullString
	if err := db.QueryRowContext(t.Context(),
		`SELECT state, lease_token, lease_expires_at FROM effect_intent WHERE id = ?`, id).Scan(&state, &tokenCol, &expiresCol); err != nil {
		t.Fatalf("read lease columns: %v", err)
	}
	return state, tokenCol.String, expiresCol.String
}

func setLease(t *testing.T, db *sql.DB, id string, token, expiresAt any) {
	t.Helper()
	if _, err := db.ExecContext(t.Context(),
		`UPDATE effect_intent SET lease_token = ?, lease_expires_at = ? WHERE id = ?`, token, expiresAt, id); err != nil {
		t.Fatalf("set lease: %v", err)
	}
}

func blockRenewals(t *testing.T, db *sql.DB) {
	t.Helper()
	if _, err := db.ExecContext(t.Context(), `
CREATE TRIGGER block_lease_renewal
BEFORE UPDATE OF lease_expires_at ON effect_intent
WHEN OLD.state = 'started' AND NEW.state = 'started' AND NEW.lease_token IS OLD.lease_token
BEGIN
    SELECT RAISE(ABORT, 'renewal blocked');
END`); err != nil {
		t.Fatalf("create renewal trigger: %v", err)
	}
}

func newProbedExecutor(t *testing.T) (*Executor, *sql.DB, *durabletest.ManualClock) {
	t.Helper()
	probe := durabletest.NewManualClock(testEpoch)
	journal, db := newJournalOnClock(t, probe, nil)
	executor, err := NewExecutor(journal)
	if err != nil {
		t.Fatalf("new executor: %v", err)
	}
	return executor, db, probe
}

func keeperGoroutines() int {
	buf := make([]byte, 1<<20)
	buf = buf[:runtime.Stack(buf, true)]
	return strings.Count(string(buf), "(*leaseKeeper).run")
}

func TestStartSetsATokenAndASixtySecondLease(t *testing.T) {
	t.Parallel()
	j, db, clock := newClockedJournal(t)
	prepared := mustPrepare(t, j, validPrepare(1))

	started := mustStart(t, j, prepared.ID)

	if len(started.LeaseToken) < 26 {
		t.Fatalf("lease token = %q, want at least 128 random bits in base32", started.LeaseToken)
	}
	_, token, expiresAt := leaseColumns(t, db, started.ID)
	if token != started.LeaseToken {
		t.Fatalf("stored token = %q, want the one Start returned", token)
	}
	if want := fmtLeaseTime(clock.Now().Add(60 * time.Second)); expiresAt != want {
		t.Fatalf("lease_expires_at = %q, want %q", expiresAt, want)
	}
	read, err := j.Get(t.Context(), started.ID)
	if err != nil || read.LeaseToken != "" {
		t.Fatalf("Get = %+v (%v), want no lease token", read, err)
	}
	listed, err := j.ListByState(t.Context(), StateStarted, 0)
	if err != nil || len(listed) != 1 || listed[0].LeaseToken != "" {
		t.Fatalf("ListByState = %+v (%v), want no lease token", listed, err)
	}
	other := mustStart(t, j, mustPrepare(t, j, validPrepare(2)).ID)
	if other.LeaseToken == started.LeaseToken {
		t.Fatal("two intents share a lease token")
	}
}

func TestLeaseIsRenewedWhileProviderRuns(t *testing.T) {
	t.Parallel()
	executor, db, probe := newProbedExecutor(t)
	provider := newHeldProvider(Outcome{Succeeded: true, Receipt: json.RawMessage(`{}`)})
	done := executeInBackground(t.Context(), executor, validPrepare(1), provider)
	<-provider.entered
	probe.AwaitArmed(t, 1)

	var id string
	if err := db.QueryRowContext(t.Context(), `SELECT id FROM effect_intent`).Scan(&id); err != nil {
		t.Fatalf("read intent id: %v", err)
	}
	for range 5 {
		probe.Advance(20 * time.Second)
		probe.AwaitArmed(t, 1)
		state, _, expiresAt := leaseColumns(t, db, id)
		if want := fmtLeaseTime(probe.Now().Add(60 * time.Second)); state != StateStarted || expiresAt != want {
			t.Fatalf("after renewal: state %s lease %q, want started and %q", state, expiresAt, want)
		}
	}
	claimed, err := executor.Journal().Claim(t.Context(), id)
	if err != nil || claimed {
		t.Fatalf("Claim of a renewed lease = %v, %v; want it refused", claimed, err)
	}

	close(provider.release)
	result := <-done
	if result.err != nil || result.intent.State != StateSucceeded {
		t.Fatalf("Execute = %+v, %v; want succeeded", result.intent, result.err)
	}
}

func TestRenewalStopsWhenExecuteReturns(t *testing.T) {
	cases := []struct {
		name    string
		outcome Outcome
		panics  bool
	}{
		{name: "a success", outcome: Outcome{Succeeded: true}},
		{name: "a definite failure", outcome: Outcome{SafeErrorCode: "provider_4xx"}},
		{name: "an ambiguous outcome", outcome: Outcome{Ambiguous: true}},
		{name: "a provider panic", panics: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			executor, _, probe := newProbedExecutor(t)
			provider := newHeldProvider(tc.outcome)
			provider.panics = tc.panics
			done := make(chan any, 1)
			go func() {
				defer func() { done <- recover() }()
				_, _ = executor.Execute(t.Context(), validPrepare(1), provider)
			}()
			<-provider.entered
			probe.AwaitArmed(t, 1)
			if got := keeperGoroutines(); got != 1 {
				t.Fatalf("renewal goroutines while the provider runs = %d, want 1", got)
			}

			close(provider.release)
			recovered := <-done
			if tc.panics && recovered == nil {
				t.Fatal("the provider panic did not reach the caller")
			}
			if got := keeperGoroutines(); got != 0 {
				t.Fatalf("renewal goroutines after Execute returned = %d, want 0", got)
			}
		})
	}
}

func TestStaleHolderCannotSettleAfterLosingLease(t *testing.T) {
	cases := []struct {
		name   string
		settle func(ctx context.Context, j *Journal, stale *Intent) error
	}{
		{name: "succeed", settle: func(ctx context.Context, j *Journal, stale *Intent) error {
			_, err := j.Succeed(ctx, stale.ID, stale.LeaseToken, json.RawMessage(`{}`))
			return err
		}},
		{name: "settle with an outcome", settle: func(ctx context.Context, j *Journal, stale *Intent) error {
			_, err := j.succeedWith(ctx, stale, &Outcome{Succeeded: true, Result: json.RawMessage(`{"a":1}`)})
			return err
		}},
		{name: "fail", settle: func(ctx context.Context, j *Journal, stale *Intent) error {
			_, err := j.Fail(ctx, stale.ID, stale.LeaseToken, "provider_4xx")
			return err
		}},
		{name: "mark unknown", settle: func(ctx context.Context, j *Journal, stale *Intent) error {
			_, err := j.MarkUnknown(ctx, stale.ID, stale.LeaseToken)
			return err
		}},
		{name: "renew", settle: func(ctx context.Context, j *Journal, stale *Intent) error {
			_, err := j.renewLease(ctx, stale)
			return err
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			j, db, clock := newClockedJournal(t)
			stale := mustStart(t, j, mustPrepare(t, j, validPrepare(1)).ID)
			lapseLeases(clock)
			current, err := j.takeOver(t.Context(), stale.ID)
			if err != nil {
				t.Fatalf("takeOver: %v", err)
			}

			assertCode(t, tc.settle(t.Context(), j, stale), ErrorCodeLeaseLost)
			state, token, _ := leaseColumns(t, db, stale.ID)
			if state != StateStarted || token != current.LeaseToken {
				t.Fatalf("after the stale write: state %s token %q, want started under the new holder's token", state, token)
			}

			if _, err := j.Succeed(t.Context(), current.ID, current.LeaseToken, json.RawMessage(`{"ok":true}`)); err != nil {
				t.Fatalf("the new holder could not settle: %v", err)
			}
			assertCode(t, tc.settle(t.Context(), j, stale), ErrorCodeLeaseLost)
		})
	}
}

func TestLeaseLossCancelsProvider(t *testing.T) {
	executor, db, probe := newProbedExecutor(t)
	provider := newHeldProvider(Outcome{Succeeded: true})
	done := executeInBackground(t.Context(), executor, validPrepare(1), provider)
	<-provider.entered
	probe.AwaitArmed(t, 1)
	var id string
	if err := db.QueryRowContext(t.Context(), `SELECT id FROM effect_intent`).Scan(&id); err != nil {
		t.Fatalf("read intent id: %v", err)
	}
	setLease(t, db, id, "replaced-holder", fmtLeaseTime(testEpoch.Add(-time.Hour)))

	probe.Advance(20 * time.Second)
	result := <-done

	assertCode(t, result.err, ErrorCodeLeaseLost)
	if !provider.wasCancelled() {
		t.Fatal("the provider context was not cancelled when the renewal found its token replaced")
	}
	state, token, _ := leaseColumns(t, db, id)
	if state != StateStarted || token != "replaced-holder" {
		t.Fatalf("after the lost lease: state %s token %q, want the new holder's record untouched", state, token)
	}
	if got := keeperGoroutines(); got != 0 {
		t.Fatalf("renewal goroutines after the lease was lost = %d, want 0", got)
	}
}

func TestLapsedLeaseSettlesNormally(t *testing.T) {
	t.Parallel()
	executor, db, probe := newProbedExecutor(t)
	blockRenewals(t, db)
	provider := newHeldProvider(Outcome{Succeeded: true, Receipt: json.RawMessage(`{"id":1}`)})
	done := executeInBackground(t.Context(), executor, validPrepare(1), provider)
	<-provider.entered
	probe.AwaitArmed(t, 1)

	probe.Advance(61 * time.Second)
	probe.AwaitArmed(t, 1)
	if provider.wasCancelled() {
		t.Fatal("a lapsed but untaken lease cancelled a provider that never opted into takeover")
	}
	close(provider.release)
	result := <-done

	if result.err != nil || result.intent.State != StateSucceeded {
		t.Fatalf("Execute = %+v, %v; want the lapsed lease to settle normally", result.intent, result.err)
	}
}

func TestLapseCancelsOnlyTakeoverProviders(t *testing.T) {
	cases := []struct {
		name          string
		wrap          func(*heldProvider) Provider
		timers        int
		wantCancelled bool
		wantState     State
	}{
		{name: "a plain provider", wrap: func(h *heldProvider) Provider { return h }, timers: 1, wantState: StateSucceeded},
		{name: "a takeover provider", wrap: func(h *heldProvider) Provider { return &takeoverHeldProvider{heldProvider: h, takeover: true} }, timers: 2, wantCancelled: true, wantState: StateStarted},
		{name: "a provider that declines takeover", wrap: func(h *heldProvider) Provider { return &takeoverHeldProvider{heldProvider: h} }, timers: 1, wantState: StateSucceeded},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			executor, db, probe := newProbedExecutor(t)
			blockRenewals(t, db)
			held := newHeldProvider(Outcome{Succeeded: true})
			done := executeInBackground(t.Context(), executor, validPrepare(1), tc.wrap(held))
			<-held.entered
			probe.AwaitArmed(t, tc.timers)

			probe.Advance(61 * time.Second)
			if tc.wantCancelled {
				result := <-done
				assertCode(t, result.err, ErrorCodeLeaseLost)
				if !held.wasCancelled() {
					t.Fatal("the takeover provider kept running past the lapse")
				}
			} else {
				probe.AwaitArmed(t, 1)
				if held.wasCancelled() {
					t.Fatal("the lapse cancelled a provider that cannot be taken over")
				}
				close(held.release)
				result := <-done
				if result.err != nil {
					t.Fatalf("Execute: %v", result.err)
				}
			}
			var id string
			if err := db.QueryRowContext(t.Context(), `SELECT id FROM effect_intent`).Scan(&id); err != nil {
				t.Fatalf("read intent id: %v", err)
			}
			if state, _, _ := leaseColumns(t, db, id); state != tc.wantState {
				t.Fatalf("state = %s, want %s", state, tc.wantState)
			}
			if tc.wantCancelled {
				resumer := &fakeProvider{outcome: Outcome{Succeeded: true}}
				resumed, err := executor.Resume(t.Context(), id, resumer, nil)
				if err != nil || resumed.State != StateSucceeded || resumer.invokeCount != 1 {
					t.Fatalf("Resume after the lapse cancel = %+v, %v with %d runs; want the intent taken over and run", resumed, err, resumer.invokeCount)
				}
			}
		})
	}
}

func TestLapseCancelLandsAtTheStoredExpiry(t *testing.T) {
	j, db, clock := newClockedJournal(t)
	blockRenewals(t, db)
	started := mustStart(t, j, mustPrepare(t, j, validPrepare(1)).ID)
	clock.Advance(30 * time.Second)
	providerCtx, revoke := context.WithCancelCause(t.Context())
	defer revoke(nil)
	provider := &takeoverHeldProvider{heldProvider: newHeldProvider(Outcome{}), takeover: true}
	stop, err := j.keepLease(t.Context(), started, revoke, provider)
	if err != nil {
		t.Fatalf("keepLease: %v", err)
	}
	defer stop()
	clock.AwaitArmed(t, 2)

	clock.Advance(25 * time.Second)
	clock.AwaitArmed(t, 2)
	if providerCtx.Err() != nil {
		t.Fatal("the provider was cancelled before the stored expiry")
	}
	clock.Advance(5 * time.Second)

	select {
	case <-providerCtx.Done():
	case <-t.Context().Done():
		t.Fatal("the provider was not cancelled at the stored expiry")
	}
	if cause := context.Cause(providerCtx); !errors.Is(cause, errLeaseRevoked) {
		t.Fatalf("cancel cause = %v, want the lease revoked", cause)
	}
}

func TestLeaseStaysLiveWhileTheSettlerReadsEvidence(t *testing.T) {
	j, db, clock := newClockedJournal(t)
	executor, err := NewExecutor(j)
	if err != nil {
		t.Fatalf("NewExecutor: %v", err)
	}
	started := mustStart(t, j, mustPrepare(t, j, validPrepare(1)).ID)
	lapseLeases(clock)
	entered := make(chan *Intent, 1)
	release := make(chan struct{})
	settle := func(_ context.Context, taken *Intent) (Outcome, bool, error) {
		entered <- taken
		<-release
		return Outcome{Succeeded: true}, true, nil
	}
	done := make(chan executeResult, 1)
	go func() {
		intent, err := executor.Resume(t.Context(), started.ID, &fakeProvider{}, settle)
		done <- executeResult{intent: intent, err: err}
	}()
	taken := <-entered
	clock.AwaitArmed(t, 1)

	for range 4 {
		clock.Advance(20 * time.Second)
		clock.AwaitArmed(t, 1)
		_, token, expiresAt := leaseColumns(t, db, started.ID)
		if want := fmtLeaseTime(clock.Now().Add(leaseDuration)); token != taken.LeaseToken || expiresAt != want {
			t.Fatalf("while the settler runs: token %q lease %q, want the resumer's token and %q", token, expiresAt, want)
		}
	}
	claimed, err := j.Claim(t.Context(), started.ID)
	if err != nil || claimed {
		t.Fatalf("Claim during a slow settler = %v, %v; want the lease live", claimed, err)
	}

	close(release)
	result := <-done
	if result.err != nil || result.intent.State != StateSucceeded {
		t.Fatalf("Resume = %+v, %v; want succeeded", result.intent, result.err)
	}
	if got := keeperGoroutines(); got != 0 {
		t.Fatalf("renewal goroutines after Resume = %d, want 0", got)
	}
}

type logSignal struct {
	slog.Handler
	message string
	seen    chan string
}

func (h *logSignal) Enabled(context.Context, slog.Level) bool { return true }

func (h *logSignal) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &logSignal{Handler: h.Handler.WithAttrs(attrs), message: h.message, seen: h.seen}
}

func (h *logSignal) WithGroup(name string) slog.Handler {
	return &logSignal{Handler: h.Handler.WithGroup(name), message: h.message, seen: h.seen}
}

func (h *logSignal) Handle(ctx context.Context, record slog.Record) error {
	if record.Message == h.message {
		record.Attrs(func(attr slog.Attr) bool {
			if attr.Key == "status" {
				h.seen <- attr.Value.String()
			}
			return true
		})
	}
	return h.Handler.Handle(ctx, record)
}

func TestRenewalStopsWithoutCancellingWhenTheRowLeftStarted(t *testing.T) {
	clock := durabletest.NewManualClock(testEpoch)
	signal := &logSignal{Handler: slog.DiscardHandler, message: "effect lease renewal stopped", seen: make(chan string, 1)}
	journal, db := newJournalOnClockLogging(t, clock, nil, slog.New(signal))
	executor, err := NewExecutor(journal)
	if err != nil {
		t.Fatalf("NewExecutor: %v", err)
	}
	provider := newHeldProvider(Outcome{Succeeded: true})
	done := executeInBackground(t.Context(), executor, validPrepare(1), provider)
	<-provider.entered
	clock.AwaitArmed(t, 1)
	var id string
	if err := db.QueryRowContext(t.Context(), `SELECT id FROM effect_intent`).Scan(&id); err != nil {
		t.Fatalf("read intent id: %v", err)
	}
	_, token, _ := leaseColumns(t, db, id)
	setLease(t, db, id, token, fmtLeaseTime(testEpoch.Add(-time.Hour)))
	if claimed, err := journal.Claim(t.Context(), id); err != nil || !claimed {
		t.Fatalf("Claim = %v, %v", claimed, err)
	}

	clock.Advance(20 * time.Second)
	if status := <-signal.seen; status != "not_started" {
		t.Fatalf("renewal stopped with status %q, want not_started", status)
	}
	if provider.wasCancelled() {
		t.Fatal("a renewal that found the row settled by recovery, under the same token, cancelled the provider")
	}
	close(provider.release)
	result := <-done

	assertCode(t, result.err, ErrorCodeTransitionInvalid)
	if state, _, _ := leaseColumns(t, db, id); state != StateUnknown {
		t.Fatalf("state = %s, want the recovered unknown record untouched", state)
	}
}

func TestRenewalStopsWhenTheCallerContextIsCancelled(t *testing.T) {
	executor, _, probe := newProbedExecutor(t)
	provider := newHeldProvider(Outcome{Succeeded: true})
	ctx, cancel := context.WithCancel(t.Context())
	done := executeInBackground(ctx, executor, validPrepare(1), provider)
	<-provider.entered
	probe.AwaitArmed(t, 1)

	cancel()
	result := <-done

	if result.err == nil {
		t.Fatal("Execute reported success after its context was cancelled")
	}
	if got := keeperGoroutines(); got != 0 {
		t.Fatalf("renewal goroutines after the caller context was cancelled = %d, want 0", got)
	}
}

func TestEmptyTokenCannotSettleAStartedIntent(t *testing.T) {
	cases := []struct {
		name   string
		settle func(ctx context.Context, j *Journal, id string) error
	}{
		{name: "succeed", settle: func(ctx context.Context, j *Journal, id string) error {
			_, err := j.Succeed(ctx, id, "", nil)
			return err
		}},
		{name: "fail", settle: func(ctx context.Context, j *Journal, id string) error {
			_, err := j.Fail(ctx, id, "", "provider_4xx")
			return err
		}},
		{name: "mark unknown", settle: func(ctx context.Context, j *Journal, id string) error {
			_, err := j.MarkUnknown(ctx, id, "")
			return err
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			j, db, clock := newClockedJournal(t)
			started := mustStart(t, j, mustPrepare(t, j, validPrepare(1)).ID)
			setLease(t, db, started.ID, nil, nil)

			assertCode(t, tc.settle(t.Context(), j, started.ID), ErrorCodeInvalidArgument)

			if state, _, _ := leaseColumns(t, db, started.ID); state != StateStarted {
				t.Fatalf("state = %s, a legacy intent must stay started until it is claimed or taken over", state)
			}
			lapseLeases(clock)
			taken, err := j.takeOver(t.Context(), started.ID)
			if err != nil {
				t.Fatalf("takeOver: %v", err)
			}
			if _, err := j.Succeed(t.Context(), taken.ID, taken.LeaseToken, nil); err != nil {
				t.Fatalf("the new holder could not settle: %v", err)
			}
		})
	}

	t.Run("a renewal without a token", func(t *testing.T) {
		j, _ := newTestJournal(t)
		_, err := j.renewLease(t.Context(), &Intent{ID: "x"})
		assertCode(t, err, ErrorCodeInvalidArgument)
		_, err = j.renewLease(t.Context(), nil)
		assertCode(t, err, ErrorCodeInvalidArgument)
	})
}

func TestResumeRefusesANilProviderBeforeTakingOver(t *testing.T) {
	j, db, clock := newClockedJournal(t)
	executor, err := NewExecutor(j)
	if err != nil {
		t.Fatalf("NewExecutor: %v", err)
	}
	started := mustStart(t, j, mustPrepare(t, j, validPrepare(1)).ID)
	lapseLeases(clock)

	_, err = executor.Resume(t.Context(), started.ID, nil, nil)

	assertCode(t, err, ErrorCodeInvalidArgument)
	if _, token, _ := leaseColumns(t, db, started.ID); token != started.LeaseToken {
		t.Fatal("a refused resume replaced the lease token")
	}
}

func TestRecoverIntentsClaimsOnlyTheNamedIntents(t *testing.T) {
	j, _, clock := newClockedJournal(t)
	first := validPrepare(1)
	first.Provider = "email"
	second := validPrepare(2)
	second.Provider = "email"
	early := mustStart(t, j, mustPrepare(t, j, first).ID)
	clock.Advance(30 * time.Second)
	late := mustStart(t, j, mustPrepare(t, j, second).ID)

	report, err := j.RecoverProvider(t.Context(), "email")
	if err != nil || report.Claimed != 0 || len(report.Pending) != 2 {
		t.Fatalf("first pass = %+v, %v; want both leases live and pending", report, err)
	}
	lapseLeases(clock)
	clock.Advance(leaseDuration)
	own := validPrepare(3)
	own.Provider = "email"
	ownStarted := mustStart(t, j, mustPrepare(t, j, own).ID)
	clock.Advance(leaseDuration + time.Second)

	retried, err := j.RecoverIntents(t.Context(), []string{early.ID, late.ID})

	if err != nil || retried.Scanned != 2 || retried.Claimed != 2 {
		t.Fatalf("RecoverIntents = %+v, %v; want the two named intents claimed", retried, err)
	}
	if current, err := j.Get(t.Context(), ownStarted.ID); err != nil || current.State != StateStarted {
		t.Fatalf("an intent that was not named = %+v (%v), want it left started", current, err)
	}
	_, err = j.RecoverIntents(t.Context(), []string{""})
	assertCode(t, err, ErrorCodeInvalidArgument)
}

func TestResumeTakesOverOnlyExpiredLeases(t *testing.T) {
	type fixture struct {
		j     *Journal
		db    *sql.DB
		clock *durabletest.ManualClock
		id    string
	}
	startedIntent := func(t *testing.T) fixture {
		t.Helper()
		j, db, clock := newClockedJournal(t)
		started := mustStart(t, j, mustPrepare(t, j, validPrepare(1)).ID)
		return fixture{j: j, db: db, clock: clock, id: started.ID}
	}
	cases := []struct {
		name     string
		prepare  func(t *testing.T) fixture
		wantCode ErrorCode
		wantRun  bool
	}{
		{name: "a live lease", prepare: startedIntent, wantCode: ErrorCodeInProgress},
		{name: "an expired lease", prepare: func(t *testing.T) fixture {
			f := startedIntent(t)
			lapseLeases(f.clock)
			return f
		}, wantRun: true},
		{name: "a lease that expires exactly now", prepare: func(t *testing.T) fixture {
			f := startedIntent(t)
			f.clock.Advance(leaseDuration)
			return f
		}, wantCode: ErrorCodeInProgress},
		{name: "an intent started before leases existed", prepare: func(t *testing.T) fixture {
			f := startedIntent(t)
			setLease(t, f.db, f.id, nil, nil)
			return f
		}, wantRun: true},
		{name: "a prepared intent", prepare: func(t *testing.T) fixture {
			j, db, clock := newClockedJournal(t)
			return fixture{j: j, db: db, clock: clock, id: mustPrepare(t, j, validPrepare(1)).ID}
		}, wantCode: ErrorCodeTransitionInvalid},
		{name: "an unknown intent", prepare: func(t *testing.T) fixture {
			f := startedIntent(t)
			lapseLeases(f.clock)
			if claimed, err := f.j.Claim(t.Context(), f.id); err != nil || !claimed {
				t.Fatalf("Claim = %v, %v", claimed, err)
			}
			return f
		}, wantCode: ErrorCodeTransitionInvalid},
		{name: "a succeeded intent", prepare: func(t *testing.T) fixture {
			j, db, clock := newClockedJournal(t)
			started := mustStart(t, j, mustPrepare(t, j, validPrepare(1)).ID)
			if _, err := j.Succeed(t.Context(), started.ID, started.LeaseToken, nil); err != nil {
				t.Fatalf("Succeed: %v", err)
			}
			lapseLeases(clock)
			return fixture{j: j, db: db, clock: clock, id: started.ID}
		}, wantCode: ErrorCodeTransitionInvalid},
		{name: "a missing intent", prepare: func(t *testing.T) fixture {
			f := startedIntent(t)
			f.id = "missing"
			return f
		}, wantCode: ErrorCodeNotFound},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := tc.prepare(t)
			executor, err := NewExecutor(f.j)
			if err != nil {
				t.Fatalf("NewExecutor: %v", err)
			}
			provider := &fakeProvider{outcome: Outcome{Succeeded: true, Receipt: json.RawMessage(`{"resumed":true}`)}}
			var before string
			if tc.wantRun {
				_, before, _ = leaseColumns(t, f.db, f.id)
			}

			resumed, err := executor.Resume(t.Context(), f.id, provider, nil)

			if tc.wantCode != "" {
				assertCode(t, err, tc.wantCode)
				if provider.invokeCount != 0 {
					t.Fatalf("the provider ran %d times for a refused resume", provider.invokeCount)
				}
				return
			}
			if err != nil || resumed.State != StateSucceeded {
				t.Fatalf("Resume = %+v, %v; want succeeded", resumed, err)
			}
			if provider.invokeCount != 1 {
				t.Fatalf("the provider ran %d times, want once", provider.invokeCount)
			}
			if _, after, _ := leaseColumns(t, f.db, f.id); after == before {
				t.Fatal("the takeover kept the previous holder's token")
			}
		})
	}
}

func TestResumeSettlesFromEvidenceBeforeRunningTheProvider(t *testing.T) {
	cases := []struct {
		name         string
		settle       Settler
		wantState    State
		wantRuns     int
		wantResult   bool
		wantSettleOK bool
	}{
		{
			name: "evidence of success records its result",
			settle: func(context.Context, *Intent) (Outcome, bool, error) {
				return Outcome{Succeeded: true, Result: json.RawMessage(`{"from":"receipt"}`)}, true, nil
			},
			wantState: StateSucceeded, wantResult: true, wantSettleOK: true,
		},
		{
			name: "evidence of failure fails the intent",
			settle: func(context.Context, *Intent) (Outcome, bool, error) {
				return Outcome{SafeErrorCode: "not_committed"}, true, nil
			},
			wantState: StateFailed, wantSettleOK: true,
		},
		{
			name: "no evidence runs the provider under the new token",
			settle: func(context.Context, *Intent) (Outcome, bool, error) {
				return Outcome{}, false, nil
			},
			wantState: StateSucceeded, wantRuns: 1,
		},
		{
			name:      "no settler runs the provider under the new token",
			settle:    nil,
			wantState: StateSucceeded, wantRuns: 1,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			j, _, clock := newClockedJournal(t)
			executor, err := NewExecutor(j)
			if err != nil {
				t.Fatalf("NewExecutor: %v", err)
			}
			started := mustStart(t, j, mustPrepare(t, j, validPrepare(1)).ID)
			lapseLeases(clock)
			provider := &fakeProvider{outcome: Outcome{Succeeded: true}}

			resumed, err := executor.Resume(t.Context(), started.ID, provider, tc.settle)

			if err != nil || resumed.State != tc.wantState {
				t.Fatalf("Resume = %+v, %v; want %s", resumed, err, tc.wantState)
			}
			if provider.invokeCount != tc.wantRuns {
				t.Fatalf("provider runs = %d, want %d", provider.invokeCount, tc.wantRuns)
			}
			record, recordErr := j.RecordedResult(t.Context(), resumed)
			if tc.wantResult && (recordErr != nil || string(record) != `{"from":"receipt"}`) {
				t.Fatalf("RecordedResult = %s, %v; want the evidence's result", record, recordErr)
			}
			if _, err := j.Succeed(t.Context(), started.ID, started.LeaseToken, nil); err == nil {
				t.Fatal("the previous holder settled after the takeover")
			}
		})
	}

	t.Run("a settler error leaves the record under the new live lease", func(t *testing.T) {
		j, db, clock := newClockedJournal(t)
		executor, err := NewExecutor(j)
		if err != nil {
			t.Fatalf("NewExecutor: %v", err)
		}
		started := mustStart(t, j, mustPrepare(t, j, validPrepare(1)).ID)
		lapseLeases(clock)
		wantErr := errors.New("receipt lookup failed")
		provider := &fakeProvider{}

		_, err = executor.Resume(t.Context(), started.ID, provider, func(context.Context, *Intent) (Outcome, bool, error) {
			return Outcome{}, false, wantErr
		})

		if !errors.Is(err, wantErr) || provider.invokeCount != 0 {
			t.Fatalf("Resume = %v with %d runs, want the settler error and no run", err, provider.invokeCount)
		}
		if state, _, _ := leaseColumns(t, db, started.ID); state != StateStarted {
			t.Fatalf("state = %s, want started", state)
		}
		if got := keeperGoroutines(); got != 0 {
			t.Fatalf("renewal goroutines after a failed settler = %d, want 0", got)
		}
		_, err = executor.Resume(t.Context(), started.ID, provider, nil)
		assertCode(t, err, ErrorCodeInProgress)
	})
}

func TestConcurrentResumersProduceOneHolder(t *testing.T) {
	t.Parallel()
	j, _, clock := newClockedJournal(t)
	started := mustStart(t, j, mustPrepare(t, j, validPrepare(1)).ID)
	lapseLeases(clock)

	const resumers = 8
	var holders, inProgress atomic.Int64
	var wg sync.WaitGroup
	gate := make(chan struct{})
	for range resumers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-gate
			_, err := j.takeOver(context.Background(), started.ID)
			switch {
			case err == nil:
				holders.Add(1)
			case fault.HasCode(err, ErrorCodeInProgress):
				inProgress.Add(1)
			}
		}()
	}
	close(gate)
	wg.Wait()

	if holders.Load() != 1 || inProgress.Load() != resumers-1 {
		t.Fatalf("holders = %d, in progress = %d; want one holder and %d refused", holders.Load(), inProgress.Load(), resumers-1)
	}
}

func TestClaimRequiresExpiredLease(t *testing.T) {
	cases := []struct {
		name        string
		advance     time.Duration
		legacy      bool
		wantClaimed bool
	}{
		{name: "a live lease", advance: 30 * time.Second},
		{name: "a lease that expires exactly now", advance: leaseDuration},
		{name: "a lease expired by a nanosecond", advance: leaseDuration + time.Nanosecond, wantClaimed: true},
		{name: "a long expired lease", advance: time.Hour, wantClaimed: true},
		{name: "an intent started before leases existed", legacy: true, wantClaimed: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			j, db, clock := newClockedJournal(t)
			started := mustStart(t, j, mustPrepare(t, j, validPrepare(1)).ID)
			if tc.legacy {
				setLease(t, db, started.ID, nil, nil)
			}
			clock.Advance(tc.advance)

			claimed, err := j.Claim(t.Context(), started.ID)

			if err != nil || claimed != tc.wantClaimed {
				t.Fatalf("Claim = %v, %v; want %v", claimed, err, tc.wantClaimed)
			}
			want := StateStarted
			if tc.wantClaimed {
				want = StateUnknown
			}
			if state, _, _ := leaseColumns(t, db, started.ID); state != want {
				t.Fatalf("state = %s, want %s", state, want)
			}
		})
	}
}

func TestLeaseExpiryOrdersAcrossFractions(t *testing.T) {
	t.Parallel()
	base := time.Date(2026, 8, 12, 9, 0, 0, 0, time.UTC)
	offsets := []time.Duration{0, time.Nanosecond, 999 * time.Nanosecond, time.Millisecond, 250 * time.Millisecond, 500 * time.Millisecond, 999999999 * time.Nanosecond, time.Second}

	t.Run("the stored text is fixed width and sorts as time", func(t *testing.T) {
		_, db, _ := newClockedJournal(t)
		for i, earlier := range offsets {
			if got := len(fmtLeaseTime(base.Add(earlier))); got != len(leaseTimeLayout) {
				t.Fatalf("fmtLeaseTime(%v) is %d bytes, want %d", earlier, got, len(leaseTimeLayout))
			}
			for _, later := range offsets[i+1:] {
				var ordered bool
				if err := db.QueryRowContext(t.Context(), `SELECT ? < ?`, fmtLeaseTime(base.Add(earlier)), fmtLeaseTime(base.Add(later))).Scan(&ordered); err != nil || !ordered {
					t.Fatalf("%q < %q = %v (%v), want true", fmtLeaseTime(base.Add(earlier)), fmtLeaseTime(base.Add(later)), ordered, err)
				}
			}
		}
	})

	t.Run("a whole second expires before the half second after it", func(t *testing.T) {
		j, db, clock := newClockedJournal(t)
		whole := mustStart(t, j, mustPrepare(t, j, validPrepare(1)).ID)
		half := mustStart(t, j, mustPrepare(t, j, validPrepare(2)).ID)
		setLease(t, db, whole.ID, whole.LeaseToken, fmtLeaseTime(base))
		setLease(t, db, half.ID, half.LeaseToken, fmtLeaseTime(base.Add(500*time.Millisecond)))
		clock.Advance(base.Add(250 * time.Millisecond).Sub(clock.Now()))

		for id, want := range map[string]bool{whole.ID: true, half.ID: false} {
			claimed, err := j.Claim(t.Context(), id)
			if err != nil || claimed != want {
				t.Fatalf("Claim(%s) = %v, %v; want %v", id, claimed, err, want)
			}
		}
	})
}

func TestEffectStateMachineUnchanged(t *testing.T) {
	startIt := func(t *testing.T, j *Journal, prepared *Intent) *Intent {
		t.Helper()
		return mustStart(t, j, prepared.ID)
	}
	unknownIt := func(t *testing.T, j *Journal, prepared *Intent) *Intent {
		t.Helper()
		return mustMarkUnknown(t, j, startIt(t, j, prepared))
	}
	cases := []struct {
		name     string
		do       func(t *testing.T, j *Journal, prepared *Intent) error
		wantCode ErrorCode
	}{
		{name: "prepared to started", do: func(t *testing.T, j *Journal, p *Intent) error { _, err := j.Start(t.Context(), p.ID); return err }},
		{name: "prepared to failed", do: func(t *testing.T, j *Journal, p *Intent) error {
			_, err := j.Fail(t.Context(), p.ID, "", "policy_denied")
			return err
		}},
		{name: "prepared to succeeded", do: func(t *testing.T, j *Journal, p *Intent) error {
			_, err := j.Succeed(t.Context(), p.ID, "", nil)
			return err
		}, wantCode: ErrorCodeTransitionInvalid},
		{name: "prepared to unknown", do: func(t *testing.T, j *Journal, p *Intent) error {
			_, err := j.MarkUnknown(t.Context(), p.ID, "")
			return err
		}, wantCode: ErrorCodeTransitionInvalid},
		{name: "started to succeeded", do: func(t *testing.T, j *Journal, p *Intent) error {
			s := startIt(t, j, p)
			_, err := j.Succeed(t.Context(), s.ID, s.LeaseToken, nil)
			return err
		}},
		{name: "started to failed", do: func(t *testing.T, j *Journal, p *Intent) error {
			s := startIt(t, j, p)
			_, err := j.Fail(t.Context(), s.ID, s.LeaseToken, "provider_4xx")
			return err
		}},
		{name: "started to unknown", do: func(t *testing.T, j *Journal, p *Intent) error {
			s := startIt(t, j, p)
			_, err := j.MarkUnknown(t.Context(), s.ID, s.LeaseToken)
			return err
		}},
		{name: "started to started", do: func(t *testing.T, j *Journal, p *Intent) error {
			startIt(t, j, p)
			_, err := j.Start(t.Context(), p.ID)
			return err
		}, wantCode: ErrorCodeTransitionInvalid},
		{name: "unknown to succeeded by resolution", do: func(t *testing.T, j *Journal, p *Intent) error {
			u := unknownIt(t, j, p)
			_, err := j.Resolve(t.Context(), u.ID, Resolution{Succeeded: true, Receipt: json.RawMessage(`{}`)})
			return err
		}},
		{name: "unknown to failed by resolution", do: func(t *testing.T, j *Journal, p *Intent) error {
			u := unknownIt(t, j, p)
			_, err := j.Resolve(t.Context(), u.ID, Resolution{SafeErrorCode: "not_delivered"})
			return err
		}},
		{name: "unknown to a linked retry", do: func(t *testing.T, j *Journal, p *Intent) error {
			u := unknownIt(t, j, p)
			approval, err := j.Approve(t.Context(), &ApprovalRequest{IntentID: u.ID, Action: ApprovalActionRetry, Reason: "owner approved a fresh attempt", ExpiresIn: time.Minute})
			if err != nil {
				t.Fatalf("Approve: %v", err)
			}
			retry, err := j.RetryWithApproval(t.Context(), u.ID, approval.Token)
			if err != nil {
				return err
			}
			if retry.State != StatePrepared || retry.RetryOf != u.ID || retry.LeaseToken != "" {
				t.Fatalf("retry = %+v, want a new prepared intent linked to the origin", retry)
			}
			if origin, err := j.Get(t.Context(), u.ID); err != nil || origin.State != StateUnknown {
				t.Fatalf("origin = %+v (%v), want it left unknown", origin, err)
			}
			return nil
		}},
		{name: "unknown to succeeded by a holder write", do: func(t *testing.T, j *Journal, p *Intent) error {
			s := startIt(t, j, p)
			mustMarkUnknown(t, j, s)
			_, err := j.Succeed(t.Context(), s.ID, s.LeaseToken, nil)
			return err
		}, wantCode: ErrorCodeTransitionInvalid},
		{name: "started to succeeded by resolution", do: func(t *testing.T, j *Journal, p *Intent) error {
			s := startIt(t, j, p)
			_, err := j.Resolve(t.Context(), s.ID, Resolution{Succeeded: true})
			return err
		}, wantCode: ErrorCodeTransitionInvalid},
		{name: "succeeded stays succeeded", do: func(t *testing.T, j *Journal, p *Intent) error {
			s := startIt(t, j, p)
			if _, err := j.Succeed(t.Context(), s.ID, s.LeaseToken, nil); err != nil {
				t.Fatalf("Succeed: %v", err)
			}
			_, err := j.Fail(t.Context(), s.ID, s.LeaseToken, "late")
			return err
		}, wantCode: ErrorCodeTransitionInvalid},
		{name: "failed stays failed", do: func(t *testing.T, j *Journal, p *Intent) error {
			s := startIt(t, j, p)
			if _, err := j.Fail(t.Context(), s.ID, s.LeaseToken, "provider_4xx"); err != nil {
				t.Fatalf("Fail: %v", err)
			}
			_, err := j.Succeed(t.Context(), s.ID, s.LeaseToken, nil)
			return err
		}, wantCode: ErrorCodeTransitionInvalid},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			j, _ := newTestJournal(t)
			err := tc.do(t, j, mustPrepare(t, j, validPrepare(1)))
			if tc.wantCode == "" {
				if err != nil {
					t.Fatalf("transition failed: %v", err)
				}
				return
			}
			assertCode(t, err, tc.wantCode)
		})
	}
}

func TestResolveRecordsTheResultOfARecoveredSuccess(t *testing.T) {
	cases := []struct {
		name        string
		result      json.RawMessage
		wantRecord  bool
		wantNotNote bool
	}{
		{name: "a recordable result", result: json.RawMessage(`{"tool_name":"exec"}`), wantRecord: true},
		{name: "no result", result: nil},
		{name: "a result that is not JSON", result: json.RawMessage(`{bad`), wantNotNote: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			j, _ := newTestJournal(t)
			unknown := mustMarkUnknown(t, j, mustStart(t, j, mustPrepare(t, j, validPrepare(1)).ID))

			resolved, err := j.Resolve(t.Context(), unknown.ID, Resolution{Succeeded: true, Receipt: json.RawMessage(`{"id":1}`), Result: tc.result})

			if err != nil || resolved.State != StateSucceeded || resolved.ReconciledAt == nil {
				t.Fatalf("Resolve = %+v, %v; want a reconciled success", resolved, err)
			}
			record, recordErr := j.RecordedResult(t.Context(), resolved)
			if tc.wantRecord {
				if recordErr != nil || string(record) != string(tc.result) {
					t.Fatalf("RecordedResult = %s, %v; want %s", record, recordErr, tc.result)
				}
				return
			}
			assertCode(t, recordErr, ErrorCodeResultUnavailable)
		})
	}
}

func TestRecoverProviderReportsTheLatestLiveLease(t *testing.T) {
	t.Parallel()
	j, _, clock := newClockedJournal(t)
	first := validPrepare(1)
	first.Provider = "email"
	second := validPrepare(2)
	second.Provider = "email"
	third := validPrepare(3)
	third.Provider = "email"
	mustStart(t, j, mustPrepare(t, j, first).ID)
	clock.Advance(10 * time.Second)
	mustStart(t, j, mustPrepare(t, j, second).ID)
	clock.Advance(10 * time.Second)
	last := mustStart(t, j, mustPrepare(t, j, third).ID)
	clock.Advance(45 * time.Second)

	report, err := j.RecoverProvider(t.Context(), "email")

	if err != nil {
		t.Fatalf("RecoverProvider: %v", err)
	}
	if report.Scanned != 3 || report.Claimed != 1 {
		t.Fatalf("report = %+v, want three scanned and the expired one claimed", report)
	}
	if want := last.StartedAt.Add(leaseDuration); !report.RetryAt.Equal(want) {
		t.Fatalf("RetryAt = %v, want the latest live expiry %v", report.RetryAt, want)
	}

	clock.Advance(report.RetryAt.Sub(clock.Now()) + time.Nanosecond)
	again, err := j.RecoverProvider(t.Context(), "email")
	if err != nil || again.Claimed != 2 || !again.RetryAt.IsZero() {
		t.Fatalf("second pass = %+v, %v; want the rest claimed and nothing left to retry", again, err)
	}
}

func TestValidateResumeEffectsAnswersALiveLeaseInProgress(t *testing.T) {
	t.Parallel()
	j, _, clock := newClockedJournal(t)
	intent := mustStart(t, j, mustPrepare(t, j, validPrepare(1)).ID)

	err := j.ValidateResumeEffects(t.Context(), "sess-1", "turn-1", []string{intent.ToolCallID})
	assertCode(t, err, ErrorCodeInProgress)
	if state, _ := j.Get(t.Context(), intent.ID); state.State != StateStarted {
		t.Fatalf("state = %s, a live lease must not be claimed", state.State)
	}

	lapseLeases(clock)
	err = j.ValidateResumeEffects(t.Context(), "sess-1", "turn-1", []string{intent.ToolCallID})
	assertCode(t, err, ErrorCodeUnknown)
}

func TestOptionsClockDrivesTheJournal(t *testing.T) {
	t.Parallel()
	j, db, clock := newClockedJournal(t)
	prepared := mustPrepare(t, j, validPrepare(1))
	if !prepared.PreparedAt.Equal(clock.Now()) {
		t.Fatalf("PreparedAt = %v, want the journal clock's %v", prepared.PreparedAt, clock.Now())
	}
	clock.Advance(time.Hour)
	started := mustStart(t, j, prepared.ID)
	if !started.StartedAt.Equal(clock.Now()) {
		t.Fatalf("StartedAt = %v, want the journal clock's %v", started.StartedAt, clock.Now())
	}
	_, _, expiresAt := leaseColumns(t, db, started.ID)
	if want := fmtLeaseTime(clock.Now().Add(leaseDuration)); expiresAt != want {
		t.Fatalf("lease_expires_at = %q, want %q", expiresAt, want)
	}
}
