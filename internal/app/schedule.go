package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/anggasct/aura/internal/channels/terminal"
	"github.com/anggasct/aura/internal/config"
	"github.com/anggasct/aura/internal/modules/broadcast"
	"github.com/anggasct/aura/internal/modules/schedule"
	"github.com/anggasct/aura/internal/modules/workflow"
	"github.com/anggasct/aura/internal/platform/durable"
	"github.com/anggasct/aura/internal/platform/principal"
	"github.com/anggasct/aura/internal/platform/telemetry"
	"github.com/anggasct/aura/internal/platform/turn"
	runtimeingress "github.com/anggasct/aura/internal/platform/turn/ingress"
	"github.com/anggasct/aura/internal/store"
)

type scheduleStore struct {
	store store.ScheduleStore
}

func (s *scheduleStore) InsertJob(ctx context.Context, job *schedule.Job) error {
	if s.store == nil || job == nil {
		return errors.New("schedule store must not be nil")
	}
	return mapScheduleStoreError(s.store.InsertJob(ctx, &store.ScheduledJob{
		ID: job.ID, Name: job.Name, CronExpression: job.CronExpression, Timezone: job.Timezone,
		Prompt: job.Prompt, OriginChannel: job.OriginChannel, OriginDestination: job.OriginDestination,
		OverlapPolicy: job.OverlapPolicy, CatchUpGraceSeconds: job.CatchUpGraceSeconds,
		State: job.State, Version: job.Version, CreatedAt: job.CreatedAt, UpdatedAt: job.UpdatedAt,
		WorkflowID: job.WorkflowID, WorkflowInputJSON: job.WorkflowInput,
	}))
}

func toScheduleJob(job *store.ScheduledJob) schedule.Job {
	return schedule.Job{
		ID: job.ID, Name: job.Name, CronExpression: job.CronExpression, Timezone: job.Timezone,
		Prompt: job.Prompt, OriginChannel: job.OriginChannel, OriginDestination: job.OriginDestination,
		OverlapPolicy: job.OverlapPolicy, CatchUpGraceSeconds: job.CatchUpGraceSeconds,
		State: job.State, Version: job.Version, CreatedAt: job.CreatedAt, UpdatedAt: job.UpdatedAt,
		WorkflowID: job.WorkflowID, WorkflowInput: job.WorkflowInputJSON,
	}
}

func (s *scheduleStore) Job(ctx context.Context, id string) (schedule.Job, bool, error) {
	if s.store == nil {
		return schedule.Job{}, false, errors.New("schedule store must not be nil")
	}
	job, err := s.store.Job(ctx, id)
	if err != nil {
		if code, ok := store.CodeOf(err); ok && code == store.ErrorCodeScheduleNotFound {
			return schedule.Job{}, false, nil
		}
		return schedule.Job{}, false, mapScheduleStoreError(err)
	}
	return toScheduleJob(&job), true, nil
}

func (s *scheduleStore) EditJob(ctx context.Context, id string, edit *schedule.JobEdit, expectedVersion int64, now time.Time) (schedule.Job, error) {
	if s.store == nil || edit == nil {
		return schedule.Job{}, errors.New("schedule store must not be nil")
	}
	updated, err := s.store.EditJob(ctx, id, &store.JobEdit{
		Name: edit.Name, CronExpression: edit.CronExpression, Timezone: edit.Timezone,
		Prompt: edit.Prompt, OriginChannel: edit.OriginChannel, OriginDestination: edit.OriginDestination,
		OverlapPolicy: edit.OverlapPolicy, CatchUpGraceSeconds: edit.CatchUpGraceSeconds,
		ExpectedVersion: expectedVersion,
	}, now)
	if err != nil {
		return schedule.Job{}, mapScheduleStoreError(err)
	}
	return toScheduleJob(&updated), nil
}

func (s *scheduleStore) SetJobState(ctx context.Context, id, state string, now time.Time) error {
	if s.store == nil {
		return errors.New("schedule store must not be nil")
	}
	return mapScheduleStoreError(s.store.SetJobState(ctx, id, state, now))
}

func toScheduleOccurrence(item *store.ScheduledOccurrence) schedule.Occurrence {
	return schedule.Occurrence{
		ID: item.ID, JobID: item.JobID, JobVersion: item.JobVersion,
		ScheduledForUTC: item.ScheduledForUTC, State: item.State, TurnID: item.TurnID,
		CreatedAt: item.CreatedAt, UpdatedAt: item.UpdatedAt,
	}
}

func (s *scheduleStore) RecordFire(ctx context.Context, occurrence *schedule.Occurrence) error {
	if s.store == nil || occurrence == nil {
		return errors.New("schedule store must not be nil")
	}
	return mapScheduleStoreError(s.store.RecordFire(ctx, &store.ScheduledOccurrence{
		ID: occurrence.ID, JobID: occurrence.JobID, JobVersion: occurrence.JobVersion,
		ScheduledForUTC: occurrence.ScheduledForUTC, State: occurrence.State,
		CreatedAt: occurrence.CreatedAt, UpdatedAt: occurrence.UpdatedAt,
	}))
}

func (s *scheduleStore) OccurrenceByFire(ctx context.Context, jobID string, scheduledFor time.Time) (schedule.Occurrence, bool, error) {
	if s.store == nil {
		return schedule.Occurrence{}, false, errors.New("schedule store must not be nil")
	}
	item, found, err := s.store.OccurrenceByFire(ctx, jobID, scheduledFor)
	if err != nil {
		return schedule.Occurrence{}, false, mapScheduleStoreError(err)
	}
	if !found {
		return schedule.Occurrence{}, false, nil
	}
	return toScheduleOccurrence(&item), true, nil
}

func (s *scheduleStore) ActiveOccurrence(ctx context.Context, jobID string) (schedule.Occurrence, bool, error) {
	if s.store == nil {
		return schedule.Occurrence{}, false, errors.New("schedule store must not be nil")
	}
	item, found, err := s.store.ActiveOccurrence(ctx, jobID)
	if err != nil {
		return schedule.Occurrence{}, false, mapScheduleStoreError(err)
	}
	if !found {
		return schedule.Occurrence{}, false, nil
	}
	return toScheduleOccurrence(&item), true, nil
}

func (s *scheduleStore) AttachTurn(ctx context.Context, id, turnID string, now time.Time) error {
	if s.store == nil {
		return errors.New("schedule store must not be nil")
	}
	return mapScheduleStoreError(s.store.AttachTurn(ctx, id, turnID, now))
}

func (s *scheduleStore) FiredOccurrences(ctx context.Context, limit int) ([]schedule.Occurrence, error) {
	if s.store == nil {
		return nil, errors.New("schedule store must not be nil")
	}
	items, err := s.store.FiredOccurrences(ctx, limit)
	if err != nil {
		return nil, mapScheduleStoreError(err)
	}
	occurrences := make([]schedule.Occurrence, 0, len(items))
	for i := range items {
		occurrences = append(occurrences, toScheduleOccurrence(&items[i]))
	}
	return occurrences, nil
}

func (s *scheduleStore) ActiveJobs(ctx context.Context, limit int) ([]schedule.Job, error) {
	if s.store == nil {
		return nil, errors.New("schedule store must not be nil")
	}
	items, err := s.store.ActiveJobs(ctx, limit)
	if err != nil {
		return nil, mapScheduleStoreError(err)
	}
	jobs := make([]schedule.Job, 0, len(items))
	for i := range items {
		jobs = append(jobs, toScheduleJob(&items[i]))
	}
	return jobs, nil
}

func (s *scheduleStore) PruneOccurrences(ctx context.Context, jobID string, before time.Time, limit int) (int, error) {
	if s.store == nil {
		return 0, errors.New("schedule store must not be nil")
	}
	count, err := s.store.PruneOccurrences(ctx, jobID, before, limit)
	if err != nil {
		return 0, mapScheduleStoreError(err)
	}
	return count, nil
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
		return schedule.Errorf(schedule.ErrorCodeUnavailable, "schedule store is unavailable")
	}
	switch code {
	case store.ErrorCodeScheduleConflict:
		return schedule.Errorf(schedule.ErrorCodeOccurrenceConflict, "schedule store conflict")
	case store.ErrorCodeScheduleNotFound:
		return schedule.Errorf(schedule.ErrorCodeJobNotFound, "schedule job not found")
	case store.ErrorCodeInvalidArgument:
		return schedule.Errorf(schedule.ErrorCodeInvalidArgument, "schedule store argument invalid")
	default:
		return schedule.Errorf(schedule.ErrorCodeUnavailable, "schedule store is unavailable")
	}
}

type cronTurnEngine interface {
	Run(ctx context.Context, req *turn.TurnRequest) iter.Seq2[store.RuntimeEvent, error]
}

type workflowStarter interface {
	StartConfigured(ctx context.Context, definitionID string, input *workflow.RunInput, principalID string, class principal.Class, durableKey string) (*workflow.RunSummary, error)
}

type scheduleWorkflows struct {
	starter atomic.Pointer[workflowStarter]
}

func (w *scheduleWorkflows) set(starter workflowStarter) {
	if w != nil && starter != nil {
		w.starter.Store(&starter)
	}
}

func (w *scheduleWorkflows) start(ctx context.Context, req *schedule.TurnRequest) (schedule.TurnResult, error) {
	var starter *workflowStarter
	if w != nil {
		starter = w.starter.Load()
	}
	if starter == nil {
		return schedule.TurnResult{}, errors.New("scheduled workflows need the durable runtime")
	}
	input := &workflow.RunInput{}
	if req.WorkflowInput != "" {
		if err := json.Unmarshal([]byte(req.WorkflowInput), input); err != nil {
			return schedule.TurnResult{}, fmt.Errorf("scheduled workflow input: %w", err)
		}
	}
	run, err := (*starter).StartConfigured(ctx, req.WorkflowID, &workflow.RunInput{Objective: input.Objective}, req.PrincipalID, principal.ClassForPrincipal(req.PrincipalID), "sched-"+req.IdempotencyKey)
	if err != nil {
		return schedule.TurnResult{}, err
	}
	return schedule.TurnResult{TurnID: run.ID, Text: fmt.Sprintf("started workflow %s (run %s)", req.WorkflowID, run.ID), Succeeded: true}, nil
}

type engineTurnRunner struct {
	engine    cronTurnEngine
	sessions  store.SessionService
	workflows *scheduleWorkflows
}

func (r *engineTurnRunner) RunTurn(ctx context.Context, req *schedule.TurnRequest) (schedule.TurnResult, error) {
	if req == nil {
		return schedule.TurnResult{}, errors.New("schedule turn request must not be nil")
	}
	if req.WorkflowID != "" {
		return r.workflows.start(ctx, req)
	}
	if r.engine == nil {
		return schedule.TurnResult{}, errors.New("turn engine is not wired")
	}
	if r.sessions != nil {
		if err := ensureCronSession(ctx, r.sessions, req.SessionID); err != nil {
			return schedule.TurnResult{}, err
		}
	}
	var stream []terminal.Event
	var turnID string
	for event, err := range r.engine.Run(ctx, &turn.TurnRequest{
		SessionID:      req.SessionID,
		PrincipalID:    req.PrincipalID,
		PrincipalClass: principal.ClassForPrincipal(req.PrincipalID),
		Origin:         turn.Origin(req.Origin),
		Parts:          []runtimeingress.InputPart{{Text: req.Prompt}},
		IdempotencyKey: req.IdempotencyKey,
	}) {
		if err != nil {
			if code, ok := turn.CodeOf(err); ok && code == turn.ErrorCodeRuntimeOverloaded {
				return schedule.TurnResult{}, schedule.Errorf(schedule.ErrorCodeRuntimeOverloaded, "runtime is overloaded")
			}
			return schedule.TurnResult{}, err
		}
		if turnID == "" {
			turnID = event.TurnID
		}
		stream = append(stream, terminal.Event{Kind: event.Kind, Author: event.Author, TurnID: event.TurnID, Payload: event.Payload})
	}
	text, _, terminalReached := terminal.PlainRenderer{}.RenderTurn(stream)
	if !terminalReached {
		return schedule.TurnResult{}, errors.New("turn stream ended before a terminal event")
	}
	completed := false
	for _, event := range stream {
		if event.Kind == turn.EventKindTurnCompleted {
			completed = true
		}
	}
	result := schedule.TurnResult{TurnID: turnID, Text: text, Succeeded: completed}
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
		ID: sessionID, OwnerID: principal.OwnerID,
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

func (n *cronNotifier) Notify(ctx context.Context, req *schedule.NotifyRequest) error {
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

type scheduleRunnerInput struct {
	db          *sql.DB
	logger      *slog.Logger
	engine      cronTurnEngine
	broadcaster *broadcast.Broadcaster
	retention   time.Duration
	observer    schedule.Observer
	workflows   *scheduleWorkflows
	starter     schedule.Starter
}

func buildScheduleRunner(in *scheduleRunnerInput) (*schedule.Runner, error) {
	if in == nil {
		return nil, errors.New("schedule runner input must not be nil")
	}
	return schedule.NewRunner(&schedule.RunnerDeps{
		Jobs:      &scheduleStore{store: store.NewScheduleStore(in.db)},
		Turns:     &engineTurnRunner{engine: in.engine, sessions: store.NewSessionService(in.db), workflows: in.workflows},
		Notify:    &cronNotifier{broadcaster: in.broadcaster},
		Starter:   in.starter,
		Logger:    in.logger,
		Retention: in.retention,
		Observer:  in.observer,
	})
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

func registerScheduleHandler(target any, runner *schedule.Runner) error {
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
	ifTodo.RegisterHandler(schedule.HandlerName, runner.Handle)
	return nil
}

func scheduleRunsRequest(rt durable.Runtime, db *sql.DB) *schedule.RunsRequest {
	return &schedule.RunsRequest{
		Starter: rt, Store: &scheduleStore{store: store.NewScheduleStore(db)}, Now: time.Now().UTC(),
	}
}

func resumeScheduleRuns(ctx context.Context, rt durable.Runtime, db *sql.DB) (int, error) {
	req := scheduleRunsRequest(rt, db)
	req.Reuse = durable.ReuseEnded
	return schedule.StartRuns(ctx, req)
}

const schedulePassInterval = 5 * time.Minute

type schedulePass struct {
	runtime  durable.Runtime
	db       *sql.DB
	logger   *slog.Logger
	interval time.Duration
}

func (p *schedulePass) run(ctx context.Context) {
	ticker := time.NewTicker(p.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if _, err := schedule.StartRuns(ctx, scheduleRunsRequest(p.runtime, p.db)); err != nil && ctx.Err() == nil {
				p.logger.WarnContext(ctx, "schedule pass failed", "component", "schedule", "error", err)
			}
		}
	}
}

func scheduleRecorderObserver(recorder *telemetry.ScheduleRecorder) schedule.Observer {
	if recorder == nil {
		return nil
	}
	return func(ctx context.Context, observation *schedule.Observation) {
		recorder.Record(ctx, &telemetry.ScheduleObservation{
			State:  observation.State,
			Result: observation.Result,
			Lag:    observation.Lag,
			Age:    observation.Age,
		})
	}
}
