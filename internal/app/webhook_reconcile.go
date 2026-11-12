package app

import (
	"cmp"
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/anggasct/aura/internal/platform/turn"
	"github.com/anggasct/aura/internal/store"
)

const (
	webhookReconcileInterval  = 5 * time.Second
	webhookReconcileBatch     = 500
	webhookReconcileEventPage = 500
)

type executionOutcome struct {
	state         string
	resultEventID string
	errorCode     string
}

type executionDerivation struct {
	completedMessage string
}

func (d *executionDerivation) next(event *store.RuntimeEvent) (outcome executionOutcome, terminal bool) {
	switch event.Kind {
	case turn.EventKindTurnAccepted:
		return executionOutcome{}, false
	case turn.EventKindMessageCompleted:
		d.completedMessage = event.ID
		return executionOutcome{state: store.WebhookExecutionStateRunning}, false
	case turn.EventKindTurnCompleted:
		return executionOutcome{state: store.WebhookExecutionStateCompleted, resultEventID: firstNonEmpty(d.completedMessage, event.ID)}, true
	case turn.EventKindTurnFailed:
		return executionOutcome{
			state:         store.WebhookExecutionStateFailed,
			resultEventID: firstNonEmpty(d.completedMessage, event.ID),
			errorCode:     failedEventCode(event.Payload),
		}, true
	case turn.EventKindTurnCancelled:
		return executionOutcome{state: store.WebhookExecutionStateCancelled, errorCode: string(turn.ErrorCodeTurnCancelled)}, true
	default:
		return executionOutcome{state: store.WebhookExecutionStateRunning}, false
	}
}

type reconcileResult struct {
	admitted bool
	settled  bool
}

type storedTurnOutcome struct {
	outcome  executionOutcome
	admitted bool
	terminal bool
}

type webhookReconciler struct {
	sessions   store.SessionService
	executions store.WebhookExecutionStore
	clock      func() time.Time
	page       int
}

func (r *webhookReconciler) reconcile(ctx context.Context, execution *store.WebhookExecution) (reconcileResult, error) {
	stored, err := r.storedOutcome(ctx, execution)
	if err != nil {
		return reconcileResult{}, err
	}
	result := reconcileResult{admitted: stored.admitted}
	if stored.outcome.state == execution.State {
		return result, nil
	}
	changed, err := r.write(ctx, execution.ID, &stored.outcome)
	if err != nil {
		return result, err
	}
	result.settled = changed && stored.terminal
	return result, nil
}

func (r *webhookReconciler) storedOutcome(ctx context.Context, execution *store.WebhookExecution) (storedTurnOutcome, error) {
	var stored storedTurnOutcome
	var derivation executionDerivation
	after := uint64(0)
	for {
		events, err := r.sessions.ListEvents(ctx, execution.SessionID, after, r.page)
		if err != nil {
			return storedTurnOutcome{}, fmt.Errorf("read the events of webhook execution %s: %w", execution.ID, err)
		}
		for i := range events {
			if events[i].TurnID != execution.TurnID {
				continue
			}
			stored.admitted = true
			outcome, terminal := derivation.next(&events[i])
			if outcome.state != "" {
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

func (r *webhookReconciler) write(ctx context.Context, executionID string, outcome *executionOutcome) (bool, error) {
	now := r.clock().UTC()
	switch outcome.state {
	case "":
		return false, nil
	case store.WebhookExecutionStateRunning:
		return r.executions.MarkRunning(ctx, executionID, now)
	default:
		return r.executions.MarkTerminal(ctx, executionID, outcome.state, outcome.resultEventID, outcome.errorCode, now)
	}
}

type unfinishedExecutionLister interface {
	ListUnfinished(ctx context.Context, query *store.UnfinishedExecutionQuery) ([]store.WebhookExecution, error)
}

type reconcileTally struct {
	settled int
	failed  int
	first   error
}

func (t *reconcileTally) add(result reconcileResult, err error) {
	switch {
	case err != nil:
		t.failed++
		t.first = cmp.Or(t.first, err)
	case result.settled:
		t.settled++
	}
}

type webhookReconcilePass struct {
	executions unfinishedExecutionLister
	reconcile  func(ctx context.Context, execution *store.WebhookExecution) (reconcileResult, error)
	clock      func() time.Time
	batch      int
	interval   time.Duration
	logger     *slog.Logger
}

func (d *webhookDispatcher) reconcilePass() *webhookReconcilePass {
	return &webhookReconcilePass{
		executions: d.executions,
		reconcile:  d.reconciler.reconcile,
		clock:      d.clock,
		batch:      webhookReconcileBatch,
		interval:   webhookReconcileInterval,
		logger:     d.logger,
	}
}

func (p *webhookReconcilePass) run(ctx context.Context) {
	p.pass(ctx)
	ticker := time.NewTicker(p.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			p.pass(ctx)
		}
	}
}

func (p *webhookReconcilePass) pass(ctx context.Context) {
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

func (p *webhookReconcilePass) settle(ctx context.Context) (reconcileTally, error) {
	var tally reconcileTally
	query := &store.UnfinishedExecutionQuery{Now: p.clock().UTC(), Limit: p.batch}
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
