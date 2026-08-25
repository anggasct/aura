package health

import (
	"cmp"
	"context"
	"errors"
	"slices"
	"sync"
	"time"
)

// StatusNone marks a finding identity with no previously emitted state; the
// first observation of a new identity transitions from here so the durable
// log carries complete state history.
const StatusNone Status = ""

// Transition is one durable finding state change. It carries only stable
// contract fields — identity, the two statuses, the code in effect, and the
// time — never evidence detail.
type Transition struct {
	FindingID string    `json:"finding_id"`
	From      Status    `json:"from"`
	To        Status    `json:"to"`
	Code      string    `json:"code"`
	At        time.Time `json:"at"`
}

// TransitionPolicy shapes flap resistance. A candidate state must survive
// StableFor since first seen before it commits (debounce), and successive
// emissions for one finding identity wait at least Cooldown apart.
type TransitionPolicy struct {
	StableFor time.Duration
	Cooldown  time.Duration
}

// EventSink receives committed transitions. A failed sink leaves the tracker's
// current state untouched, so the same transition is attempted again on the
// next observation until it is durably recorded.
type EventSink func(ctx context.Context, t *Transition) error

type pendingTransition struct {
	finding   Finding
	firstSeen time.Time
}

// StateTracker turns raw observations into durable, flap-resistant
// transitions. It is safe for concurrent use; observations may arrive from
// any evaluation path.
type StateTracker struct {
	policy TransitionPolicy
	now    func() time.Time
	sink   EventSink

	mu       sync.Mutex
	current  map[string]Finding
	pending  map[string]pendingTransition
	lastEmit map[string]time.Time
}

// NewStateTracker builds a tracker. History seeds prior state so a restart
// neither re-emits old states nor forgets the baseline to compare against;
// nil history starts empty.
func NewStateTracker(policy TransitionPolicy, sink EventSink, history func(ctx context.Context) ([]Transition, error)) (*StateTracker, error) {
	if sink == nil {
		return nil, errors.New("health: transition sink must not be nil")
	}
	if policy.StableFor <= 0 {
		policy.StableFor = 30 * time.Second
	}
	if policy.Cooldown <= 0 {
		policy.Cooldown = time.Minute
	}
	tracker := &StateTracker{
		policy:   policy,
		now:      func() time.Time { return time.Now().UTC() },
		sink:     sink,
		current:  make(map[string]Finding),
		pending:  make(map[string]pendingTransition),
		lastEmit: make(map[string]time.Time),
	}
	if history != nil {
		past, err := history(context.Background())
		if err != nil {
			return nil, err
		}
		for _, t := range past {
			tracker.current[t.FindingID] = Finding{ID: t.FindingID, Code: t.Code, Status: t.To}
			tracker.lastEmit[t.FindingID] = t.At
		}
	}
	return tracker, nil
}

// SetClock overrides the time source; tests use this instead of sleeping.
func (t *StateTracker) SetClock(now func() time.Time) { t.now = now }

// Observe folds one evaluation's findings into the tracker. Committed
// transitions are returned alongside any sink persistence error; the error
// does not advance state, so the next observation retries the same
// transition.
func (t *StateTracker) Observe(ctx context.Context, findings []Finding) (committed []Transition, err error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()

	for i := range findings {
		finding := &findings[i]
		id := finding.ID

		current, tracked := t.current[id]
		if !tracked {
			transition := Transition{FindingID: id, From: StatusNone, To: finding.Status, Code: finding.Code, At: now}
			emitErr := t.emit(ctx, &transition, id, now)
			if emitErr != nil {
				return committed, emitErr
			}
			committed = append(committed, transition)
			delete(t.pending, id)
			continue
		}
		if finding.Status == current.Status {
			delete(t.pending, id)
			continue
		}

		candidate, pendingExists := t.pending[id]
		if !pendingExists || candidate.finding.Status != finding.Status {
			candidate = pendingTransition{finding: *finding, firstSeen: now}
			t.pending[id] = candidate
		}
		if now.Sub(candidate.firstSeen) < t.policy.StableFor {
			continue
		}
		if last, emitted := t.lastEmit[id]; emitted && now.Sub(last) < t.policy.Cooldown {
			continue
		}

		transition := Transition{FindingID: id, From: current.Status, To: finding.Status, Code: finding.Code, At: now}
		emitErr := t.emit(ctx, &transition, id, now)
		if emitErr != nil {
			return committed, emitErr
		}
		committed = append(committed, transition)
		delete(t.pending, id)
	}
	return committed, nil
}

func (t *StateTracker) emit(ctx context.Context, transition *Transition, id string, now time.Time) error {
	if err := t.sink(ctx, transition); err != nil {
		return err
	}
	t.current[id] = Finding{ID: id, Code: transition.Code, Status: transition.To}
	t.lastEmit[id] = now
	return nil
}

// Snapshot returns the currently tracked states, oldest ID first.
func (t *StateTracker) Snapshot() []Finding {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]Finding, 0, len(t.current))
	for id := range t.current {
		out = append(out, t.current[id])
	}
	slices.SortFunc(out, func(a, b Finding) int { return cmp.Compare(a.ID, b.ID) })
	return out
}
