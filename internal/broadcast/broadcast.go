package broadcast

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

const (
	PriorityUrgent  = "urgent"
	PriorityWarning = "warning"
	PriorityInfo    = "info"
)

const (
	StateHeld      = "held"
	StateScheduled = "scheduled"
	StateStarted   = "started"
	StateSucceeded = "succeeded"
	StateFailed    = "failed"
	StateUnknown   = "unknown"
	StateCancelled = "cancelled"
)

const maxContentBytes = 1 << 16

const maxAliasRunes = 64

const maxProducerRunes = 128

const maxKeyRunes = 256

type ErrorCode string

const (
	ErrorCodeInvalidArgument ErrorCode = "invalid_argument"
	ErrorCodeUnknownAlias    ErrorCode = "unknown_alias"
	ErrorCodeChannelUnknown  ErrorCode = "channel_unknown"
	ErrorCodeConflict        ErrorCode = "conflict"
	ErrorCodeUnavailable     ErrorCode = "unavailable"
)

type Error struct {
	Code   ErrorCode
	Detail string
}

func (e *Error) Error() string {
	return fmt.Sprintf("%s: %s", e.Code, e.Detail)
}

func CodeOf(err error) (ErrorCode, bool) {
	var target *Error
	if !errors.As(err, &target) {
		return "", false
	}
	return target.Code, true
}

func Errorf(code ErrorCode, format string, args ...any) error {
	return &Error{Code: code, Detail: fmt.Sprintf(format, args...)}
}

type Notification struct {
	Producer         string
	IdempotencyKey   string
	Priority         string
	DestinationAlias string
	ContentJSON      string
	CreatedAt        time.Time
}

type Route struct {
	Source   string
	Instance string
}

type Item struct {
	ID               string
	Producer         string
	IdempotencyKey   string
	ContentDigest    string
	Priority         string
	DestinationAlias string
	ContentJSON      string
	State            string
	NotBefore        time.Time
	CreatedAt        time.Time
}

type ItemRecord struct {
	ID               string
	Producer         string
	IdempotencyKey   string
	ContentDigest    string
	Priority         string
	DestinationAlias string
	ContentJSON      string
	State            string
	NotBefore        time.Time
	CreatedAt        time.Time
	UpdatedAt        time.Time
}

type ItemStore interface {
	Insert(ctx context.Context, record *ItemRecord) (ItemRecord, bool, error)
}

type ChannelRegistry interface {
	Registered(source string) bool
}

type Broadcaster struct {
	destinations map[string]string
	channels     ChannelRegistry
	items        ItemStore
}

func New(destinations map[string]string, channels ChannelRegistry, items ItemStore) (*Broadcaster, error) {
	if channels == nil {
		return nil, Errorf(ErrorCodeInvalidArgument, "channel registry must not be nil")
	}
	if items == nil {
		return nil, Errorf(ErrorCodeInvalidArgument, "item store must not be nil")
	}
	routes := make(map[string]string, len(destinations))
	for alias, route := range destinations {
		if !ValidAlias(alias) {
			return nil, Errorf(ErrorCodeInvalidArgument, "destination alias is not valid")
		}
		if _, _, err := ParseRoute(route); err != nil {
			return nil, err
		}
		routes[alias] = route
	}
	return &Broadcaster{destinations: routes, channels: channels, items: items}, nil
}

func ValidAlias(alias string) bool {
	if alias == "" || len(alias) > maxAliasRunes {
		return false
	}
	for i := range len(alias) {
		c := alias[i]
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' && c != '_' {
			return false
		}
	}
	return true
}

func ParseRoute(route string) (source, instance string, err error) {
	if route == "" || strings.Contains(route, "://") || strings.ContainsAny(route, " \t\n/") {
		return "", "", Errorf(ErrorCodeInvalidArgument, "route is not a channel destination")
	}
	parts := strings.Split(route, ":")
	if len(parts) < 1 || len(parts) > 2 {
		return "", "", Errorf(ErrorCodeInvalidArgument, "route is not a channel destination")
	}
	source = parts[0]
	if !ValidAlias(source) {
		return "", "", Errorf(ErrorCodeInvalidArgument, "route source is not valid")
	}
	if len(parts) == 2 {
		instance = parts[1]
		if instance == "" || len(instance) > maxAliasRunes {
			return "", "", Errorf(ErrorCodeInvalidArgument, "route instance is not valid")
		}
	}
	return source, instance, nil
}

func validPriority(priority string) bool {
	switch priority {
	case PriorityUrgent, PriorityWarning, PriorityInfo:
		return true
	default:
		return false
	}
}

func newItemID() (string, error) {
	var raw [8]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return "bcst_" + hex.EncodeToString(raw[:]), nil
}

func digestContent(content string) string {
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:])
}

func (b *Broadcaster) Resolve(alias string) (Route, error) {
	route, ok := b.destinations[alias]
	if !ok {
		return Route{}, Errorf(ErrorCodeUnknownAlias, "destination alias is not configured")
	}
	source, instance, err := ParseRoute(route)
	if err != nil {
		return Route{}, err
	}
	if !b.channels.Registered(source) {
		return Route{}, Errorf(ErrorCodeChannelUnknown, "destination channel is not registered")
	}
	return Route{Source: source, Instance: instance}, nil
}

func (b *Broadcaster) Submit(ctx context.Context, notification *Notification) (Item, bool, error) {
	if ctx == nil {
		return Item{}, false, Errorf(ErrorCodeInvalidArgument, "context must not be nil")
	}
	if notification == nil {
		return Item{}, false, Errorf(ErrorCodeInvalidArgument, "notification must not be nil")
	}
	if err := ctx.Err(); err != nil {
		return Item{}, false, err
	}
	if strings.TrimSpace(notification.Producer) == "" || len([]rune(notification.Producer)) > maxProducerRunes {
		return Item{}, false, Errorf(ErrorCodeInvalidArgument, "producer is not valid")
	}
	if strings.TrimSpace(notification.IdempotencyKey) == "" || len([]rune(notification.IdempotencyKey)) > maxKeyRunes {
		return Item{}, false, Errorf(ErrorCodeInvalidArgument, "idempotency key is not valid")
	}
	if !validPriority(notification.Priority) {
		return Item{}, false, Errorf(ErrorCodeInvalidArgument, "priority is not valid")
	}
	if !ValidAlias(notification.DestinationAlias) {
		return Item{}, false, Errorf(ErrorCodeUnknownAlias, "destination alias is not valid")
	}
	if notification.ContentJSON == "" || len(notification.ContentJSON) > maxContentBytes {
		return Item{}, false, Errorf(ErrorCodeInvalidArgument, "content is not within bounds")
	}
	if !json.Valid([]byte(notification.ContentJSON)) {
		return Item{}, false, Errorf(ErrorCodeInvalidArgument, "content is not valid JSON")
	}
	if _, err := b.Resolve(notification.DestinationAlias); err != nil {
		return Item{}, false, err
	}
	id, err := newItemID()
	if err != nil {
		return Item{}, false, Errorf(ErrorCodeUnavailable, "item identity is unavailable")
	}
	createdAt := notification.CreatedAt.UTC()
	if createdAt.IsZero() {
		createdAt = time.Now().UTC()
	}
	record, replayed, err := b.items.Insert(ctx, &ItemRecord{
		ID:               id,
		Producer:         notification.Producer,
		IdempotencyKey:   notification.IdempotencyKey,
		ContentDigest:    digestContent(notification.ContentJSON),
		Priority:         notification.Priority,
		DestinationAlias: notification.DestinationAlias,
		ContentJSON:      notification.ContentJSON,
		State:            StateScheduled,
		NotBefore:        createdAt,
		CreatedAt:        createdAt,
		UpdatedAt:        createdAt,
	})
	if err != nil {
		return Item{}, false, err
	}
	return Item{
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
	}, replayed, nil
}
