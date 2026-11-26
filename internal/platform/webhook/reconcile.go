package webhook

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/anggasct/aura/internal/platform/turn"
)

const (
	DefaultReconcileInterval  = 5 * time.Second
	DefaultReconcileBatch     = 500
	DefaultReconcileEventPage = 500
)

type EventLister interface {
	ListSessionEvents(ctx context.Context, sessionID string, filter turn.EventFilter) ([]turn.Event, error)
}

type UnfinishedExecutionLister interface {
	ListUnfinished(ctx context.Context, query *UnfinishedQuery) ([]Execution, error)
}

type ExecutionOutcome struct {
	State         string
	ResultEventID string
	ErrorCode     string
}

type ExecutionDerivation struct {
	completedMessage string
}

func (d *ExecutionDerivation) Next(event *turn.Event) (outcome ExecutionOutcome, terminal bool) {
	if event == nil {
		return ExecutionOutcome{}, false
	}
	switch event.Kind {
	case turn.EventKindTurnAccepted:
		return ExecutionOutcome{}, false
	case turn.EventKindMessageCompleted:
		d.completedMessage = event.ID
		return ExecutionOutcome{State: StateRunning}, false
	case turn.EventKindTurnCompleted:
		return ExecutionOutcome{State: StateCompleted, ResultEventID: firstNonEmpty(d.completedMessage, event.ID)}, true
	case turn.EventKindTurnFailed:
		return ExecutionOutcome{
			State:         StateFailed,
			ResultEventID: firstNonEmpty(d.completedMessage, event.ID),
			ErrorCode:     failedEventCode(event.Payload),
		}, true
	case turn.EventKindTurnCancelled:
		return ExecutionOutcome{State: StateCancelled, ErrorCode: string(turn.ErrorCodeTurnCancelled)}, true
	default:
		return ExecutionOutcome{State: StateRunning}, false
	}
}

type ReconcileResult struct {
	Admitted bool
	Settled  bool
}

type storedTurnOutcome struct {
	outcome  ExecutionOutcome
	admitted bool
	terminal bool
}

type ReconcilerDeps struct {
	Events     EventLister
	Executions ExecutionStore
	Clock      func() time.Time
	EventPage  int
}

type Reconciler struct {
	events     EventLister
	executions ExecutionStore
	clock      func() time.Time
	page       int
}

func NewReconciler(deps *ReconcilerDeps) (*Reconciler, error) {
	if deps == nil {
		return nil, Errorf(ErrorCodeInvalidArgument, "reconciler dependencies must not be nil")
	}
	if deps.Events == nil {
		return nil, Errorf(ErrorCodeInvalidArgument, "event lister must not be nil")
	}
	if deps.Executions == nil {
		return nil, Errorf(ErrorCodeInvalidArgument, "execution store must not be nil")
	}
	if deps.Clock == nil {
		return nil, Errorf(ErrorCodeInvalidArgument, "clock must not be nil")
	}
	page := deps.EventPage
	if page <= 0 {
		page = DefaultReconcileEventPage
	}
	return &Reconciler{
		events:     deps.Events,
		executions: deps.Executions,
		clock:      deps.Clock,
		page:       page,
	}, nil
}

func (r *Reconciler) Reconcile(ctx context.Context, execution *Execution) (ReconcileResult, error) {
	if execution == nil {
		return ReconcileResult{}, Errorf(ErrorCodeInvalidArgument, "execution must not be nil")
	}
	stored, err := r.storedOutcome(ctx, execution)
	if err != nil {
		return ReconcileResult{}, err
	}
	result := ReconcileResult{Admitted: stored.admitted}
	if stored.outcome.State == execution.State {
		return result, nil
	}
	changed, err := r.Write(ctx, execution.ID, &stored.outcome)
	if err != nil {
		return result, err
	}
	result.Settled = changed && stored.terminal
	return result, nil
}

func (r *Reconciler) storedOutcome(ctx context.Context, execution *Execution) (storedTurnOutcome, error) {
	var stored storedTurnOutcome
	var derivation ExecutionDerivation
	after := uint64(0)
	for {
		events, err := r.events.ListSessionEvents(ctx, execution.SessionID, turn.EventFilter{
			AfterSequence: after,
			Limit:         r.page,
		})
		if err != nil {
			return storedTurnOutcome{}, fmt.Errorf("read the events of webhook execution %s: %w", execution.ID, err)
		}
		for i := range events {
			if events[i].TurnID != execution.TurnID {
				continue
			}
			stored.admitted = true
			outcome, terminal := derivation.Next(&events[i])
			if outcome.State != "" {
				stored.outcome = outcome
			}
			if terminal {
				stored.terminal = true
				return stored, nil
			}
		}
		if len(events) < r.page {
			return stored, nil
		}
		after = events[len(events)-1].Sequence
	}
}

func (r *Reconciler) Write(ctx context.Context, executionID string, outcome *ExecutionOutcome) (bool, error) {
	if outcome == nil {
		return false, Errorf(ErrorCodeInvalidArgument, "outcome must not be nil")
	}
	now := r.clock().UTC()
	switch outcome.State {
	case "":
		return false, nil
	case StateRunning:
		return r.executions.MarkRunning(ctx, executionID, now)
	default:
		return r.executions.MarkTerminal(ctx, executionID, outcome.State, outcome.ResultEventID, outcome.ErrorCode, now)
	}
}

type reconcileTally struct {
	settled int
	failed  int
	first   error
}

func (t *reconcileTally) add(result ReconcileResult, err error) {
	switch {
	case err != nil:
		t.failed++
		t.first = cmp.Or(t.first, err)
	case result.Settled:
		t.settled++
	}
}

type ReconcilePassOptions struct {
	Executions UnfinishedExecutionLister
	Reconciler *Reconciler
	Reconcile  func(ctx context.Context, execution *Execution) (ReconcileResult, error)
	Clock      func() time.Time
	Batch      int
	Interval   time.Duration
	Logger     *slog.Logger
}

type ReconcilePass struct {
	executions UnfinishedExecutionLister
	reconcile  func(ctx context.Context, execution *Execution) (ReconcileResult, error)
	clock      func() time.Time
	batch      int
	interval   time.Duration
	logger     *slog.Logger
}

func NewReconcilePass(opts *ReconcilePassOptions) (*ReconcilePass, error) {
	if opts == nil {
		return nil, Errorf(ErrorCodeInvalidArgument, "reconcile pass options must not be nil")
	}
	if opts.Executions == nil {
		return nil, Errorf(ErrorCodeInvalidArgument, "execution lister must not be nil")
	}
	if opts.Clock == nil {
		return nil, Errorf(ErrorCodeInvalidArgument, "clock must not be nil")
	}
	reconcileFn := opts.Reconcile
	if reconcileFn == nil {
		if opts.Reconciler == nil {
			return nil, Errorf(ErrorCodeInvalidArgument, "reconciler must not be nil")
		}
		reconcileFn = opts.Reconciler.Reconcile
	}
	batch := opts.Batch
	if batch <= 0 {
		batch = DefaultReconcileBatch
	}
	interval := opts.Interval
	if interval <= 0 {
		interval = DefaultReconcileInterval
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &ReconcilePass{
		executions: opts.Executions,
		reconcile:  reconcileFn,
		clock:      opts.Clock,
		batch:      batch,
		interval:   interval,
		logger:     logger,
	}, nil
}

func (p *ReconcilePass) Run(ctx context.Context) {
	p.Pass(ctx)
	ticker := time.NewTicker(p.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			p.Pass(ctx)
		}
	}
}

func (p *ReconcilePass) Pass(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}
	tally, err := p.settle(ctx)
	if ctx.Err() != nil {
		return
	}
	if err != nil || tally.failed > 0 {
		p.logger.WarnContext(ctx, "webhook reconcile failed",
			"component", "webhook",
			"settled", tally.settled,
			"failed", tally.failed,
			"error", cmp.Or(tally.first, err),
		)
		return
	}
	if tally.settled > 0 {
		p.logger.InfoContext(ctx, "webhook executions reconciled", "component", "webhook", "settled", tally.settled)
	}
}

func (p *ReconcilePass) settle(ctx context.Context) (reconcileTally, error) {
	var tally reconcileTally
	query := &UnfinishedQuery{Now: p.clock().UTC(), Limit: p.batch}
	for {
		executions, err := p.executions.ListUnfinished(ctx, query)
		if err != nil {
			return tally, fmt.Errorf("list unfinished webhook executions: %w", err)
		}
		for i := range executions {
			if ctx.Err() != nil {
				return tally, ctx.Err()
			}
			tally.add(p.reconcile(ctx, &executions[i]))
		}
		if len(executions) < p.batch {
			return tally, nil
		}
		query.AfterID = executions[len(executions)-1].ID
	}
}

func failedEventCode(payload []byte) string {
	var decoded struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(payload, &decoded); err != nil || decoded.Code == "" {
		return string(turn.ErrorCodeRuntimeInternal)
	}
	return decoded.Code
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
