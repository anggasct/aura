package health

import (
	"cmp"
	"context"
	"errors"
	"slices"
	"sync"
	"time"
)

const StatusNone Status = ""

type Transition struct {
	FindingID string    `json:"finding_id"`
	From      Status    `json:"from"`
	To        Status    `json:"to"`
	Code      string    `json:"code"`
	At        time.Time `json:"at"`
}

type TransitionPolicy struct {
	StableFor time.Duration
	Cooldown  time.Duration
}

type EventSink func(ctx context.Context, t *Transition) error

type pendingTransition struct {
	finding   Finding
	firstSeen time.Time
}

type StateTracker struct {
	policy TransitionPolicy
	now    func() time.Time
	sink   EventSink

	mu       sync.Mutex
	current  map[string]Finding
	pending  map[string]pendingTransition
	lastEmit map[string]time.Time
}

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

func (t *StateTracker) SetClock(now func() time.Time) { t.now = now }

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
