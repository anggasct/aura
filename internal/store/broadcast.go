package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
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
	CreateDigest(ctx context.Context, parent *BroadcastItem, childIDs []string) (BroadcastItem, bool, error)
	ClaimDispatchSlot(ctx context.Context, alias string, now time.Time, gap time.Duration) (time.Time, error)
	DeferDestination(ctx context.Context, alias string, until time.Time) error
	DestinationCursor(ctx context.Context, alias string) (time.Time, bool, error)
	ListHeld(ctx context.Context, alias, priority string, notBefore time.Time, limit int) ([]BroadcastItem, error)
	ListActive(ctx context.Context, limit int) ([]BroadcastItem, error)
	ListItems(ctx context.Context, states []string, limit int) ([]BroadcastItem, error)
	Settle(ctx context.Context, id, state, effectID string, now time.Time) error
	NoteAttempt(ctx context.Context, id string, attempt int64, now time.Time) error
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

func (s *sqliteBroadcastStore) CreateDigest(ctx context.Context, parent *BroadcastItem, childIDs []string) (BroadcastItem, bool, error) {
	if s.db == nil {
		return BroadcastItem{}, false, errNilArgument("db")
	}
	if parent == nil {
		return BroadcastItem{}, false, errNilArgument("parent")
	}
	if len(childIDs) == 0 {
		return BroadcastItem{}, false, Errorf(ErrorCodeInvalidArgument, "digest children must not be empty")
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{})
	if err != nil {
		return BroadcastItem{}, false, classifyBusy(fmt.Errorf("begin digest transaction: %w", err))
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()
	_, err = tx.ExecContext(ctx,
		`INSERT INTO broadcast_item (
			id, producer, idempotency_key, content_digest, priority, destination_alias,
			content_json, state, not_before, attempt_count, effect_id, digest_parent_id,
			created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		parent.ID,
		parent.Producer,
		parent.IdempotencyKey,
		parent.ContentDigest,
		parent.Priority,
		parent.DestinationAlias,
		parent.ContentJSON,
		parent.State,
		formatTime(parent.NotBefore.UTC()),
		parent.AttemptCount,
		nullText(parent.EffectID),
		nullText(parent.DigestParentID),
		formatTime(parent.CreatedAt.UTC()),
		formatTime(parent.UpdatedAt.UTC()),
	)
	if err != nil {
		if !isConstraintUnique(err) {
			return BroadcastItem{}, false, classifyBusy(fmt.Errorf("insert digest parent: %w", err))
		}
		existing, found, findErr := s.ItemByKey(ctx, parent.Producer, parent.IdempotencyKey)
		if findErr != nil {
			return BroadcastItem{}, false, findErr
		}
		if !found {
			return BroadcastItem{}, false, classifyBusy(fmt.Errorf("insert digest parent: %w", err))
		}
		return existing, true, nil
	}
	link, err := tx.PrepareContext(ctx,
		`UPDATE broadcast_item SET state = ?, digest_parent_id = ?, updated_at = ? WHERE id = ? AND state = ?`)
	if err != nil {
		return BroadcastItem{}, false, classifyBusy(fmt.Errorf("prepare digest link: %w", err))
	}
	defer func() { _ = link.Close() }()
	var linked int64
	for _, id := range childIDs {
		result, err := link.ExecContext(ctx,
			BroadcastStateCancelled, parent.ID, formatTime(parent.UpdatedAt.UTC()), id, BroadcastStateHeld)
		if err != nil {
			return BroadcastItem{}, false, classifyBusy(fmt.Errorf("link digest children: %w", err))
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return BroadcastItem{}, false, classifyBusy(fmt.Errorf("link digest children: %w", err))
		}
		linked += affected
	}
	if linked != int64(len(childIDs)) {
		return BroadcastItem{}, false, Errorf(ErrorCodeBroadcastConflict, "digest children are not all held")
	}
	if err := tx.Commit(); err != nil {
		return BroadcastItem{}, false, classifyBusy(fmt.Errorf("commit digest transaction: %w", err))
	}
	committed = true
	return *parent, false, nil
}

func (s *sqliteBroadcastStore) findItem(ctx context.Context, query string, args ...any) (BroadcastItem, bool, error) {
	item, found, err := scanOneBroadcastItem(s.db.QueryRowContext(ctx, query, args...))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return BroadcastItem{}, false, nil
		}
		return BroadcastItem{}, false, classifyBusy(fmt.Errorf("load broadcast item: %w", err))
	}
	return item, found, nil
}

type broadcastRowScanner interface {
	Scan(dest ...any) error
}

func scanOneBroadcastItem(row broadcastRowScanner) (BroadcastItem, bool, error) {
	var item BroadcastItem
	var notBeforeRaw, createdAtRaw, updatedAtRaw string
	var effectID, digestParentID sql.NullString
	err := row.Scan(
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
		return BroadcastItem{}, false, err
	}
	return finishBroadcastItemScan(&item, notBeforeRaw, createdAtRaw, updatedAtRaw, effectID, digestParentID)
}

func scanBroadcastItem(rows *sql.Rows) (BroadcastItem, error) {
	var item BroadcastItem
	var notBeforeRaw, createdAtRaw, updatedAtRaw string
	var effectID, digestParentID sql.NullString
	if err := rows.Scan(
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
	); err != nil {
		return BroadcastItem{}, classifyBusy(fmt.Errorf("scan broadcast item: %w", err))
	}
	finished, _, err := finishBroadcastItemScan(&item, notBeforeRaw, createdAtRaw, updatedAtRaw, effectID, digestParentID)
	return finished, err
}

func finishBroadcastItemScan(item *BroadcastItem, notBeforeRaw, createdAtRaw, updatedAtRaw string, effectID, digestParentID sql.NullString) (BroadcastItem, bool, error) {
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
	return *item, true, nil
}

func (s *sqliteBroadcastStore) DestinationCursor(ctx context.Context, alias string) (time.Time, bool, error) {
	if s.db == nil {
		return time.Time{}, false, errNilArgument("db")
	}
	if alias == "" {
		return time.Time{}, false, Errorf(ErrorCodeInvalidArgument, "destination alias must not be empty")
	}
	var raw string
	err := s.db.QueryRowContext(ctx,
		`SELECT next_eligible_at FROM broadcast_destination WHERE alias = ?`, alias).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, false, nil
	}
	if err != nil {
		return time.Time{}, false, classifyBusy(fmt.Errorf("load destination cursor: %w", err))
	}
	cursor, err := parseTime(raw)
	if err != nil {
		return time.Time{}, false, Errorf(ErrorCodeBroadcastInvalid, "destination cursor is not a valid timestamp")
	}
	return cursor, true, nil
}

func (s *sqliteBroadcastStore) ClaimDispatchSlot(ctx context.Context, alias string, now time.Time, gap time.Duration) (time.Time, error) {
	if s.db == nil {
		return time.Time{}, errNilArgument("db")
	}
	if alias == "" {
		return time.Time{}, Errorf(ErrorCodeInvalidArgument, "destination alias must not be empty")
	}
	if gap < 0 {
		return time.Time{}, Errorf(ErrorCodeInvalidArgument, "dispatch gap must not be negative")
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{})
	if err != nil {
		return time.Time{}, classifyBusy(fmt.Errorf("begin dispatch slot transaction: %w", err))
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()
	var raw string
	err = tx.QueryRowContext(ctx,
		`SELECT next_eligible_at FROM broadcast_destination WHERE alias = ?`, alias).Scan(&raw)
	slot := now.UTC()
	switch {
	case errors.Is(err, sql.ErrNoRows):
	case err != nil:
		return time.Time{}, classifyBusy(fmt.Errorf("load destination cursor: %w", err))
	default:
		cursor, parseErr := parseTime(raw)
		if parseErr != nil {
			return time.Time{}, Errorf(ErrorCodeBroadcastInvalid, "destination cursor is not a valid timestamp")
		}
		slot = cursor.UTC()
		if slot.Before(now.UTC()) {
			slot = now.UTC()
		}
	}
	next := slot.Add(gap)
	_, err = tx.ExecContext(ctx,
		`INSERT INTO broadcast_destination (alias, next_eligible_at, updated_at) VALUES (?, ?, ?)
		ON CONFLICT(alias) DO UPDATE SET next_eligible_at = excluded.next_eligible_at, updated_at = excluded.updated_at`,
		alias, formatTime(next), formatTime(now.UTC()),
	)
	if err != nil {
		return time.Time{}, classifyBusy(fmt.Errorf("advance destination cursor: %w", err))
	}
	if err := tx.Commit(); err != nil {
		return time.Time{}, classifyBusy(fmt.Errorf("commit dispatch slot transaction: %w", err))
	}
	committed = true
	return slot, nil
}

func (s *sqliteBroadcastStore) DeferDestination(ctx context.Context, alias string, until time.Time) error {
	if s.db == nil {
		return errNilArgument("db")
	}
	if alias == "" {
		return Errorf(ErrorCodeInvalidArgument, "destination alias must not be empty")
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{})
	if err != nil {
		return classifyBusy(fmt.Errorf("begin defer destination transaction: %w", err))
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()
	var raw string
	err = tx.QueryRowContext(ctx,
		`SELECT next_eligible_at FROM broadcast_destination WHERE alias = ?`, alias).Scan(&raw)
	switch {
	case errors.Is(err, sql.ErrNoRows):
	case err != nil:
		return classifyBusy(fmt.Errorf("load destination cursor: %w", err))
	default:
		cursor, parseErr := parseTime(raw)
		if parseErr != nil {
			return Errorf(ErrorCodeBroadcastInvalid, "destination cursor is not a valid timestamp")
		}
		if !cursor.Before(until.UTC()) {
			return nil
		}
	}
	_, err = tx.ExecContext(ctx,
		`INSERT INTO broadcast_destination (alias, next_eligible_at, updated_at) VALUES (?, ?, ?)
		ON CONFLICT(alias) DO UPDATE SET next_eligible_at = excluded.next_eligible_at, updated_at = excluded.updated_at`,
		alias, formatTime(until.UTC()), formatTime(time.Now().UTC()),
	)
	if err != nil {
		return classifyBusy(fmt.Errorf("defer destination cursor: %w", err))
	}
	if err := tx.Commit(); err != nil {
		return classifyBusy(fmt.Errorf("commit defer destination transaction: %w", err))
	}
	committed = true
	return nil
}

func validSettleState(state string) bool {
	switch state {
	case BroadcastStateSucceeded, BroadcastStateFailed, BroadcastStateUnknown, BroadcastStateCancelled:
		return true
	default:
		return false
	}
}

func (s *sqliteBroadcastStore) ListHeld(ctx context.Context, alias, priority string, notBefore time.Time, limit int) ([]BroadcastItem, error) {
	if s.db == nil {
		return nil, errNilArgument("db")
	}
	if alias == "" || priority == "" {
		return nil, Errorf(ErrorCodeInvalidArgument, "destination alias and priority must not be empty")
	}
	if limit <= 0 {
		return nil, Errorf(ErrorCodeInvalidArgument, "list limit must be positive")
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+selectBroadcastItemColumns+` FROM broadcast_item
		WHERE state = '`+BroadcastStateHeld+`' AND destination_alias = ? AND priority = ? AND not_before <= ?
		ORDER BY created_at, id LIMIT ?`,
		alias, priority, formatTime(notBefore.UTC()), limit)
	if err != nil {
		return nil, classifyBusy(fmt.Errorf("list held broadcast items: %w", err))
	}
	defer func() { _ = rows.Close() }()
	items := []BroadcastItem{}
	for rows.Next() {
		item, err := scanBroadcastItem(rows)
		if err != nil {
			_ = rows.Close()
			return nil, err
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, classifyBusy(fmt.Errorf("list held broadcast items: %w", err))
	}
	return items, nil
}

func (s *sqliteBroadcastStore) ListActive(ctx context.Context, limit int) ([]BroadcastItem, error) {
	if s.db == nil {
		return nil, errNilArgument("db")
	}
	if limit <= 0 {
		return nil, Errorf(ErrorCodeInvalidArgument, "list limit must be positive")
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+selectBroadcastItemColumns+` FROM broadcast_item
		WHERE state IN ('`+BroadcastStateHeld+`','`+BroadcastStateScheduled+`','`+BroadcastStateStarted+`')
		ORDER BY not_before, created_at, id LIMIT ?`,
		limit)
	if err != nil {
		return nil, classifyBusy(fmt.Errorf("list active broadcast items: %w", err))
	}
	defer func() { _ = rows.Close() }()
	items := []BroadcastItem{}
	for rows.Next() {
		item, err := scanBroadcastItem(rows)
		if err != nil {
			_ = rows.Close()
			return nil, err
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, classifyBusy(fmt.Errorf("list active broadcast items: %w", err))
	}
	return items, nil
}

func (s *sqliteBroadcastStore) Settle(ctx context.Context, id, state, effectID string, now time.Time) error {
	if s.db == nil {
		return errNilArgument("db")
	}
	if id == "" {
		return Errorf(ErrorCodeInvalidArgument, "broadcast id must not be empty")
	}
	if !validSettleState(state) {
		return Errorf(ErrorCodeInvalidArgument, "settle state is not terminal")
	}
	result, err := s.db.ExecContext(ctx,
		`UPDATE broadcast_item SET state = ?, effect_id = ?, updated_at = ?
		WHERE id = ? AND state IN ('`+BroadcastStateHeld+`','`+BroadcastStateScheduled+`','`+BroadcastStateStarted+`','`+BroadcastStateUnknown+`')`,
		state, nullText(effectID), formatTime(now.UTC()), id)
	if err != nil {
		return classifyBusy(fmt.Errorf("settle broadcast item: %w", err))
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return classifyBusy(fmt.Errorf("settle broadcast item: %w", err))
	}
	if affected == 0 {
		_, findErr := s.Item(ctx, id)
		if findErr != nil {
			return findErr
		}
		return &Error{Code: ErrorCodeBroadcastConflict, Detail: "broadcast item is already terminal"}
	}
	return nil
}

func (s *sqliteBroadcastStore) NoteAttempt(ctx context.Context, id string, attempt int64, now time.Time) error {
	if s.db == nil {
		return errNilArgument("db")
	}
	if id == "" {
		return Errorf(ErrorCodeInvalidArgument, "broadcast id must not be empty")
	}
	if attempt < 0 {
		return Errorf(ErrorCodeInvalidArgument, "attempt count must not be negative")
	}
	_, err := s.db.ExecContext(ctx,
		`UPDATE broadcast_item SET attempt_count = CASE WHEN attempt_count < ? THEN ? ELSE attempt_count END,
		updated_at = ? WHERE id = ?`,
		attempt, attempt, formatTime(now.UTC()), id)
	if err != nil {
		return classifyBusy(fmt.Errorf("note broadcast attempt: %w", err))
	}
	return nil
}

func (s *sqliteBroadcastStore) ListItems(ctx context.Context, states []string, limit int) ([]BroadcastItem, error) {
	if s.db == nil {
		return nil, errNilArgument("db")
	}
	if limit <= 0 {
		return nil, Errorf(ErrorCodeInvalidArgument, "list limit must be positive")
	}
	for _, state := range states {
		if !validBroadcastState(state) {
			return nil, Errorf(ErrorCodeInvalidArgument, "broadcast state filter is not valid")
		}
	}
	if len(states) == 0 {
		return s.listBroadcastItems(ctx,
			`SELECT `+selectBroadcastItemColumns+` FROM broadcast_item ORDER BY created_at DESC, id DESC LIMIT ?`,
			limit)
	}
	merged := []BroadcastItem{}
	for _, state := range states {
		items, err := s.listBroadcastItems(ctx,
			`SELECT `+selectBroadcastItemColumns+` FROM broadcast_item WHERE state = ? ORDER BY created_at DESC, id DESC LIMIT ?`,
			state, limit)
		if err != nil {
			return nil, err
		}
		merged = append(merged, items...)
	}
	slices.SortFunc(merged, func(a, b BroadcastItem) int {
		if a.CreatedAt.Equal(b.CreatedAt) {
			return strings.Compare(b.ID, a.ID)
		}
		return b.CreatedAt.Compare(a.CreatedAt)
	})
	if len(merged) > limit {
		merged = merged[:limit]
	}
	return merged, nil
}

func (s *sqliteBroadcastStore) listBroadcastItems(ctx context.Context, query string, args ...any) ([]BroadcastItem, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, classifyBusy(fmt.Errorf("list broadcast items: %w", err))
	}
	defer func() { _ = rows.Close() }()
	items := []BroadcastItem{}
	for rows.Next() {
		item, err := scanBroadcastItem(rows)
		if err != nil {
			_ = rows.Close()
			return nil, err
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, classifyBusy(fmt.Errorf("list broadcast items: %w", err))
	}
	return items, nil
}
