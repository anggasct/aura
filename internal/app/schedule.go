package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"sync/atomic"
	"time"

	"github.com/spf13/cobra"

	appconfig "github.com/anggasct/aura/internal/app/config"
	"github.com/anggasct/aura/internal/channels/terminal"
	"github.com/anggasct/aura/internal/modules/broadcast"
	"github.com/anggasct/aura/internal/modules/schedule"
	"github.com/anggasct/aura/internal/modules/workflow"
	"github.com/anggasct/aura/internal/platform/durable"
	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/principal"
	"github.com/anggasct/aura/internal/platform/telemetry"
	"github.com/anggasct/aura/internal/platform/turn"
	runtimeingress "github.com/anggasct/aura/internal/platform/turn/ingress"
	"github.com/anggasct/aura/internal/platform/turn/store"
	"github.com/anggasct/aura/internal/store"
)

type cronTurnEngine interface {
	Run(ctx context.Context, req *turn.TurnRequest) iter.Seq2[turn.Event, error]
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
		return schedule.TurnResult{}, errors.New("scheduled workflows have no workflow interpreter")
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
	sessions  turnstore.SessionService
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
			if fault.HasCode(err, turn.ErrorCodeRuntimeOverloaded) {
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

func ensureCronSession(ctx context.Context, sessions turnstore.SessionService, sessionID string) error {
	if sessions == nil {
		return nil
	}
	if _, err := sessions.Get(ctx, sessionID); err == nil {
		return nil
	} else if !errors.Is(err, sql.ErrNoRows) {
		if code, ok := fault.CodeOf[turnstore.ErrorCode](err); !ok || code != turnstore.ErrorCodeSessionNotFound {
			return err
		}
	}
	now := time.Now().UTC()
	err := sessions.Create(ctx, &turn.Session{
		ID: sessionID, OwnerID: principal.OwnerID,
		Metadata: json.RawMessage(`{}`), CreatedAt: now, UpdatedAt: now,
	})
	if code, ok := fault.CodeOf[turnstore.ErrorCode](err); ok && code == turnstore.ErrorCodeSessionIDConflict {
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

func buildCronBroadcaster(cfg *appconfig.Config, db *sql.DB, rt durable.Runtime, channels *broadcastChannels) (*broadcast.Broadcaster, error) {
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

func scheduleOpener(gf *globalFlags) schedule.Opener {
	return func(cmd *cobra.Command, needDurable bool, fn func(ctx context.Context, env *schedule.CommandEnv) error) error {
		result, err := appconfig.Load(gf.configPath)
		if err != nil {
			return err
		}
		var parts []Part
		if needDurable {
			parts = []Part{PartDurable}
		}
		return withApp(cmd, &commandInput{result: &result, parts: parts}, func(ctx context.Context, app *App) error {
			jobs, err := schedule.NewSQLiteStore(app.db)
			if err != nil {
				return err
			}
			service, err := schedule.NewService(jobs)
			if err != nil {
				return err
			}
			env := &schedule.CommandEnv{
				Config:  app.result.Config.Scheduler,
				Store:   jobs,
				Service: service,
				Starter: app.runtime,
				WorkflowChecker: func(checkCtx context.Context, id string) error {
					return checkScheduledWorkflow(checkCtx, app.result.Config, app.db, id)
				},
			}
			return fn(ctx, env)
		})
	}
}

func checkScheduledWorkflow(ctx context.Context, cfg *appconfig.Config, db *sql.DB, id string) error {
	specs, err := workflow.LoadDefinitionsDir(workflowDefinitionsDir(cfg))
	if err != nil {
		return err
	}
	for _, spec := range specs {
		if spec.ID == id {
			return nil
		}
	}
	runs, err := workflow.NewStore(db, turnstore.NewTurnStore(db))
	if err != nil {
		return err
	}
	if _, err := runs.LatestDefinition(ctx, id); err != nil {
		return &usageError{fmt.Errorf("workflow %q is neither a definition nor a saved routine", id)}
	}
	return nil
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
