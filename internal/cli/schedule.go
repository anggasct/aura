package cli

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"log/slog"
	"time"

	"github.com/anggasct/aura/internal/broadcast"
	"github.com/anggasct/aura/internal/channel/terminal"
	"github.com/anggasct/aura/internal/config"
	"github.com/anggasct/aura/internal/durable"
	"github.com/anggasct/aura/internal/runtime"
	runtimeingress "github.com/anggasct/aura/internal/runtime/ingress"
	"github.com/anggasct/aura/internal/scheduler"
	"github.com/anggasct/aura/internal/store"
)

type scheduleStore struct {
	store store.ScheduleStore
}

func (s *scheduleStore) InsertJob(ctx context.Context, job *scheduler.Job) error {
	if s.store == nil || job == nil {
		return errors.New("schedule store must not be nil")
	}
	return mapScheduleStoreError(s.store.InsertJob(ctx, &store.ScheduledJob{
		ID: job.ID, Name: job.Name, CronExpression: job.CronExpression, Timezone: job.Timezone,
		Prompt: job.Prompt, OriginChannel: job.OriginChannel, OriginDestination: job.OriginDestination,
		OverlapPolicy: job.OverlapPolicy, CatchUpGraceSeconds: job.CatchUpGraceSeconds,
		State: job.State, Version: job.Version, CreatedAt: job.CreatedAt, UpdatedAt: job.UpdatedAt,
	}))
}

func toScheduleJob(job *store.ScheduledJob) scheduler.Job {
	return scheduler.Job{
		ID: job.ID, Name: job.Name, CronExpression: job.CronExpression, Timezone: job.Timezone,
		Prompt: job.Prompt, OriginChannel: job.OriginChannel, OriginDestination: job.OriginDestination,
		OverlapPolicy: job.OverlapPolicy, CatchUpGraceSeconds: job.CatchUpGraceSeconds,
		State: job.State, Version: job.Version, CreatedAt: job.CreatedAt, UpdatedAt: job.UpdatedAt,
	}
}

func (s *scheduleStore) Job(ctx context.Context, id string) (scheduler.Job, bool, error) {
	if s.store == nil {
		return scheduler.Job{}, false, errors.New("schedule store must not be nil")
	}
	job, err := s.store.Job(ctx, id)
	if err != nil {
		if code, ok := store.CodeOf(err); ok && code == store.ErrorCodeScheduleNotFound {
			return scheduler.Job{}, false, nil
		}
		return scheduler.Job{}, false, mapScheduleStoreError(err)
	}
	return toScheduleJob(&job), true, nil
}

func (s *scheduleStore) EditJob(ctx context.Context, id string, edit *scheduler.JobEdit, expectedVersion int64, now time.Time) (scheduler.Job, error) {
	if s.store == nil || edit == nil {
		return scheduler.Job{}, errors.New("schedule store must not be nil")
	}
	updated, err := s.store.EditJob(ctx, id, &store.JobEdit{
		Name: edit.Name, CronExpression: edit.CronExpression, Timezone: edit.Timezone,
		Prompt: edit.Prompt, OriginChannel: edit.OriginChannel, OriginDestination: edit.OriginDestination,
		OverlapPolicy: edit.OverlapPolicy, CatchUpGraceSeconds: edit.CatchUpGraceSeconds,
		ExpectedVersion: expectedVersion,
	}, now)
	if err != nil {
		return scheduler.Job{}, mapScheduleStoreError(err)
	}
	return toScheduleJob(&updated), nil
}

func (s *scheduleStore) SetJobState(ctx context.Context, id, state string, now time.Time) error {
	if s.store == nil {
		return errors.New("schedule store must not be nil")
	}
	return mapScheduleStoreError(s.store.SetJobState(ctx, id, state, now))
}

func toScheduleOccurrence(item *store.ScheduledOccurrence) scheduler.Occurrence {
	return scheduler.Occurrence{
		ID: item.ID, JobID: item.JobID, JobVersion: item.JobVersion,
		ScheduledForUTC: item.ScheduledForUTC, State: item.State, TurnID: item.TurnID,
		CreatedAt: item.CreatedAt, UpdatedAt: item.UpdatedAt,
	}
}

func (s *scheduleStore) RecordFire(ctx context.Context, occurrence *scheduler.Occurrence) error {
	if s.store == nil || occurrence == nil {
		return errors.New("schedule store must not be nil")
	}
	return mapScheduleStoreError(s.store.RecordFire(ctx, &store.ScheduledOccurrence{
		ID: occurrence.ID, JobID: occurrence.JobID, JobVersion: occurrence.JobVersion,
		ScheduledForUTC: occurrence.ScheduledForUTC, State: occurrence.State,
		CreatedAt: occurrence.CreatedAt, UpdatedAt: occurrence.UpdatedAt,
	}))
}

func (s *scheduleStore) OccurrenceByFire(ctx context.Context, jobID string, scheduledFor time.Time) (scheduler.Occurrence, bool, error) {
	if s.store == nil {
		return scheduler.Occurrence{}, false, errors.New("schedule store must not be nil")
	}
	item, found, err := s.store.OccurrenceByFire(ctx, jobID, scheduledFor)
	if err != nil {
		return scheduler.Occurrence{}, false, mapScheduleStoreError(err)
	}
	if !found {
		return scheduler.Occurrence{}, false, nil
	}
	return toScheduleOccurrence(&item), true, nil
}

func (s *scheduleStore) ActiveOccurrence(ctx context.Context, jobID string) (scheduler.Occurrence, bool, error) {
	if s.store == nil {
		return scheduler.Occurrence{}, false, errors.New("schedule store must not be nil")
	}
	item, found, err := s.store.ActiveOccurrence(ctx, jobID)
	if err != nil {
		return scheduler.Occurrence{}, false, mapScheduleStoreError(err)
	}
	if !found {
		return scheduler.Occurrence{}, false, nil
	}
	return toScheduleOccurrence(&item), true, nil
}

func (s *scheduleStore) AttachTurn(ctx context.Context, id, turnID string, now time.Time) error {
	if s.store == nil {
		return errors.New("schedule store must not be nil")
	}
	return mapScheduleStoreError(s.store.AttachTurn(ctx, id, turnID, now))
}

func (s *scheduleStore) SettleOccurrence(ctx context.Context, id, state, turnID, resultEventID, safeErrorCode string, now time.Time) error {
	if s.store == nil {
		return errors.New("schedule store must not be nil")
	}
	return mapScheduleStoreError(s.store.SettleOccurrence(ctx, id, state, turnID, resultEventID, safeErrorCode, now))
}

func mapScheduleStoreError(err error) error {
	if err == nil {
		return nil
	}
	code, ok := store.CodeOf(err)
	if !ok {
		return scheduler.Errorf(scheduler.ErrorCodeUnavailable, "schedule store is unavailable")
	}
	switch code {
	case store.ErrorCodeScheduleConflict:
		return scheduler.Errorf(scheduler.ErrorCodeOccurrenceConflict, "schedule store conflict")
	case store.ErrorCodeScheduleNotFound:
		return scheduler.Errorf(scheduler.ErrorCodeJobNotFound, "schedule job not found")
	case store.ErrorCodeInvalidArgument:
		return scheduler.Errorf(scheduler.ErrorCodeInvalidArgument, "schedule store argument invalid")
	default:
		return scheduler.Errorf(scheduler.ErrorCodeUnavailable, "schedule store is unavailable")
	}
}

type cronTurnEngine interface {
	Run(ctx context.Context, req *runtime.TurnRequest) iter.Seq2[store.RuntimeEvent, error]
}

type engineTurnRunner struct {
	engine   cronTurnEngine
	sessions store.SessionService
}

func (r *engineTurnRunner) RunTurn(ctx context.Context, req *scheduler.TurnRequest) (scheduler.TurnResult, error) {
	if req == nil {
		return scheduler.TurnResult{}, errors.New("schedule turn request must not be nil")
	}
	if r.engine == nil {
		return scheduler.TurnResult{}, errors.New("turn engine is not wired")
	}
	if r.sessions != nil {
		if err := ensureCronSession(ctx, r.sessions, req.SessionID); err != nil {
			return scheduler.TurnResult{}, err
		}
	}
	var stream []terminal.Event
	var turnID string
	for event, err := range r.engine.Run(ctx, &runtime.TurnRequest{
		SessionID:      req.SessionID,
		PrincipalID:    req.PrincipalID,
		Origin:         runtime.Origin(req.Origin),
		Parts:          []runtimeingress.InputPart{{Text: req.Prompt}},
		IdempotencyKey: req.IdempotencyKey,
	}) {
		if err != nil {
			if code, ok := runtime.CodeOf(err); ok && code == runtime.ErrorCodeRuntimeOverloaded {
				return scheduler.TurnResult{}, scheduler.Errorf(scheduler.ErrorCodeRuntimeOverloaded, "runtime is overloaded")
			}
			return scheduler.TurnResult{}, err
		}
		if turnID == "" {
			turnID = event.TurnID
		}
		stream = append(stream, terminal.Event{Kind: event.Kind, Author: event.Author, TurnID: event.TurnID, Payload: event.Payload})
	}
	text, _, terminalReached := terminal.PlainRenderer{}.RenderTurn(stream)
	if !terminalReached {
		return scheduler.TurnResult{}, errors.New("turn stream ended before a terminal event")
	}
	completed := false
	for _, event := range stream {
		if event.Kind == runtime.EventKindTurnCompleted {
			completed = true
		}
	}
	result := scheduler.TurnResult{TurnID: turnID, Text: text, Succeeded: completed}
	if !completed {
		result.SafeErrorCode = "turn_failed"
	}
	return result, nil
}

func ensureCronSession(ctx context.Context, sessions store.SessionService, sessionID string) error {
	if sessions == nil {
		return nil
	}
	if _, err := sessions.Get(ctx, sessionID); err == nil {
		return nil
	} else if !errors.Is(err, sql.ErrNoRows) {
		if code, ok := store.CodeOf(err); !ok || code != store.ErrorCodeSessionNotFound {
			return err
		}
	}
	now := time.Now().UTC()
	err := sessions.Create(ctx, &store.Session{
		ID: sessionID, OwnerID: "cron",
		Metadata: json.RawMessage(`{}`), CreatedAt: now, UpdatedAt: now,
	})
	if code, ok := store.CodeOf(err); ok && code == store.ErrorCodeSessionIDConflict {
		return nil
	}
	return err
}

type cronNotifier struct {
	broadcaster *broadcast.Broadcaster
}

func (n *cronNotifier) Notify(ctx context.Context, req *scheduler.NotifyRequest) error {
	if req == nil {
		return errors.New("schedule notify request must not be nil")
	}
	if n.broadcaster == nil {
		return errors.New("broadcaster is not wired")
	}
	_, _, err := n.broadcaster.Submit(ctx, &broadcast.Notification{
		Producer:         req.Producer,
		IdempotencyKey:   req.Key,
		Priority:         req.Priority,
		DestinationAlias: req.DestinationAlias,
		ContentJSON:      notifyContentJSON(req.Text),
		CreatedAt:        time.Now().UTC(),
	})
	return err
}

func notifyContentJSON(text string) string {
	raw, err := json.Marshal(map[string]string{"text": text})
	if err != nil {
		return `{"text":""}`
	}
	return string(raw)
}

func buildScheduleRunner(db *sql.DB, logger *slog.Logger, engine cronTurnEngine, broadcaster *broadcast.Broadcaster) (*scheduler.Runner, error) {
	turns := &engineTurnRunner{engine: engine, sessions: store.NewSessionService(db)}
	return scheduler.NewRunner(
		&scheduleStore{store: store.NewScheduleStore(db)},
		turns,
		&cronNotifier{broadcaster: broadcaster},
		logger,
	)
}

func buildCronBroadcaster(cfg *config.Config, db *sql.DB, rt durable.Runtime, channels *broadcastChannels) (*broadcast.Broadcaster, error) {
	location, err := time.LoadLocation(cfg.Broadcast.Timezone)
	if err != nil {
		return nil, fmt.Errorf("broadcast timezone %q is not valid", cfg.Broadcast.Timezone)
	}
	startMin, err := broadcast.ParseHourMinute(cfg.Broadcast.QuietHours.Start)
	if err != nil {
		return nil, fmt.Errorf("broadcast quiet start: %w", err)
	}
	endMin, err := broadcast.ParseHourMinute(cfg.Broadcast.QuietHours.End)
	if err != nil {
		return nil, fmt.Errorf("broadcast quiet end: %w", err)
	}
	registered := map[string]bool{}
	for source := range channels.senders {
		registered[source] = true
	}
	return broadcast.New(
		broadcast.Policy{
			Destinations:   cfg.Broadcast.Destinations,
			Quiet:          broadcast.QuietConfig{Enabled: cfg.Broadcast.QuietHours.Enabled, Location: location, StartMin: startMin, EndMin: endMin},
			MaxDigestItems: cfg.Broadcast.MaxDigestItems,
			MaxDigestBytes: cfg.Broadcast.MaxDigestBytes,
		},
		&cronChannelRegistry{registered: registered},
		&broadcastItemStore{store: store.NewBroadcastStore(db)},
		func(ctx context.Context, itemID string) error {
			return broadcast.StartItemRun(ctx, rt, itemID)
		},
		nil,
	)
}

type cronChannelRegistry struct {
	registered map[string]bool
}

func (r *cronChannelRegistry) Registered(source string) bool {
	return r.registered[source]
}

func registerScheduleHandler(target any, runner *scheduler.Runner) error {
	if runner == nil {
		return errors.New("schedule runner must not be nil")
	}
	type registrar interface {
		RegisterHandler(string, durable.Handler)
	}
	ifTodo, ok := target.(registrar)
	if !ok || ifTodo == nil {
		return errors.New("schedule handler target does not accept handlers")
	}
	ifTodo.RegisterHandler(scheduler.HandlerName, runner.Handle)
	return nil
}

func resumeScheduleRuns(ctx context.Context, rt durable.Runtime, db *sql.DB) (int, error) {
	jobs, err := store.NewScheduleStore(db).ActiveJobs(ctx, 512)
	if err != nil {
		return 0, err
	}
	for i := range jobs {
		if _, err := rt.Start(ctx, durable.StartRequest{
			Handler: scheduler.HandlerName, Key: jobs[i].ID, Payload: []byte(jobs[i].ID),
		}); err != nil {
			return 0, err
		}
	}
	return len(jobs), nil
}
