package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

const (
	BroadcastPriorityUrgent  = "urgent"
	BroadcastPriorityWarning = "warning"
	BroadcastPriorityInfo    = "info"
)

const (
	BroadcastStateHeld      = "held"
	BroadcastStateScheduled = "scheduled"
	BroadcastStateStarted   = "started"
	BroadcastStateSucceeded = "succeeded"
	BroadcastStateFailed    = "failed"
	BroadcastStateUnknown   = "unknown"
	BroadcastStateCancelled = "cancelled"
)

type BroadcastItem struct {
	ID               string
	Producer         string
	IdempotencyKey   string
	ContentDigest    string
	Priority         string
	DestinationAlias string
	ContentJSON      string
	State            string
	NotBefore        time.Time
	AttemptCount     int64
	EffectID         string
	DigestParentID   string
	CreatedAt        time.Time
	UpdatedAt        time.Time
}

type BroadcastStore interface {
	InsertItem(ctx context.Context, item *BroadcastItem) (BroadcastItem, bool, error)
	Item(ctx context.Context, id string) (BroadcastItem, error)
	ItemByKey(ctx context.Context, producer, key string) (BroadcastItem, bool, error)
}

type sqliteBroadcastStore struct {
	db *sql.DB
}

func NewBroadcastStore(db *sql.DB) BroadcastStore {
	return &sqliteBroadcastStore{db: db}
}

func validBroadcastPriority(priority string) bool {
	switch priority {
	case BroadcastPriorityUrgent, BroadcastPriorityWarning, BroadcastPriorityInfo:
		return true
	default:
		return false
	}
}

func validBroadcastState(state string) bool {
	switch state {
	case BroadcastStateHeld, BroadcastStateScheduled, BroadcastStateStarted,
		BroadcastStateSucceeded, BroadcastStateFailed, BroadcastStateUnknown,
		BroadcastStateCancelled:
		return true
	default:
		return false
	}
}

const selectBroadcastItemColumns = `
	id, producer, idempotency_key, content_digest, priority, destination_alias,
	content_json, state, not_before, attempt_count, effect_id, digest_parent_id,
	created_at, updated_at
`

func (s *sqliteBroadcastStore) InsertItem(ctx context.Context, item *BroadcastItem) (BroadcastItem, bool, error) {
	if s.db == nil {
		return BroadcastItem{}, false, errNilArgument("db")
	}
	if item == nil {
		return BroadcastItem{}, false, errNilArgument("item")
	}
	if item.ID == "" || item.Producer == "" || item.IdempotencyKey == "" {
		return BroadcastItem{}, false, Errorf(ErrorCodeInvalidArgument, "broadcast identity must not be empty")
	}
	if !validBroadcastPriority(item.Priority) {
		return BroadcastItem{}, false, Errorf(ErrorCodeBroadcastInvalid, "broadcast priority is not valid")
	}
	if item.DestinationAlias == "" {
		return BroadcastItem{}, false, Errorf(ErrorCodeInvalidArgument, "broadcast destination must not be empty")
	}
	if !json.Valid([]byte(item.ContentJSON)) {
		return BroadcastItem{}, false, Errorf(ErrorCodeBroadcastInvalid, "broadcast content is not valid JSON")
	}
	if !validBroadcastState(item.State) {
		return BroadcastItem{}, false, Errorf(ErrorCodeBroadcastInvalid, "broadcast state is not valid")
	}
	if item.AttemptCount < 0 {
		return BroadcastItem{}, false, Errorf(ErrorCodeBroadcastInvalid, "broadcast attempt count must not be negative")
	}

	_, err := s.db.ExecContext(ctx,
		`INSERT INTO broadcast_item (
			id, producer, idempotency_key, content_digest, priority, destination_alias,
			content_json, state, not_before, attempt_count, effect_id, digest_parent_id,
			created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		item.ID,
		item.Producer,
		item.IdempotencyKey,
		item.ContentDigest,
		item.Priority,
		item.DestinationAlias,
		item.ContentJSON,
		item.State,
		formatTime(item.NotBefore.UTC()),
		item.AttemptCount,
		nullText(item.EffectID),
		nullText(item.DigestParentID),
		formatTime(item.CreatedAt.UTC()),
		formatTime(item.UpdatedAt.UTC()),
	)
	if err == nil {
		return *item, false, nil
	}
	if !isConstraintUnique(err) {
		return BroadcastItem{}, false, classifyBusy(fmt.Errorf("insert broadcast item: %w", err))
	}
	existing, found, findErr := s.ItemByKey(ctx, item.Producer, item.IdempotencyKey)
	if findErr != nil {
		return BroadcastItem{}, false, findErr
	}
	if !found {
		return BroadcastItem{}, false, classifyBusy(fmt.Errorf("insert broadcast item: %w", err))
	}
	if existing.ContentDigest != item.ContentDigest {
		return BroadcastItem{}, false, &Error{
			Code:   ErrorCodeBroadcastConflict,
			Detail: fmt.Sprintf("idempotency key %q is already claimed with different content", item.IdempotencyKey),
		}
	}
	return existing, true, nil
}

func (s *sqliteBroadcastStore) Item(ctx context.Context, id string) (BroadcastItem, error) {
	if s.db == nil {
		return BroadcastItem{}, errNilArgument("db")
	}
	item, found, err := s.findItem(ctx,
		`SELECT `+selectBroadcastItemColumns+` FROM broadcast_item WHERE id = ?`, id)
	if err != nil {
		return BroadcastItem{}, err
	}
	if !found {
		return BroadcastItem{}, &Error{Code: ErrorCodeBroadcastNotFound, Detail: "broadcast item not found"}
	}
	return item, nil
}

func (s *sqliteBroadcastStore) ItemByKey(ctx context.Context, producer, key string) (BroadcastItem, bool, error) {
	if s.db == nil {
		return BroadcastItem{}, false, errNilArgument("db")
	}
	return s.findItem(ctx,
		`SELECT `+selectBroadcastItemColumns+` FROM broadcast_item WHERE producer = ? AND idempotency_key = ?`,
		producer, key)
}

func (s *sqliteBroadcastStore) findItem(ctx context.Context, query string, args ...any) (BroadcastItem, bool, error) {
	var item BroadcastItem
	var notBeforeRaw, createdAtRaw, updatedAtRaw string
	var effectID, digestParentID sql.NullString
	err := s.db.QueryRowContext(ctx, query, args...).Scan(
		&item.ID,
		&item.Producer,
		&item.IdempotencyKey,
		&item.ContentDigest,
		&item.Priority,
		&item.DestinationAlias,
		&item.ContentJSON,
		&item.State,
		&notBeforeRaw,
		&item.AttemptCount,
		&effectID,
		&digestParentID,
		&createdAtRaw,
		&updatedAtRaw,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return BroadcastItem{}, false, nil
	}
	if err != nil {
		return BroadcastItem{}, false, classifyBusy(fmt.Errorf("load broadcast item: %w", err))
	}
	if !validBroadcastPriority(item.Priority) || !validBroadcastState(item.State) {
		return BroadcastItem{}, false, Errorf(ErrorCodeBroadcastInvalid, "broadcast row carries an invalid priority or state")
	}
	notBefore, err := parseTime(notBeforeRaw)
	if err != nil {
		return BroadcastItem{}, false, Errorf(ErrorCodeBroadcastInvalid, "broadcast not_before is not a valid timestamp")
	}
	createdAt, err := parseTime(createdAtRaw)
	if err != nil {
		return BroadcastItem{}, false, Errorf(ErrorCodeBroadcastInvalid, "broadcast created_at is not a valid timestamp")
	}
	updatedAt, err := parseTime(updatedAtRaw)
	if err != nil {
		return BroadcastItem{}, false, Errorf(ErrorCodeBroadcastInvalid, "broadcast updated_at is not a valid timestamp")
	}
	item.NotBefore = notBefore
	item.CreatedAt = createdAt
	item.UpdatedAt = updatedAt
	item.EffectID = effectID.String
	item.DigestParentID = digestParentID.String
	return item, true, nil
}
