package broadcast

import (
	"context"
	"encoding/json"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/anggasct/aura/internal/durable"
)

const (
	retryBaseDelay = 5 * time.Second
	retryMaxDelay  = 5 * time.Minute
	maxReleaseScan = 1024
)

const HandlerName = "broadcast"

type SendRequest struct {
	Route          Route  `json:"route"`
	Text           string `json:"text"`
	SessionID      string `json:"session_id"`
	IdempotencyKey string `json:"idempotency_key"`
}

type SendOutcome struct {
	IntentID  string `json:"intent_id"`
	Receipt   []byte `json:"receipt"`
	Succeeded bool   `json:"succeeded"`
	Ambiguous bool   `json:"ambiguous"`
}

type Sender interface {
	Send(ctx context.Context, req *SendRequest) (SendOutcome, error)
}

type FullItem struct {
	Item
	AttemptCount   int64
	EffectID       string
	DigestParentID string
}

type RunStore interface {
	ItemStore
	Load(ctx context.Context, id string) (FullItem, bool, error)
	ListHeld(ctx context.Context, alias, priority string, notBefore time.Time, limit int) ([]FullItem, error)
	Settle(ctx context.Context, id, state, effectID string, now time.Time) error
	NoteAttempt(ctx context.Context, id string, attempt int64, now time.Time) error
	ClaimDispatchSlot(ctx context.Context, alias string, now time.Time, gap time.Duration) (time.Time, error)
}

type RunPolicy struct {
	MaxAttempts    int
	MaxDeliveryAge time.Duration
	DispatchGap    time.Duration
	Fallback       map[string]string
	MaxDigestItems int
	MaxDigestBytes int64
}

type Runner struct {
	items          RunStore
	routes         map[string]string
	senders        map[string]Sender
	fallback       map[string]string
	maxAttempts    int
	maxAge         time.Duration
	gap            time.Duration
	maxDigestItems int
	maxDigestBytes int64
	logger         *slog.Logger
}

func NewRunner(items RunStore, routes map[string]string, senders map[string]Sender, policy RunPolicy, logger *slog.Logger) (*Runner, error) {
	if items == nil {
		return nil, Errorf(ErrorCodeInvalidArgument, "run store must not be nil")
	}
	if policy.MaxAttempts <= 0 {
		return nil, Errorf(ErrorCodeInvalidArgument, "max attempts must be positive")
	}
	if policy.MaxDeliveryAge <= 0 {
		return nil, Errorf(ErrorCodeInvalidArgument, "max delivery age must be positive")
	}
	if policy.DispatchGap < 0 {
		return nil, Errorf(ErrorCodeInvalidArgument, "dispatch gap must not be negative")
	}
	if policy.MaxDigestItems <= 0 || policy.MaxDigestBytes <= 0 {
		return nil, Errorf(ErrorCodeInvalidArgument, "digest bounds must be positive")
	}
	resolved := make(map[string]string, len(routes))
	for alias, route := range routes {
		if !ValidAlias(alias) {
			return nil, Errorf(ErrorCodeInvalidArgument, "destination alias is not valid")
		}
		if _, _, err := ParseRoute(route); err != nil {
			return nil, err
		}
		resolved[alias] = route
	}
	fallback := make(map[string]string, len(policy.Fallback))
	for alias, route := range policy.Fallback {
		if !ValidAlias(alias) {
			return nil, Errorf(ErrorCodeInvalidArgument, "fallback alias is not valid")
		}
		if route == "" {
			continue
		}
		if _, _, err := ParseRoute(route); err != nil {
			return nil, err
		}
		fallback[alias] = route
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Runner{
		items:          items,
		routes:         resolved,
		senders:        senders,
		fallback:       fallback,
		maxAttempts:    policy.MaxAttempts,
		maxAge:         policy.MaxDeliveryAge,
		gap:            policy.DispatchGap,
		maxDigestItems: policy.MaxDigestItems,
		maxDigestBytes: policy.MaxDigestBytes,
		logger:         logger,
	}, nil
}

func terminalState(state string) bool {
	switch state {
	case StateSucceeded, StateFailed, StateUnknown, StateCancelled:
		return true
	default:
		return false
	}
}

func retryBackoff(attempt int) time.Duration {
	delay := retryBaseDelay
	for i := 1; i < attempt; i++ {
		delay *= 2
		if delay >= retryMaxDelay || delay <= 0 {
			return retryMaxDelay
		}
	}
	if delay > retryMaxDelay || delay <= 0 {
		return retryMaxDelay
	}
	return delay
}

func RenderText(content string) string {
	var value any
	if err := json.Unmarshal([]byte(content), &value); err != nil {
		return content
	}
	switch node := value.(type) {
	case string:
		return node
	case map[string]any:
		if text, ok := node["text"].(string); ok {
			return text
		}
		parts, ok := node["items"].([]any)
		if !ok {
			return content
		}
		rendered := make([]string, 0, len(parts))
		for _, part := range parts {
			partMap, ok := part.(map[string]any)
			if !ok {
				continue
			}
			raw, err := json.Marshal(partMap["content"])
			if err != nil {
				continue
			}
			rendered = append(rendered, RenderText(string(raw)))
		}
		return strings.Join(rendered, "\n---\n")
	default:
		return content
	}
}

func (r *Runner) clock(ctx context.Context, inv durable.Invocation, key string) (time.Time, error) {
	raw, err := inv.RunAction(ctx, "clock:"+key, func(context.Context) ([]byte, error) {
		return []byte(time.Now().UTC().Format(time.RFC3339Nano)), nil
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

func (r *Runner) sleepUntil(inv durable.Invocation, now, instant time.Time) error {
	if !instant.After(now) {
		return nil
	}
	return inv.Sleep(instant.Sub(now))
}

func (r *Runner) senderFor(route Route) (Sender, error) {
	sender, ok := r.senders[route.Source]
	if !ok || sender == nil {
		return nil, Errorf(ErrorCodeChannelUnknown, "destination channel is not wired")
	}
	return sender, nil
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
		return Errorf(ErrorCodeInvalidArgument, "broadcast item id must not be empty")
	}
	item, found, err := r.items.Load(ctx, id)
	if err != nil {
		return err
	}
	if !found {
		r.logger.WarnContext(ctx, "broadcast item missing", "component", "broadcast")
		return nil
	}
	if terminalState(item.State) {
		return nil
	}
	now, err := r.clock(ctx, inv, "start")
	if err != nil {
		return err
	}
	if now.Sub(item.CreatedAt) > r.maxAge {
		return r.items.Settle(ctx, id, StateFailed, item.EffectID, now)
	}
	if item.State == StateHeld {
		if err := r.releaseWindow(ctx, inv, &item); err != nil {
			return err
		}
		current, found, err := r.items.Load(ctx, id)
		if err != nil {
			return err
		}
		if !found || terminalState(current.State) {
			return nil
		}
		item = current
	}
	return r.deliverOne(ctx, inv, &item.Item, item.ID)
}

func toItems(records []FullItem) []Item {
	items := make([]Item, 0, len(records))
	for i := range records {
		items = append(items, records[i].Item)
	}
	return items
}

func (r *Runner) releaseWindow(ctx context.Context, inv durable.Invocation, self *FullItem) error {
	members, err := r.items.ListHeld(ctx, self.DestinationAlias, self.Priority, self.NotBefore, maxReleaseScan)
	if err != nil {
		return err
	}
	groups := GroupDigests(toItems(members), r.maxDigestItems, r.maxDigestBytes)
	for index := range groups {
		group := groups[index]
		if len(group) == 0 {
			continue
		}
		if len(group) == 1 {
			single, found, err := r.items.Load(ctx, group[0].ID)
			if err != nil {
				return err
			}
			if !found || terminalState(single.State) {
				continue
			}
			if err := r.deliverOne(ctx, inv, &single.Item, single.ID); err != nil {
				return err
			}
			continue
		}
		parent, _, err := r.createDigest(ctx, group, index)
		if err != nil {
			if code, ok := CodeOf(err); ok && code == ErrorCodeConflict {
				continue
			}
			return err
		}
		if err := r.deliverOne(ctx, inv, &parent, parent.ID); err != nil {
			return err
		}
	}
	return nil
}

func (r *Runner) createDigest(ctx context.Context, group []Item, index int) (Item, bool, error) {
	parent, err := buildDigestParent(group, index, r.maxDigestBytes)
	if err != nil {
		return Item{}, false, err
	}
	record, replayed, err := r.items.CreateDigest(ctx, parent, childIDsOf(group))
	if err != nil {
		return Item{}, false, err
	}
	return recordToItem(&record), replayed, nil
}

func (r *Runner) deliverOne(ctx context.Context, inv durable.Invocation, item *Item, keyBase string) error {
	current, found, err := r.items.Load(ctx, item.ID)
	if err != nil {
		return err
	}
	if !found || terminalState(current.State) {
		return nil
	}
	routeValue, ok := r.routes[item.DestinationAlias]
	if !ok {
		now, err := r.clock(ctx, inv, "withdrawn:"+keyBase)
		if err != nil {
			return err
		}
		return r.items.Settle(ctx, item.ID, StateFailed, "", now)
	}
	source, instance, err := ParseRoute(routeValue)
	if err != nil {
		return err
	}
	sender, err := r.senderFor(Route{Source: source, Instance: instance})
	if err != nil {
		now, clockErr := r.clock(ctx, inv, "unwired:"+keyBase)
		if clockErr != nil {
			return clockErr
		}
		return r.items.Settle(ctx, item.ID, StateFailed, "", now)
	}
	text := RenderText(item.ContentJSON)
	sessionID := "broadcast:" + item.DestinationAlias
	for attempt := 1; attempt <= r.maxAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		now, err := r.clock(ctx, inv, "attempt")
		if err != nil {
			return err
		}
		if now.Sub(item.CreatedAt) > r.maxAge {
			return r.items.Settle(ctx, item.ID, StateFailed, "", now)
		}
		slot, err := r.items.ClaimDispatchSlot(ctx, item.DestinationAlias, now, r.gap)
		if err != nil {
			return err
		}
		if err := r.sleepUntil(inv, now, slot); err != nil {
			return err
		}
		if err := r.items.NoteAttempt(ctx, item.ID, int64(attempt), now); err != nil {
			return err
		}
		outcome, sendErr := r.journaledSend(ctx, inv, sender, &SendRequest{
			Route:          Route{Source: source, Instance: instance},
			Text:           text,
			SessionID:      sessionID,
			IdempotencyKey: "bcst:" + keyBase + ":a" + strconv.Itoa(attempt),
		}, keyBase, attempt)
		if sendErr != nil {
			if attempt < r.maxAttempts {
				if err := inv.Sleep(retryBackoff(attempt)); err != nil {
					return err
				}
				continue
			}
			return r.fallbackOrFail(ctx, inv, item, keyBase, text, sessionID, "")
		}
		if outcome.Ambiguous {
			return r.items.Settle(ctx, item.ID, StateUnknown, outcome.IntentID, now)
		}
		if outcome.Succeeded {
			return r.items.Settle(ctx, item.ID, StateSucceeded, outcome.IntentID, now)
		}
		return r.fallbackOrFail(ctx, inv, item, keyBase, text, sessionID, outcome.IntentID)
	}
	return nil
}

func (r *Runner) fallbackOrFail(ctx context.Context, inv durable.Invocation, item *Item, keyBase, text, sessionID, intentID string) error {
	routeValue, ok := r.fallback[item.DestinationAlias]
	if !ok || routeValue == "" {
		now, err := r.clock(ctx, inv, "failed:"+keyBase)
		if err != nil {
			return err
		}
		return r.items.Settle(ctx, item.ID, StateFailed, intentID, now)
	}
	source, instance, err := ParseRoute(routeValue)
	if err != nil {
		return err
	}
	fallbackSender, err := r.senderFor(Route{Source: source, Instance: instance})
	if err != nil {
		now, clockErr := r.clock(ctx, inv, "failed:"+keyBase)
		if clockErr != nil {
			return clockErr
		}
		return r.items.Settle(ctx, item.ID, StateFailed, intentID, now)
	}
	outcome, sendErr := r.journaledSend(ctx, inv, fallbackSender, &SendRequest{Route: Route{Source: source, Instance: instance},
		Text:           text,
		SessionID:      sessionID,
		IdempotencyKey: "bcst:" + keyBase + ":fb",
	}, keyBase, 0)
	now, err := r.clock(ctx, inv, "fallback:"+keyBase)
	if err != nil {
		return err
	}
	if sendErr != nil {
		return r.items.Settle(ctx, item.ID, StateUnknown, intentID, now)
	}
	if outcome.Ambiguous {
		return r.items.Settle(ctx, item.ID, StateUnknown, outcome.IntentID, now)
	}
	if outcome.Succeeded {
		return r.items.Settle(ctx, item.ID, StateSucceeded, outcome.IntentID, now)
	}
	return r.items.Settle(ctx, item.ID, StateFailed, outcome.IntentID, now)
}

type journaledOutcome struct {
	Outcome SendOutcome `json:"outcome"`
}

func (r *Runner) journaledSend(ctx context.Context, inv durable.Invocation, sender Sender, req *SendRequest, keyBase string, attempt int) (SendOutcome, error) {
	key := "send:" + keyBase + ":a" + strconv.Itoa(attempt)
	if attempt == 0 {
		key = "send:" + keyBase + ":fb"
	}
	raw, err := inv.RunAction(ctx, key, func(ctx context.Context) ([]byte, error) {
		outcome, err := sender.Send(ctx, req)
		if err != nil {
			return nil, err
		}
		return json.Marshal(journaledOutcome{Outcome: outcome})
	})
	if err != nil {
		return SendOutcome{}, err
	}
	var decoded journaledOutcome
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return SendOutcome{}, Errorf(ErrorCodeInvalidArgument, "journaled send outcome is not decodable")
	}
	return decoded.Outcome, nil
}
