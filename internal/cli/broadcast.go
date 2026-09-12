package cli

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"time"

	"github.com/anggasct/aura/internal/broadcast"
	"github.com/anggasct/aura/internal/channel/discord"
	"github.com/anggasct/aura/internal/config"
	"github.com/anggasct/aura/internal/durable"
	"github.com/anggasct/aura/internal/effect"
	runtimechannelhost "github.com/anggasct/aura/internal/runtime/channelhost"
	"github.com/anggasct/aura/internal/store"
	"github.com/anggasct/aura/internal/telemetry"
)

type broadcastItemStore struct {
	store store.BroadcastStore
}

func (s *broadcastItemStore) Insert(ctx context.Context, record *broadcast.ItemRecord) (broadcast.ItemRecord, bool, error) {
	if s.store == nil || record == nil {
		return broadcast.ItemRecord{}, false, errors.New("broadcast store must not be nil")
	}
	stored, replayed, err := s.store.InsertItem(ctx, &store.BroadcastItem{
		ID:               record.ID,
		Producer:         record.Producer,
		IdempotencyKey:   record.IdempotencyKey,
		ContentDigest:    record.ContentDigest,
		Priority:         record.Priority,
		DestinationAlias: record.DestinationAlias,
		ContentJSON:      record.ContentJSON,
		State:            record.State,
		NotBefore:        record.NotBefore,
		AttemptCount:     record.AttemptCount,
		EffectID:         record.EffectID,
		DigestParentID:   record.DigestParentID,
		CreatedAt:        record.CreatedAt,
		UpdatedAt:        record.UpdatedAt,
	})
	if err != nil {
		return broadcast.ItemRecord{}, false, mapBroadcastStoreError(err)
	}
	return toBroadcastRecord(&stored), replayed, nil
}

func toBroadcastRecord(item *store.BroadcastItem) broadcast.ItemRecord {
	return broadcast.ItemRecord{
		ID:               item.ID,
		Producer:         item.Producer,
		IdempotencyKey:   item.IdempotencyKey,
		ContentDigest:    item.ContentDigest,
		Priority:         item.Priority,
		DestinationAlias: item.DestinationAlias,
		ContentJSON:      item.ContentJSON,
		State:            item.State,
		NotBefore:        item.NotBefore,
		AttemptCount:     item.AttemptCount,
		EffectID:         item.EffectID,
		DigestParentID:   item.DigestParentID,
		CreatedAt:        item.CreatedAt,
		UpdatedAt:        item.UpdatedAt,
	}
}

func toBroadcastFull(item *store.BroadcastItem) broadcast.FullItem {
	record := toBroadcastRecord(item)
	return broadcast.FullItem{
		Item: broadcast.Item{
			ID:               record.ID,
			Producer:         record.Producer,
			IdempotencyKey:   record.IdempotencyKey,
			ContentDigest:    record.ContentDigest,
			Priority:         record.Priority,
			DestinationAlias: record.DestinationAlias,
			ContentJSON:      record.ContentJSON,
			State:            record.State,
			NotBefore:        record.NotBefore,
			CreatedAt:        record.CreatedAt,
		},
		AttemptCount:   record.AttemptCount,
		EffectID:       record.EffectID,
		DigestParentID: record.DigestParentID,
	}
}

func (s *broadcastItemStore) CreateDigest(ctx context.Context, parent *broadcast.ItemRecord, childIDs []string) (broadcast.ItemRecord, bool, error) {
	if s.store == nil || parent == nil {
		return broadcast.ItemRecord{}, false, errors.New("broadcast store must not be nil")
	}
	mapped := &store.BroadcastItem{
		ID:               parent.ID,
		Producer:         parent.Producer,
		IdempotencyKey:   parent.IdempotencyKey,
		ContentDigest:    parent.ContentDigest,
		Priority:         parent.Priority,
		DestinationAlias: parent.DestinationAlias,
		ContentJSON:      parent.ContentJSON,
		State:            parent.State,
		NotBefore:        parent.NotBefore,
		CreatedAt:        parent.CreatedAt,
		UpdatedAt:        parent.UpdatedAt,
	}
	stored, replayed, err := s.store.CreateDigest(ctx, mapped, childIDs)
	if err != nil {
		return broadcast.ItemRecord{}, false, mapBroadcastStoreError(err)
	}
	return toBroadcastRecord(&stored), replayed, nil
}

func (s *broadcastItemStore) Load(ctx context.Context, id string) (broadcast.FullItem, bool, error) {
	if s.store == nil {
		return broadcast.FullItem{}, false, errors.New("broadcast store must not be nil")
	}
	item, err := s.store.Item(ctx, id)
	if err != nil {
		if code, ok := store.CodeOf(err); ok && code == store.ErrorCodeBroadcastNotFound {
			return broadcast.FullItem{}, false, nil
		}
		return broadcast.FullItem{}, false, mapBroadcastStoreError(err)
	}
	return toBroadcastFull(&item), true, nil
}

func (s *broadcastItemStore) ListHeld(ctx context.Context, alias, priority string, notBefore time.Time, limit int) ([]broadcast.FullItem, error) {
	if s.store == nil {
		return nil, errors.New("broadcast store must not be nil")
	}
	items, err := s.store.ListHeld(ctx, alias, priority, notBefore, limit)
	if err != nil {
		return nil, mapBroadcastStoreError(err)
	}
	return mapBroadcastFull(items), nil
}

func mapBroadcastFull(items []store.BroadcastItem) []broadcast.FullItem {
	out := make([]broadcast.FullItem, 0, len(items))
	for i := range items {
		out = append(out, toBroadcastFull(&items[i]))
	}
	return out
}

func (s *broadcastItemStore) Settle(ctx context.Context, id, state, effectID string, now time.Time) error {
	if s.store == nil {
		return errors.New("broadcast store must not be nil")
	}
	return mapBroadcastStoreError(s.store.Settle(ctx, id, state, effectID, now))
}

func (s *broadcastItemStore) NoteAttempt(ctx context.Context, id string, attempt int64, now time.Time) error {
	if s.store == nil {
		return errors.New("broadcast store must not be nil")
	}
	return mapBroadcastStoreError(s.store.NoteAttempt(ctx, id, attempt, now))
}

func (s *broadcastItemStore) ClaimDispatchSlot(ctx context.Context, alias string, now time.Time, gap time.Duration) (time.Time, error) {
	if s.store == nil {
		return time.Time{}, errors.New("broadcast store must not be nil")
	}
	slot, err := s.store.ClaimDispatchSlot(ctx, alias, now, gap)
	if err != nil {
		return time.Time{}, mapBroadcastStoreError(err)
	}
	return slot, nil
}

func mapBroadcastStoreError(err error) error {
	if err == nil {
		return nil
	}
	code, ok := store.CodeOf(err)
	if !ok {
		return broadcast.Errorf(broadcast.ErrorCodeUnavailable, "broadcast store is unavailable")
	}
	switch code {
	case store.ErrorCodeBroadcastConflict:
		return broadcast.Errorf(broadcast.ErrorCodeConflict, "broadcast store conflict")
	case store.ErrorCodeInvalidArgument:
		return broadcast.Errorf(broadcast.ErrorCodeInvalidArgument, "broadcast store argument invalid")
	default:
		return broadcast.Errorf(broadcast.ErrorCodeUnavailable, "broadcast store is unavailable")
	}
}

type discordBroadcastSender struct {
	adapter  *discord.Adapter
	effects  *effect.Executor
	sessions store.SessionService
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
	case effect.StateUnknown:
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

type broadcastChannels struct {
	senders   map[string]broadcast.Sender
	providers map[string]effect.Provider
}

func ensureBroadcastSession(ctx context.Context, sessions store.SessionService, sessionID string) error {
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
		ID:        sessionID,
		OwnerID:   "broadcast",
		Metadata:  json.RawMessage(`{}`),
		CreatedAt: now,
		UpdatedAt: now,
	})
	if code, ok := store.CodeOf(err); ok && code == store.ErrorCodeSessionIDConflict {
		return nil
	}
	return err
}

func buildBroadcastSenders(adapters []runtimechannelhost.ChannelPort, effects *effect.Executor, sessions store.SessionService) *broadcastChannels {
	out := &broadcastChannels{senders: map[string]broadcast.Sender{}, providers: map[string]effect.Provider{}}
	for _, adapter := range adapters {
		concrete, ok := adapter.(*discord.Adapter)
		if !ok {
			continue
		}
		out.providers["discord"] = concrete
		out.senders["discord"] = &discordBroadcastSender{adapter: concrete, effects: effects, sessions: sessions}
	}
	return out
}

func buildBroadcastRunner(cfg *config.Config, db *sql.DB, logger *slog.Logger, channels *broadcastChannels, observer broadcast.Observer) (*broadcast.Runner, error) {
	if cfg == nil {
		return nil, errors.New("broadcast config must not be nil")
	}
	return broadcast.NewRunner(
		&broadcastItemStore{store: store.NewBroadcastStore(db)},
		cfg.Broadcast.Destinations,
		channels.senders,
		broadcast.RunPolicy{
			MaxAttempts:    cfg.Broadcast.MaxAttempts,
			MaxDeliveryAge: time.Duration(cfg.Broadcast.MaxDeliveryAge),
			DispatchGap:    time.Duration(cfg.Broadcast.MinDispatchGap),
			Fallback:       cfg.Broadcast.Fallback,
			MaxDigestItems: cfg.Broadcast.MaxDigestItems,
			MaxDigestBytes: cfg.Broadcast.MaxDigestBytes,
		},
		logger,
		observer,
	)
}

func registerBroadcastHandler(target any, runner *broadcast.Runner) error {
	if runner == nil {
		return errors.New("broadcast runner must not be nil")
	}
	type registrar interface {
		RegisterHandler(string, durable.Handler)
	}
	ifTodo, ok := target.(registrar)
	if !ok || ifTodo == nil {
		return errors.New("broadcast handler target does not accept handlers")
	}
	ifTodo.RegisterHandler(broadcast.HandlerName, runner.Handle)
	return nil
}

func resumeBroadcastRunners(ctx context.Context, runtime durable.Runtime, db *sql.DB) (int, error) {
	return broadcast.ResumeActive(ctx, runtime, &broadcastResumeStore{store: store.NewBroadcastStore(db)})
}

type broadcastResumeStore struct {
	store store.BroadcastStore
}

func (s *broadcastResumeStore) ListActive(ctx context.Context, limit int) ([]broadcast.FullItem, error) {
	if s.store == nil {
		return nil, errors.New("broadcast store must not be nil")
	}
	items, err := s.store.ListActive(ctx, limit)
	if err != nil {
		return nil, err
	}
	return mapBroadcastFull(items), nil
}

func broadcastEffectProviders(cfg *config.Config, db *sql.DB, logger *slog.Logger) (map[string]effect.Provider, error) {
	providers := map[string]effect.Provider{}
	if cfg == nil || !cfg.Channels.Discord.Enabled {
		return providers, nil
	}
	_, artifactRoot, _, err := storagePaths(cfg)
	if err != nil {
		return nil, err
	}
	adapter, err := newDiscordAdapter(cfg, db, artifactRoot, logger)
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
