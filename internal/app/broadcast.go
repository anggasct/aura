package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"time"

	"github.com/spf13/cobra"

	appconfig "github.com/anggasct/aura/internal/app/config"
	"github.com/anggasct/aura/internal/channels/discord"
	"github.com/anggasct/aura/internal/modules/broadcast"
	toolsbuiltin "github.com/anggasct/aura/internal/modules/builtin"
	"github.com/anggasct/aura/internal/platform/effect"
	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/telemetry"
	"github.com/anggasct/aura/internal/platform/turn"
	runtimechannelhost "github.com/anggasct/aura/internal/platform/turn/channelhost"
	"github.com/anggasct/aura/internal/platform/turn/store"
)

type discordBroadcastSender struct {
	adapter  *discord.Adapter
	effects  *effect.Executor
	sessions turnstore.SessionService
}

func (s *discordBroadcastSender) Send(ctx context.Context, req *broadcast.SendRequest) (broadcast.SendOutcome, error) {
	if req == nil {
		return broadcast.SendOutcome{}, errors.New("broadcast send request must not be nil")
	}
	if !isBroadcastChannelID(req.Route.Instance) {
		return broadcast.SendOutcome{}, nil
	}
	if s.adapter == nil || s.effects == nil {
		return broadcast.SendOutcome{}, errors.New("discord broadcast sender is not wired")
	}
	if s.sessions != nil {
		if err := ensureBroadcastSession(ctx, s.sessions, req.SessionID); err != nil {
			return broadcast.SendOutcome{}, err
		}
	}
	payload, err := json.Marshal(discord.SendOperation{ChannelID: req.Route.Instance, Text: req.Text})
	if err != nil {
		return broadcast.SendOutcome{}, err
	}
	intent, err := s.effects.Execute(ctx, &effect.PrepareRequest{
		SessionID:      req.SessionID,
		IdempotencyKey: req.IdempotencyKey,
		Provider:       "discord",
		Operation:      discord.OpSendMessage,
		Classification: effect.ClassificationEffectful,
		Request:        payload,
		EventKind:      effect.EventKindChannelRequested,
	}, s.adapter)
	if err != nil {
		return broadcast.SendOutcome{}, err
	}
	switch intent.State {
	case effect.StateSucceeded:
		return broadcast.SendOutcome{IntentID: intent.ID, Receipt: intent.ProviderReceipt, Succeeded: true}, nil
	case effect.StateUnknown, effect.StateStarted:
		return broadcast.SendOutcome{IntentID: intent.ID, Ambiguous: true}, nil
	default:
		return broadcast.SendOutcome{IntentID: intent.ID}, nil
	}
}

func isBroadcastChannelID(id string) bool {
	if id == "" {
		return false
	}
	for i := range len(id) {
		if id[i] < '0' || id[i] > '9' {
			return false
		}
	}
	return true
}

func ensureBroadcastSession(ctx context.Context, sessions turnstore.SessionService, sessionID string) error {
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
		ID:        sessionID,
		OwnerID:   "broadcast",
		Metadata:  json.RawMessage(`{}`),
		CreatedAt: now,
		UpdatedAt: now,
	})
	if code, ok := fault.CodeOf[turnstore.ErrorCode](err); ok && code == turnstore.ErrorCodeSessionIDConflict {
		return nil
	}
	return err
}

func buildBroadcastSenders(adapters []runtimechannelhost.ChannelPort, effects *effect.Executor, sessions turnstore.SessionService) map[string]broadcast.Sender {
	senders := map[string]broadcast.Sender{}
	for _, adapter := range adapters {
		concrete, ok := adapter.(*discord.Adapter)
		if !ok {
			continue
		}
		senders["discord"] = &discordBroadcastSender{adapter: concrete, effects: effects, sessions: sessions}
		break
	}
	return senders
}

type broadcastEffectReconciler struct {
	executor *effect.Executor
}

func (r *broadcastEffectReconciler) GetIntent(ctx context.Context, id string) (*effect.Intent, error) {
	return r.executor.Journal().Get(ctx, id)
}

func (r *broadcastEffectReconciler) Reconcile(ctx context.Context, id string, provider effect.Provider) (*effect.Intent, error) {
	return r.executor.Reconcile(ctx, id, provider, nil)
}

func broadcastOpener(gf *globalFlags) broadcast.Opener {
	return func(cmd *cobra.Command, needDurable bool, fn func(ctx context.Context, env *broadcast.CommandEnv) error) error {
		result, err := appconfig.Load(gf.configPath)
		if err != nil {
			return err
		}
		var parts []Part
		if needDurable {
			parts = []Part{PartDurable}
		}
		return withApp(cmd, &commandInput{result: &result, parts: parts}, func(ctx context.Context, app *App) error {
			items, err := broadcast.NewSQLiteStore(app.db)
			if err != nil {
				return err
			}
			env := &broadcast.CommandEnv{
				Store:   items,
				Runtime: app.runtime,
				Effects: func() (broadcast.EffectReconciler, error) {
					executor, err := toolsbuiltin.NewChannelEffects(app.db, app.logger)
					if err != nil {
						return nil, err
					}
					return &broadcastEffectReconciler{executor: executor}, nil
				},
				EffectProviders: func() (map[string]effect.Provider, error) {
					return broadcastEffectProviders(app.result.Config, app.db, app.logger)
				},
			}
			return fn(ctx, env)
		})
	}
}

func broadcastEffectProviders(cfg *appconfig.Config, db *sql.DB, logger *slog.Logger) (map[string]effect.Provider, error) {
	providers := map[string]effect.Provider{}
	if cfg == nil || !cfg.Channels.Discord.Enabled || len(cfg.Channels.Discord.Accounts) == 0 {
		return providers, nil
	}
	_, artifactRoot, _, err := storagePaths(cfg)
	if err != nil {
		return nil, err
	}
	adapter, err := newDiscordAdapter(cfg, &cfg.Channels.Discord.Accounts[0], db, artifactRoot, logger)
	if err != nil {
		return nil, err
	}
	providers["discord"] = adapter
	return providers, nil
}

func broadcastRecorderObserver(recorder *telemetry.BroadcastRecorder) broadcast.Observer {
	if recorder == nil {
		return nil
	}
	return func(ctx context.Context, observation *broadcast.Observation) {
		recorder.Record(ctx, &telemetry.BroadcastObservation{
			Priority:    observation.Priority,
			State:       observation.State,
			Result:      observation.Result,
			Attempts:    observation.Attempts,
			Age:         observation.Age,
			RateDelay:   observation.RateDelay,
			DigestCount: observation.DigestCount,
		})
	}
}
