package schedule

import (
	"context"
	"encoding/json"
	"log/slog"
	"strconv"
	"time"

	"github.com/anggasct/aura/internal/platform/durable"
	"github.com/anggasct/aura/internal/platform/principal"
)

const HandlerName = "schedule"

const overlapPollInterval = 30 * time.Second

const (
	PriorityInfo    = "info"
	PriorityWarning = "warning"
)

type TurnRequest struct {
	SessionID      string
	PrincipalID    string
	Origin         string
	Prompt         string
	IdempotencyKey string
	WorkflowID     string
	WorkflowInput  string
}

type TurnResult struct {
	TurnID        string
	Text          string
	Succeeded     bool
	SafeErrorCode string
}

type TurnRunner interface {
	RunTurn(ctx context.Context, req *TurnRequest) (TurnResult, error)
}

type NotifyRequest struct {
	Producer         string
	Key              string
	Priority         string
	DestinationAlias string
	Text             string
}

type Notifier interface {
	Notify(ctx context.Context, req *NotifyRequest) error
}

type RunStore interface {
	JobStore
	ActiveOccurrence(ctx context.Context, jobID string) (Occurrence, bool, error)
	AttachTurn(ctx context.Context, id, turnID string, now time.Time) error
	SettleOccurrence(ctx context.Context, id, state, turnID, resultEventID, safeErrorCode string, now time.Time) error
	PruneOccurrences(ctx context.Context, jobID string, before time.Time, limit int) (int, error)
}

const defaultRetention = 720 * time.Hour

const pruneBatchLimit = 100

const (
	actionNext   = "next"
	actionNotify = "notify"
	actionTurn   = "turn/"
)

const (
	notifySent   = "sent"
	notifyFailed = "failed"
	nextNone     = "none"
	nextStarted  = "started"
)

type RunnerDeps struct {
	Jobs      RunStore
	Turns     TurnRunner
	Notify    Notifier
	Starter   Starter
	Logger    *slog.Logger
	Retention time.Duration
	Observer  Observer
}

type Runner struct {
	jobs      RunStore
	turns     TurnRunner
	notify    Notifier
	starter   Starter
	overlap   time.Duration
	retention time.Duration
	logger    *slog.Logger
	principal string
	now       func() time.Time
	observer  Observer
}

func NewRunner(deps *RunnerDeps) (*Runner, error) {
	if deps == nil {
		return nil, Errorf(ErrorCodeInvalidArgument, "runner dependencies must not be nil")
	}
	if deps.Jobs == nil {
		return nil, Errorf(ErrorCodeInvalidArgument, "run store must not be nil")
	}
	if deps.Turns == nil {
		return nil, Errorf(ErrorCodeInvalidArgument, "turn runner must not be nil")
	}
	if deps.Notify == nil {
		return nil, Errorf(ErrorCodeInvalidArgument, "notifier must not be nil")
	}
	if deps.Starter == nil {
		return nil, Errorf(ErrorCodeInvalidArgument, "run starter must not be nil")
	}
	retention := deps.Retention
	if retention <= 0 {
		retention = defaultRetention
	}
	logger := deps.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &Runner{
		jobs: deps.Jobs, turns: deps.Turns, notify: deps.Notify, starter: deps.Starter,
		overlap: overlapPollInterval, retention: retention, logger: logger,
		principal: principal.OwnerID, observer: deps.Observer,
		now: func() time.Time { return time.Now().UTC() },
	}, nil
}

type step int

const (
	stepEnd step = iota
	stepNext
	stepDrive
)

func (r *Runner) Handle(ctx context.Context, inv durable.Invocation) error {
	if ctx == nil {
		return Errorf(ErrorCodeInvalidArgument, "context must not be nil")
	}
	if inv == nil {
		return Errorf(ErrorCodeInvalidArgument, "invocation must not be nil")
	}
	fire, err := decodeFire(inv.Payload())
	if err != nil {
		return err
	}
	if err := inv.Sleep(max(fire.Instant.Sub(r.now()), 0)); err != nil {
		return err
	}
	next, occurrence, err := r.locate(ctx, inv, &fire)
	if err != nil || next == stepEnd {
		return err
	}
	if next == stepDrive {
		if err := r.drive(ctx, inv, &occurrence); err != nil {
			return err
		}
	}
	return r.startSuccessor(ctx, inv, &fire)
}

func (r *Runner) locate(ctx context.Context, inv durable.Invocation, fire *firePayload) (step, Occurrence, error) {
	occurrence, found, err := r.jobs.OccurrenceByFire(ctx, fire.JobID, fire.Instant)
	switch {
	case err != nil:
		return stepEnd, Occurrence{}, err
	case found && occurrence.State != OccurrenceFired:
		return stepNext, occurrence, nil
	case found:
		r.observeFired(ctx, fire.Instant)
		return stepDrive, occurrence, nil
	case !fire.Chain:
		return stepEnd, Occurrence{}, nil
	}
	return r.gate(ctx, inv, fire)
}

func (r *Runner) gate(ctx context.Context, inv durable.Invocation, fire *firePayload) (step, Occurrence, error) {
	job, live, err := r.liveJob(ctx, fire.JobID)
	if err != nil || !live {
		return stepEnd, Occurrence{}, err
	}
	scheduled, err := firesAt(&job, fire)
	if err != nil || !scheduled {
		return stepNext, Occurrence{}, err
	}
	r.observeFired(ctx, fire.Instant)
	if r.now().Sub(fire.Instant) > graceOf(&job) {
		return r.record(ctx, &recording{job: &job, instant: fire.Instant, state: OccurrenceExpired})
	}
	verdict, err := r.checkOverlap(ctx, inv, &job)
	switch {
	case err != nil:
		return stepEnd, Occurrence{}, err
	case verdict == overlapEnd:
		return stepEnd, Occurrence{}, nil
	case verdict == overlapSkip:
		return r.record(ctx, &recording{job: &job, instant: fire.Instant, state: OccurrenceSkippedOverlap})
	}
	return r.record(ctx, &recording{job: &job, instant: fire.Instant, state: OccurrenceFired})
}

func (r *Runner) liveJob(ctx context.Context, jobID string) (Job, bool, error) {
	job, found, err := r.jobs.Job(ctx, jobID)
	if err != nil {
		return Job{}, false, err
	}
	if !found {
		r.logger.WarnContext(ctx, "schedule job missing", "component", "schedule")
		return Job{}, false, nil
	}
	return job, job.State != JobDeleted && job.State != JobPaused, nil
}

func firesAt(job *Job, fire *firePayload) (bool, error) {
	if job.CronExpression == fire.Expression && job.Timezone == fire.Timezone {
		return true, nil
	}
	next, err := nextFireOf(job, fire.Instant.Add(-time.Nanosecond))
	if err != nil {
		return false, err
	}
	return next.Equal(fire.Instant), nil
}

func graceOf(job *Job) time.Duration {
	return time.Duration(job.CatchUpGraceSeconds) * time.Second
}

type overlapVerdict int

const (
	overlapProceed overlapVerdict = iota
	overlapSkip
	overlapEnd
)

func (r *Runner) checkOverlap(ctx context.Context, inv durable.Invocation, job *Job) (overlapVerdict, error) {
	if job.OverlapPolicy == OverlapQueueOne {
		proceed, err := r.awaitActive(ctx, inv, job.ID)
		if err != nil || !proceed {
			return overlapEnd, err
		}
		return overlapProceed, nil
	}
	_, found, err := r.jobs.ActiveOccurrence(ctx, job.ID)
	if err != nil {
		return overlapEnd, err
	}
	if found {
		return overlapSkip, nil
	}
	return overlapProceed, nil
}

func (r *Runner) awaitActive(ctx context.Context, inv durable.Invocation, jobID string) (bool, error) {
	for {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		_, live, err := r.liveJob(ctx, jobID)
		if err != nil || !live {
			return false, err
		}
		_, found, err := r.jobs.ActiveOccurrence(ctx, jobID)
		if err != nil {
			return false, err
		}
		if !found {
			return true, nil
		}
		if err := inv.Sleep(r.overlap); err != nil {
			return false, err
		}
	}
}

type recording struct {
	job     *Job
	instant time.Time
	state   string
}

func (r *Runner) record(ctx context.Context, rec *recording) (step, Occurrence, error) {
	now := r.now()
	occurrence := Occurrence{
		ID: occurrenceID(rec.job.ID, rec.instant), JobID: rec.job.ID, JobVersion: rec.job.Version,
		ScheduledForUTC: rec.instant.UTC(), State: rec.state, CreatedAt: now, UpdatedAt: now,
	}
	if err := r.jobs.RecordFire(ctx, &occurrence); err != nil {
		return r.recordedElsewhere(ctx, &occurrence, err)
	}
	if rec.state == OccurrenceFired {
		return stepDrive, occurrence, nil
	}
	r.settled(ctx, &occurrence, now)
	return stepNext, occurrence, nil
}

func (r *Runner) recordedElsewhere(ctx context.Context, occurrence *Occurrence, recordErr error) (step, Occurrence, error) {
	if code, ok := CodeOf(recordErr); !ok || code != ErrorCodeOccurrenceConflict {
		return stepEnd, Occurrence{}, recordErr
	}
	existing, found, err := r.jobs.OccurrenceByFire(ctx, occurrence.JobID, occurrence.ScheduledForUTC)
	switch {
	case err != nil:
		return stepEnd, Occurrence{}, err
	case !found:
		return stepEnd, Occurrence{}, recordErr
	case existing.State == OccurrenceFired:
		return stepDrive, existing, nil
	}
	return stepNext, existing, nil
}

func (r *Runner) drive(ctx context.Context, inv durable.Invocation, occurrence *Occurrence) error {
	outcome := turnOutcome{}
	for attempt := 1; ; attempt++ {
		var err error
		outcome, err = r.submitTurn(ctx, inv, &submission{occurrence: occurrence, attempt: attempt, first: outcome.First})
		if err != nil {
			return err
		}
		if outcome.Kind != outcomeOverloaded {
			return r.conclude(ctx, inv, &turnEnd{occurrence: occurrence, outcome: &outcome})
		}
		r.observe(ctx, &Observation{State: OccurrenceFired, Result: ResultRetried})
		if err := inv.Sleep(r.pause(&outcome)); err != nil {
			return err
		}
	}
}

func (r *Runner) pause(outcome *turnOutcome) time.Duration {
	return min(r.overlap, max(outcome.Deadline.Sub(r.now()), 0))
}

func (r *Runner) submitTurn(ctx context.Context, inv durable.Invocation, sub *submission) (turnOutcome, error) {
	raw, err := inv.RunAction(ctx, actionTurn+strconv.Itoa(sub.attempt), func(actionCtx context.Context) ([]byte, error) {
		return r.decideTurn(actionCtx, sub)
	})
	if err != nil {
		return turnOutcome{}, err
	}
	var outcome turnOutcome
	if err := json.Unmarshal(raw, &outcome); err != nil {
		return turnOutcome{}, Errorf(ErrorCodeInvalidArgument, "journaled turn outcome is not valid")
	}
	return outcome, nil
}

func (r *Runner) decideTurn(ctx context.Context, sub *submission) ([]byte, error) {
	job, found, err := r.jobs.Job(ctx, sub.occurrence.JobID)
	if err != nil {
		return nil, err
	}
	if !found || job.State == JobDeleted {
		return json.Marshal(turnOutcome{Kind: outcomeCancelled})
	}
	window := admissionOf(&job, sub, r.now())
	if window.expired {
		return json.Marshal(turnOutcome{Kind: outcomeExpired})
	}
	result, err := r.turns.RunTurn(ctx, &TurnRequest{
		SessionID: "cron:" + job.ID, PrincipalID: r.principal, Origin: "internal", Prompt: job.Prompt,
		IdempotencyKey: turnKey(job.ID, sub.occurrence.ScheduledForUTC),
		WorkflowID:     job.WorkflowID, WorkflowInput: job.WorkflowInput,
	})
	if durable.Preempted(ctx) {
		return nil, context.Cause(ctx)
	}
	if err != nil {
		if code, ok := CodeOf(err); ok && code == ErrorCodeRuntimeOverloaded {
			return json.Marshal(turnOutcome{Kind: outcomeOverloaded, First: window.first, Deadline: window.deadline})
		}
		return nil, err
	}
	return json.Marshal(resultOutcome(&result, job.OriginDestination))
}

type turnEnd struct {
	occurrence *Occurrence
	outcome    *turnOutcome
}

func (r *Runner) conclude(ctx context.Context, inv durable.Invocation, end *turnEnd) error {
	occurrence, outcome := end.occurrence, end.outcome
	switch outcome.Kind {
	case outcomeExpired:
		return r.settle(ctx, occurrence, &settlement{state: OccurrenceExpired})
	case outcomeCancelled:
		return r.settle(ctx, occurrence, &settlement{state: OccurrenceCancelled})
	}
	if err := r.jobs.AttachTurn(ctx, occurrence.ID, outcome.TurnID, r.now()); err != nil {
		return settledElsewhere(err)
	}
	if err := r.notifyOutcome(ctx, inv, end); err != nil {
		return err
	}
	terminal := settlement{state: OccurrenceCompleted, turnID: outcome.TurnID}
	if !outcome.Succeeded {
		terminal = settlement{state: OccurrenceFailed, turnID: outcome.TurnID, safeErrorCode: outcome.SafeErrorCode}
	}
	return r.settle(ctx, occurrence, &terminal)
}

func (r *Runner) notifyOutcome(ctx context.Context, inv durable.Invocation, n *turnEnd) error {
	priority := PriorityInfo
	if !n.outcome.Succeeded {
		priority = PriorityWarning
	}
	req := &NotifyRequest{
		Producer: "cron", Key: "cron:" + n.occurrence.ID, Priority: priority,
		DestinationAlias: n.outcome.Destination, Text: n.outcome.Text,
	}
	_, err := inv.RunAction(ctx, actionNotify, func(actionCtx context.Context) ([]byte, error) {
		err := r.notify.Notify(actionCtx, req)
		if durable.Preempted(actionCtx) {
			return nil, context.Cause(actionCtx)
		}
		if err != nil {
			r.logger.WarnContext(actionCtx, "schedule notification failed", "component", "schedule", "error", err)
			return []byte(notifyFailed), nil
		}
		return []byte(notifySent), nil
	})
	return err
}

type settlement struct {
	state         string
	turnID        string
	safeErrorCode string
}

func (r *Runner) settle(ctx context.Context, occurrence *Occurrence, terminal *settlement) error {
	now := r.now()
	if err := r.jobs.SettleOccurrence(ctx, occurrence.ID, terminal.state, terminal.turnID, "", terminal.safeErrorCode, now); err != nil {
		return settledElsewhere(err)
	}
	settled := *occurrence
	settled.State = terminal.state
	r.settled(ctx, &settled, now)
	return nil
}

func settledElsewhere(err error) error {
	if code, ok := CodeOf(err); ok && code == ErrorCodeOccurrenceConflict {
		return nil
	}
	return err
}

func (r *Runner) settled(ctx context.Context, occurrence *Occurrence, now time.Time) {
	r.observe(ctx, &Observation{State: occurrence.State, Result: ResultSettled, Age: max(now.Sub(occurrence.CreatedAt), 0)})
	if err := r.pruneHistory(ctx, occurrence.JobID, now); err != nil {
		r.logger.WarnContext(ctx, "occurrence prune failed", "component", "schedule")
	}
}

func (r *Runner) pruneHistory(ctx context.Context, jobID string, now time.Time) error {
	cutoff := now.Add(-r.retention)
	_, err := r.jobs.PruneOccurrences(ctx, jobID, cutoff, pruneBatchLimit)
	return err
}

func (r *Runner) startSuccessor(ctx context.Context, inv durable.Invocation, fire *firePayload) error {
	_, err := inv.RunAction(ctx, actionNext, func(actionCtx context.Context) ([]byte, error) {
		return r.successor(actionCtx, fire)
	})
	return err
}

func (r *Runner) successor(ctx context.Context, fire *firePayload) ([]byte, error) {
	job, live, err := r.liveJob(ctx, fire.JobID)
	if err != nil {
		return nil, err
	}
	if !live {
		return []byte(nextNone), nil
	}
	after := fire.Instant
	if !fire.Chain {
		_, active, err := r.jobs.ActiveOccurrence(ctx, job.ID)
		if err != nil {
			return nil, err
		}
		if active {
			return []byte(nextNone), nil
		}
		after = r.now()
	}
	if err := StartNext(ctx, &NextRequest{Starter: r.starter, Job: &job, After: after}); err != nil {
		return nil, err
	}
	return []byte(nextStarted), nil
}

func (r *Runner) observeFired(ctx context.Context, instant time.Time) {
	r.observe(ctx, &Observation{State: OccurrenceFired, Result: ResultFired, Lag: max(r.now().Sub(instant), 0)})
}

func turnKey(jobID string, fire time.Time) string {
	layout := "20060102T150405"
	if isManual(fire) {
		layout = "20060102T150405.000000000"
	}
	return "cron:" + jobID + ":" + fire.UTC().Format(layout)
}

func isManual(instant time.Time) bool {
	return instant.Nanosecond() != 0
}
