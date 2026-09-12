package scheduler

import (
	"context"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/anggasct/aura/internal/durable"
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
	PendingFire(ctx context.Context, jobID string) (Occurrence, bool, error)
	PruneOccurrences(ctx context.Context, jobID string, before time.Time, limit int) (int, error)
}

const defaultRetention = 720 * time.Hour

const pruneBatchLimit = 100

const wakeInterval = 5 * time.Minute

type Runner struct {
	jobs      RunStore
	turns     TurnRunner
	notify    Notifier
	overlap   time.Duration
	retention time.Duration
	logger    *slog.Logger
	principal string
	now       func() time.Time
	observer  Observer
}

func NewRunner(jobs RunStore, turns TurnRunner, notify Notifier, logger *slog.Logger, retention time.Duration, observer Observer) (*Runner, error) {
	if jobs == nil {
		return nil, Errorf(ErrorCodeInvalidArgument, "run store must not be nil")
	}
	if turns == nil {
		return nil, Errorf(ErrorCodeInvalidArgument, "turn runner must not be nil")
	}
	if notify == nil {
		return nil, Errorf(ErrorCodeInvalidArgument, "notifier must not be nil")
	}
	if retention <= 0 {
		retention = defaultRetention
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Runner{jobs: jobs, turns: turns, notify: notify, overlap: overlapPollInterval, retention: retention, logger: logger, principal: "cron", observer: observer, now: func() time.Time {
		return time.Now().UTC()
	}}, nil
}

func (r *Runner) clock(ctx context.Context, inv durable.Invocation, key string) (time.Time, error) {
	raw, err := inv.RunAction(ctx, "clock:"+key, func(context.Context) ([]byte, error) {
		return []byte(r.now().Format(time.RFC3339Nano)), nil
	})
	if err != nil {
		return time.Time{}, err
	}
	moment, err := time.Parse(time.RFC3339Nano, string(raw))
	if err != nil {
		return time.Time{}, Errorf(ErrorCodeInvalidArgument, "journaled clock is not a valid timestamp")
	}
	return moment, nil
}

func (r *Runner) Handle(ctx context.Context, inv durable.Invocation) error {
	if ctx == nil {
		return Errorf(ErrorCodeInvalidArgument, "context must not be nil")
	}
	if inv == nil {
		return Errorf(ErrorCodeInvalidArgument, "invocation must not be nil")
	}
	id := strings.TrimSpace(string(inv.Payload()))
	if id == "" {
		return Errorf(ErrorCodeInvalidArgument, "schedule job id must not be empty")
	}
	job, found, err := r.jobs.Job(ctx, id)
	if err != nil {
		return err
	}
	if !found {
		r.logger.WarnContext(ctx, "schedule job missing", "component", "schedule")
		return nil
	}
	schedule, err := ParseExpression(job.CronExpression)
	if err != nil {
		return err
	}
	location, err := time.LoadLocation(job.Timezone)
	if err != nil {
		return Errorf(ErrorCodeTimezoneInvalid, "timezone is not a valid IANA name")
	}
	anchor, err := r.clock(ctx, inv, "start")
	if err != nil {
		return err
	}
	var iter uint64
	for {
		iter++
		job, found, err := r.jobs.Job(ctx, id)
		if err != nil {
			return err
		}
		if !found || job.State == JobDeleted || job.State == JobPaused {
			return nil
		}
		if pending, found, err := r.jobs.PendingFire(ctx, id); err != nil {
			return err
		} else if found {
			now, err := r.clock(ctx, inv, "pending:"+strconv.FormatUint(iter, 10))
			if err != nil {
				return err
			}
			if _, err := r.fireDue(ctx, inv, &job, pending.ScheduledForUTC, now, iter); err != nil {
				return err
			}
			continue
		}
		fire, err := NextFire(&schedule, location, anchor)
		if err != nil {
			return err
		}
		now, err := r.clock(ctx, inv, "fire:"+strconv.FormatUint(iter, 10))
		if err != nil {
			return err
		}
		if fire.After(now) {
			remaining := fire.Sub(now)
			if remaining > wakeInterval {
				remaining = wakeInterval
			}
			if err := inv.Sleep(remaining); err != nil {
				return err
			}
			continue
		}
		done, err := r.fireDue(ctx, inv, &job, fire, now, iter)
		if err != nil {
			return err
		}
		if done {
			anchor = fire
		}
	}
}

func (r *Runner) fireDue(ctx context.Context, inv durable.Invocation, job *Job, fire, now time.Time, iter uint64) (bool, error) {
	tag := strconv.FormatUint(iter, 10)
	grace := time.Duration(job.CatchUpGraceSeconds) * time.Second
	lag := now.Sub(fire)
	if lag < 0 {
		lag = 0
	}
	r.observe(ctx, &Observation{State: OccurrenceFired, Result: ResultFired, Lag: lag})
	if now.Sub(fire) > grace {
		occurrence, replayed, err := r.recordFire(ctx, job, fire, now)
		if err != nil {
			return false, err
		}
		if replayed {
			current, found, err := r.jobs.OccurrenceByFire(ctx, job.ID, fire.UTC())
			if err != nil {
				return false, err
			}
			if found && current.State != OccurrenceFired {
				return true, nil
			}
		}
		if err := r.settle(ctx, job.ID, &occurrence, OccurrenceExpired, "", "", now); err != nil {
			return false, err
		}
		return true, nil
	}
	if job.OverlapPolicy == OverlapQueueOne {
		proceed, err := r.awaitActive(ctx, inv, job)
		if err != nil {
			return false, err
		}
		if !proceed {
			return false, nil
		}
	} else {
		_, found, err := r.jobs.ActiveOccurrence(ctx, job.ID)
		if err != nil {
			return false, err
		}
		if found {
			occurrence, replayed, err := r.recordFire(ctx, job, fire, now)
			if err != nil {
				return false, err
			}
			if replayed {
				current, found, err := r.jobs.OccurrenceByFire(ctx, job.ID, fire.UTC())
				if err != nil {
					return false, err
				}
				if found && current.State != OccurrenceFired {
					return true, nil
				}
			}
			if err := r.settle(ctx, job.ID, &occurrence, OccurrenceSkippedOverlap, "", "", now); err != nil {
				return false, err
			}
			return true, nil
		}
	}
	occurrence, replayed, err := r.recordFire(ctx, job, fire, now)
	if err != nil {
		return false, err
	}
	if replayed {
		current, found, err := r.jobs.OccurrenceByFire(ctx, job.ID, fire.UTC())
		if err != nil {
			return false, err
		}
		if found && current.State != OccurrenceFired {
			return true, nil
		}
	}
	for retry := 0; ; retry++ {
		result, err := r.turns.RunTurn(ctx, &TurnRequest{
			SessionID:      "cron:" + job.ID,
			PrincipalID:    r.principal,
			Origin:         "internal",
			Prompt:         job.Prompt,
			IdempotencyKey: turnKey(job.ID, fire),
		})
		if err != nil {
			if code, ok := CodeOf(err); ok && code == ErrorCodeRuntimeOverloaded {
				r.observe(ctx, &Observation{State: occurrence.State, Result: ResultRetried})
				expired, err := r.awaitCapacity(ctx, inv, job, fire, tag+":"+strconv.Itoa(retry))
				if err != nil {
					return false, err
				}
				if expired {
					settled, err := r.clock(ctx, inv, "expired:"+tag)
					if err != nil {
						return false, err
					}
					if err := r.settle(ctx, job.ID, &occurrence, OccurrenceExpired, "", "", settled); err != nil {
						return false, err
					}
					return true, nil
				}
				continue
			}
			return false, err
		}
		if err := r.jobs.AttachTurn(ctx, occurrence.ID, result.TurnID, now); err != nil {
			return false, err
		}
		terminal := OccurrenceCompleted
		code := ""
		if !result.Succeeded {
			terminal = OccurrenceFailed
			code = result.SafeErrorCode
		}
		settled, err := r.clock(ctx, inv, "settle:"+tag)
		if err != nil {
			return false, err
		}
		if err := r.settle(ctx, job.ID, &occurrence, terminal, result.TurnID, code, settled); err != nil {
			return false, err
		}
		priority := PriorityInfo
		if terminal == OccurrenceFailed {
			priority = PriorityWarning
		}
		if err := r.notify.Notify(ctx, &NotifyRequest{
			Producer:         "cron",
			Key:              "cron:" + occurrence.ID,
			Priority:         priority,
			DestinationAlias: job.OriginDestination,
			Text:             result.Text,
		}); err != nil {
			return false, err
		}
		return true, nil
	}
}

func (r *Runner) awaitCapacity(ctx context.Context, inv durable.Invocation, job *Job, fire time.Time, tag string) (bool, error) {
	grace := time.Duration(job.CatchUpGraceSeconds) * time.Second
	if err := ctx.Err(); err != nil {
		return false, err
	}
	now, err := r.clock(ctx, inv, "capacity:"+tag)
	if err != nil {
		return false, err
	}
	if now.Sub(fire) > grace {
		return true, nil
	}
	remaining := grace - now.Sub(fire)
	if remaining > overlapPollInterval {
		remaining = overlapPollInterval
	}
	if err := inv.Sleep(remaining); err != nil {
		return false, err
	}
	return false, nil
}

func (r *Runner) awaitActive(ctx context.Context, inv durable.Invocation, job *Job) (bool, error) {
	for {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		current, found, err := r.jobs.Job(ctx, job.ID)
		if err != nil {
			return false, err
		}
		if !found || current.State == JobDeleted || current.State == JobPaused {
			return false, nil
		}
		_, found, err = r.jobs.ActiveOccurrence(ctx, job.ID)
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

func (r *Runner) settle(ctx context.Context, jobID string, occurrence *Occurrence, state, turnID, safeErrorCode string, now time.Time) error {
	if err := r.jobs.SettleOccurrence(ctx, occurrence.ID, state, turnID, "", safeErrorCode, now); err != nil {
		return err
	}
	age := now.Sub(occurrence.CreatedAt)
	if age < 0 {
		age = 0
	}
	r.observe(ctx, &Observation{State: state, Result: ResultSettled, Age: age})
	if err := r.pruneHistory(ctx, jobID, now); err != nil {
		r.logger.WarnContext(ctx, "occurrence prune failed", "component", "schedule")
	}
	return nil
}

func (r *Runner) pruneHistory(ctx context.Context, jobID string, now time.Time) error {
	cutoff := now.Add(-r.retention)
	_, err := r.jobs.PruneOccurrences(ctx, jobID, cutoff, pruneBatchLimit)
	return err
}

func (r *Runner) recordFire(ctx context.Context, job *Job, fire, now time.Time) (Occurrence, bool, error) {
	id := occurrenceID(job.ID, fire)
	err := r.jobs.RecordFire(ctx, &Occurrence{
		ID: id, JobID: job.ID, JobVersion: job.Version, ScheduledForUTC: fire.UTC(),
		State: OccurrenceFired, CreatedAt: now, UpdatedAt: now,
	})
	if err != nil {
		existing, found, findErr := r.jobs.OccurrenceByFire(ctx, job.ID, fire.UTC())
		if findErr != nil {
			return Occurrence{}, false, findErr
		}
		if !found {
			return Occurrence{}, false, err
		}
		return existing, true, nil
	}
	return Occurrence{
		ID: id, JobID: job.ID, JobVersion: job.Version, ScheduledForUTC: fire.UTC(),
		State: OccurrenceFired, CreatedAt: now, UpdatedAt: now,
	}, false, nil
}

func turnKey(jobID string, fire time.Time) string {
	return "cron:" + jobID + ":" + fire.UTC().Format("20060102T150405")
}
