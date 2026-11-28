package broadcast

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/sqlite"
)

type SQLiteStore struct {
	db *sql.DB
}

func NewSQLiteStore(db *sql.DB) (*SQLiteStore, error) {
	if db == nil {
		return nil, fault.New(ErrorCodeInvalidArgument, "database must not be nil")
	}
	return &SQLiteStore{db: db}, nil
}

type rowScanner interface {
	Scan(dest ...any) error
}

func isConstraintUnique(classified error) bool {
	return fault.HasCode(classified, sqlite.ErrorCodeConstraintUnique) ||
		fault.HasCode(classified, sqlite.ErrorCodeConstraintPrimaryKey)
}

func wrapTxError(op string, err error) error {
	if err == nil {
		return nil
	}
	if _, ok := fault.CodeOf[ErrorCode](err); ok {
		return err
	}
	return fault.Wrap(ErrorCodeUnavailable, op, err)
}

func validBroadcastState(state string) bool {
	switch state {
	case StateHeld, StateScheduled, StateStarted,
		StateSucceeded, StateFailed, StateUnknown,
		StateCancelled:
		return true
	default:
		return false
	}
}

func validSettleState(state string) bool {
	switch state {
	case StateSucceeded, StateFailed, StateUnknown, StateCancelled:
		return true
	default:
		return false
	}
}

func nullableText(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func toFullItem(record *ItemRecord) FullItem {
	return FullItem{
		Item:           recordToItem(record),
		AttemptCount:   record.AttemptCount,
		EffectID:       record.EffectID,
		DigestParentID: record.DigestParentID,
	}
}

const selectBroadcastItemColumns = `
	id, producer, idempotency_key, content_digest, priority, destination_alias,
	content_json, state, not_before, attempt_count, effect_id, digest_parent_id,
	created_at, updated_at, tainted
`

func (s *SQLiteStore) Insert(ctx context.Context, record *ItemRecord) (ItemRecord, bool, error) {
	if s == nil || s.db == nil {
		return ItemRecord{}, false, fault.New(ErrorCodeInvalidArgument, "broadcast store must not be nil")
	}
	if record == nil {
		return ItemRecord{}, false, fault.New(ErrorCodeInvalidArgument, "item must not be nil")
	}
	if record.ID == "" || record.Producer == "" || record.IdempotencyKey == "" {
		return ItemRecord{}, false, fault.New(ErrorCodeInvalidArgument, "broadcast identity must not be empty")
	}
	if !validPriority(record.Priority) {
		return ItemRecord{}, false, fault.New(ErrorCodeInvalidArgument, "broadcast priority is not valid")
	}
	if record.DestinationAlias == "" {
		return ItemRecord{}, false, fault.New(ErrorCodeInvalidArgument, "broadcast destination must not be empty")
	}
	if !json.Valid([]byte(record.ContentJSON)) {
		return ItemRecord{}, false, fault.New(ErrorCodeInvalidArgument, "broadcast content is not valid JSON")
	}
	if !validBroadcastState(record.State) {
		return ItemRecord{}, false, fault.New(ErrorCodeInvalidArgument, "broadcast state is not valid")
	}
	if record.AttemptCount < 0 {
		return ItemRecord{}, false, fault.New(ErrorCodeInvalidArgument, "broadcast attempt count must not be negative")
	}

	_, err := sqlite.Conn(ctx, s.db).ExecContext(ctx,
		`INSERT INTO broadcast_item (
			id, producer, idempotency_key, content_digest, priority, destination_alias,
			content_json, state, not_before, attempt_count, effect_id, digest_parent_id,
			created_at, updated_at, tainted
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		record.ID,
		record.Producer,
		record.IdempotencyKey,
		record.ContentDigest,
		record.Priority,
		record.DestinationAlias,
		record.ContentJSON,
		record.State,
		sqlite.FormatTime(record.NotBefore.UTC()),
		record.AttemptCount,
		nullableText(record.EffectID),
		nullableText(record.DigestParentID),
		sqlite.FormatTime(record.CreatedAt.UTC()),
		sqlite.FormatTime(record.UpdatedAt.UTC()),
		record.Tainted,
	)
	if err == nil {
		return *record, false, nil
	}
	classified := sqlite.Classify(err)
	if !isConstraintUnique(classified) {
		return ItemRecord{}, false, fault.Wrap(ErrorCodeUnavailable, "insert broadcast item", classified)
	}
	existing, found, findErr := s.ItemByKey(ctx, record.Producer, record.IdempotencyKey)
	if findErr != nil {
		return ItemRecord{}, false, findErr
	}
	if !found {
		return ItemRecord{}, false, fault.Wrap(ErrorCodeUnavailable, "insert broadcast item", classified)
	}
	if existing.ContentDigest != record.ContentDigest || existing.Tainted != record.Tainted {
		return ItemRecord{}, false, fault.New(
			ErrorCodeConflict,
			fmt.Sprintf("idempotency key %q is already claimed with different content", record.IdempotencyKey),
		)
	}
	return existing, true, nil
}

func (s *SQLiteStore) Item(ctx context.Context, id string) (ItemRecord, error) {
	if s == nil || s.db == nil {
		return ItemRecord{}, fault.New(ErrorCodeInvalidArgument, "broadcast store must not be nil")
	}
	item, found, err := s.findItem(ctx,
		`SELECT `+selectBroadcastItemColumns+` FROM broadcast_item WHERE id = ?`, id)
	if err != nil {
		return ItemRecord{}, err
	}
	if !found {
		return ItemRecord{}, fault.New(ErrorCodeNotFound, "broadcast item not found")
	}
	return item, nil
}

func (s *SQLiteStore) Load(ctx context.Context, id string) (FullItem, bool, error) {
	if s == nil || s.db == nil {
		return FullItem{}, false, fault.New(ErrorCodeInvalidArgument, "broadcast store must not be nil")
	}
	item, found, err := s.findItem(ctx,
		`SELECT `+selectBroadcastItemColumns+` FROM broadcast_item WHERE id = ?`, id)
	if err != nil {
		return FullItem{}, false, err
	}
	if !found {
		return FullItem{}, false, nil
	}
	return toFullItem(&item), true, nil
}

func (s *SQLiteStore) ItemByKey(ctx context.Context, producer, key string) (ItemRecord, bool, error) {
	if s == nil || s.db == nil {
		return ItemRecord{}, false, fault.New(ErrorCodeInvalidArgument, "broadcast store must not be nil")
	}
	return s.findItem(ctx,
		`SELECT `+selectBroadcastItemColumns+` FROM broadcast_item WHERE producer = ? AND idempotency_key = ?`,
		producer, key)
}

func (s *SQLiteStore) CreateDigest(ctx context.Context, parent *ItemRecord, childIDs []string) (ItemRecord, bool, error) {
	if s == nil || s.db == nil {
		return ItemRecord{}, false, fault.New(ErrorCodeInvalidArgument, "broadcast store must not be nil")
	}
	if parent == nil {
		return ItemRecord{}, false, fault.New(ErrorCodeInvalidArgument, "parent must not be nil")
	}
	if len(childIDs) == 0 {
		return ItemRecord{}, false, fault.New(ErrorCodeInvalidArgument, "digest children must not be empty")
	}
	var out ItemRecord
	var replayed bool
	err := sqlite.RunInTx(ctx, s.db, func(ctx context.Context) error {
		conn := sqlite.Conn(ctx, s.db)
		_, err := conn.ExecContext(ctx,
			`INSERT INTO broadcast_item (
				id, producer, idempotency_key, content_digest, priority, destination_alias,
				content_json, state, not_before, attempt_count, effect_id, digest_parent_id,
				created_at, updated_at, tainted
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			parent.ID,
			parent.Producer,
			parent.IdempotencyKey,
			parent.ContentDigest,
			parent.Priority,
			parent.DestinationAlias,
			parent.ContentJSON,
			parent.State,
			sqlite.FormatTime(parent.NotBefore.UTC()),
			parent.AttemptCount,
			nullableText(parent.EffectID),
			nullableText(parent.DigestParentID),
			sqlite.FormatTime(parent.CreatedAt.UTC()),
			sqlite.FormatTime(parent.UpdatedAt.UTC()),
			parent.Tainted,
		)
		if err != nil {
			classified := sqlite.Classify(err)
			if !isConstraintUnique(classified) {
				return fault.Wrap(ErrorCodeUnavailable, "insert digest parent", classified)
			}
			existing, found, findErr := s.ItemByKey(ctx, parent.Producer, parent.IdempotencyKey)
			if findErr != nil {
				return findErr
			}
			if !found {
				return fault.Wrap(ErrorCodeUnavailable, "insert digest parent", classified)
			}
			out = existing
			replayed = true
			return nil
		}
		link, err := conn.PrepareContext(ctx,
			`UPDATE broadcast_item SET state = ?, digest_parent_id = ?, updated_at = ? WHERE id = ? AND state = ?`)
		if err != nil {
			return fault.Wrap(ErrorCodeUnavailable, "prepare digest link", sqlite.Classify(err))
		}
		defer func() { _ = link.Close() }()
		var linked int64
		for _, id := range childIDs {
			result, err := link.ExecContext(ctx,
				StateCancelled, parent.ID, sqlite.FormatTime(parent.UpdatedAt.UTC()), id, StateHeld)
			if err != nil {
				return fault.Wrap(ErrorCodeUnavailable, "link digest children", sqlite.Classify(err))
			}
			affected, err := result.RowsAffected()
			if err != nil {
				return fault.Wrap(ErrorCodeUnavailable, "link digest children", sqlite.Classify(err))
			}
			linked += affected
		}
		if linked != int64(len(childIDs)) {
			return fault.New(ErrorCodeConflict, "digest children are not all held")
		}
		out = *parent
		return nil
	})
	if err != nil {
		return ItemRecord{}, false, wrapTxError("create digest", err)
	}
	return out, replayed, nil
}

func (s *SQLiteStore) findItem(ctx context.Context, query string, args ...any) (ItemRecord, bool, error) {
	item, found, err := scanOneBroadcastItem(sqlite.Conn(ctx, s.db).QueryRowContext(ctx, query, args...))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ItemRecord{}, false, nil
		}
		if _, ok := fault.CodeOf[ErrorCode](err); ok {
			return ItemRecord{}, false, err
		}
		return ItemRecord{}, false, fault.Wrap(ErrorCodeUnavailable, "load broadcast item", sqlite.Classify(err))
	}
	return item, found, nil
}

func scanOneBroadcastItem(row rowScanner) (ItemRecord, bool, error) {
	var item ItemRecord
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
		&item.Tainted,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return ItemRecord{}, false, nil
	}
	if err != nil {
		return ItemRecord{}, false, err
	}
	return finishBroadcastItemScan(&item, notBeforeRaw, createdAtRaw, updatedAtRaw, effectID, digestParentID)
}

func scanBroadcastItem(rows *sql.Rows) (ItemRecord, error) {
	var item ItemRecord
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
		&item.Tainted,
	); err != nil {
		return ItemRecord{}, fault.Wrap(ErrorCodeUnavailable, "scan broadcast item", sqlite.Classify(err))
	}
	finished, _, err := finishBroadcastItemScan(&item, notBeforeRaw, createdAtRaw, updatedAtRaw, effectID, digestParentID)
	return finished, err
}

func finishBroadcastItemScan(item *ItemRecord, notBeforeRaw, createdAtRaw, updatedAtRaw string, effectID, digestParentID sql.NullString) (ItemRecord, bool, error) {
	if !validPriority(item.Priority) || !validBroadcastState(item.State) {
		return ItemRecord{}, false, fault.New(ErrorCodeUnavailable, "broadcast row carries an invalid priority or state")
	}
	notBefore, err := sqlite.ParseTime(notBeforeRaw)
	if err != nil {
		return ItemRecord{}, false, fault.New(ErrorCodeUnavailable, "broadcast not_before is not a valid timestamp")
	}
	createdAt, err := sqlite.ParseTime(createdAtRaw)
	if err != nil {
		return ItemRecord{}, false, fault.New(ErrorCodeUnavailable, "broadcast created_at is not a valid timestamp")
	}
	updatedAt, err := sqlite.ParseTime(updatedAtRaw)
	if err != nil {
		return ItemRecord{}, false, fault.New(ErrorCodeUnavailable, "broadcast updated_at is not a valid timestamp")
	}
	item.NotBefore = notBefore
	item.CreatedAt = createdAt
	item.UpdatedAt = updatedAt
	item.EffectID = effectID.String
	item.DigestParentID = digestParentID.String
	return *item, true, nil
}

func (s *SQLiteStore) DestinationCursor(ctx context.Context, alias string) (time.Time, bool, error) {
	if s == nil || s.db == nil {
		return time.Time{}, false, fault.New(ErrorCodeInvalidArgument, "broadcast store must not be nil")
	}
	if alias == "" {
		return time.Time{}, false, fault.New(ErrorCodeInvalidArgument, "destination alias must not be empty")
	}
	var raw string
	err := sqlite.Conn(ctx, s.db).QueryRowContext(ctx,
		`SELECT next_eligible_at FROM broadcast_destination WHERE alias = ?`, alias).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, false, nil
	}
	if err != nil {
		return time.Time{}, false, fault.Wrap(ErrorCodeUnavailable, "load destination cursor", sqlite.Classify(err))
	}
	cursor, err := sqlite.ParseTime(raw)
	if err != nil {
		return time.Time{}, false, fault.New(ErrorCodeUnavailable, "destination cursor is not a valid timestamp")
	}
	return cursor, true, nil
}

func (s *SQLiteStore) ClaimDispatchSlot(ctx context.Context, alias string, now time.Time, gap time.Duration) (time.Time, error) {
	if s == nil || s.db == nil {
		return time.Time{}, fault.New(ErrorCodeInvalidArgument, "broadcast store must not be nil")
	}
	if alias == "" {
		return time.Time{}, fault.New(ErrorCodeInvalidArgument, "destination alias must not be empty")
	}
	if gap < 0 {
		return time.Time{}, fault.New(ErrorCodeInvalidArgument, "dispatch gap must not be negative")
	}
	var slot time.Time
	err := sqlite.RunInTx(ctx, s.db, func(ctx context.Context) error {
		conn := sqlite.Conn(ctx, s.db)
		var raw string
		err := conn.QueryRowContext(ctx,
			`SELECT next_eligible_at FROM broadcast_destination WHERE alias = ?`, alias).Scan(&raw)
		slot = now.UTC()
		switch {
		case errors.Is(err, sql.ErrNoRows):
		case err != nil:
			return fault.Wrap(ErrorCodeUnavailable, "load destination cursor", sqlite.Classify(err))
		default:
			cursor, parseErr := sqlite.ParseTime(raw)
			if parseErr != nil {
				return fault.New(ErrorCodeUnavailable, "destination cursor is not a valid timestamp")
			}
			slot = cursor.UTC()
			if slot.Before(now.UTC()) {
				slot = now.UTC()
			}
		}
		next := slot.Add(gap)
		_, err = conn.ExecContext(ctx,
			`INSERT INTO broadcast_destination (alias, next_eligible_at, updated_at) VALUES (?, ?, ?)
			ON CONFLICT(alias) DO UPDATE SET next_eligible_at = excluded.next_eligible_at, updated_at = excluded.updated_at`,
			alias, sqlite.FormatTime(next), sqlite.FormatTime(now.UTC()),
		)
		if err != nil {
			return fault.Wrap(ErrorCodeUnavailable, "advance destination cursor", sqlite.Classify(err))
		}
		return nil
	})
	if err != nil {
		return time.Time{}, wrapTxError("claim dispatch slot", err)
	}
	return slot, nil
}

func (s *SQLiteStore) DeferDestination(ctx context.Context, alias string, until time.Time) error {
	if s == nil || s.db == nil {
		return fault.New(ErrorCodeInvalidArgument, "broadcast store must not be nil")
	}
	if alias == "" {
		return fault.New(ErrorCodeInvalidArgument, "destination alias must not be empty")
	}
	err := sqlite.RunInTx(ctx, s.db, func(ctx context.Context) error {
		conn := sqlite.Conn(ctx, s.db)
		var raw string
		err := conn.QueryRowContext(ctx,
			`SELECT next_eligible_at FROM broadcast_destination WHERE alias = ?`, alias).Scan(&raw)
		switch {
		case errors.Is(err, sql.ErrNoRows):
		case err != nil:
			return fault.Wrap(ErrorCodeUnavailable, "load destination cursor", sqlite.Classify(err))
		default:
			cursor, parseErr := sqlite.ParseTime(raw)
			if parseErr != nil {
				return fault.New(ErrorCodeUnavailable, "destination cursor is not a valid timestamp")
			}
			if !cursor.Before(until.UTC()) {
				return nil
			}
		}
		_, err = conn.ExecContext(ctx,
			`INSERT INTO broadcast_destination (alias, next_eligible_at, updated_at) VALUES (?, ?, ?)
			ON CONFLICT(alias) DO UPDATE SET next_eligible_at = excluded.next_eligible_at, updated_at = excluded.updated_at`,
			alias, sqlite.FormatTime(until.UTC()), sqlite.FormatTime(time.Now().UTC()),
		)
		if err != nil {
			return fault.Wrap(ErrorCodeUnavailable, "defer destination cursor", sqlite.Classify(err))
		}
		return nil
	})
	return wrapTxError("defer destination", err)
}

func (s *SQLiteStore) ListHeld(ctx context.Context, alias, priority string, notBefore time.Time, limit int) ([]FullItem, error) {
	if s == nil || s.db == nil {
		return nil, fault.New(ErrorCodeInvalidArgument, "broadcast store must not be nil")
	}
	if alias == "" || priority == "" {
		return nil, fault.New(ErrorCodeInvalidArgument, "destination alias and priority must not be empty")
	}
	if limit <= 0 {
		return nil, fault.New(ErrorCodeInvalidArgument, "list limit must be positive")
	}
	records, err := s.listBroadcastItems(ctx,
		`SELECT `+selectBroadcastItemColumns+` FROM broadcast_item
		WHERE state = '`+StateHeld+`' AND destination_alias = ? AND priority = ? AND not_before <= ?
		ORDER BY created_at, id LIMIT ?`,
		alias, priority, sqlite.FormatTime(notBefore.UTC()), limit)
	if err != nil {
		return nil, err
	}
	out := make([]FullItem, 0, len(records))
	for i := range records {
		out = append(out, toFullItem(&records[i]))
	}
	return out, nil
}

func (s *SQLiteStore) ListActive(ctx context.Context, limit int) ([]FullItem, error) {
	if s == nil || s.db == nil {
		return nil, fault.New(ErrorCodeInvalidArgument, "broadcast store must not be nil")
	}
	if limit <= 0 {
		return nil, fault.New(ErrorCodeInvalidArgument, "list limit must be positive")
	}
	records, err := s.listBroadcastItems(ctx,
		`SELECT `+selectBroadcastItemColumns+` FROM broadcast_item
		WHERE state IN ('`+StateHeld+`','`+StateScheduled+`','`+StateStarted+`')
		ORDER BY not_before, created_at, id LIMIT ?`,
		limit)
	if err != nil {
		return nil, err
	}
	out := make([]FullItem, 0, len(records))
	for i := range records {
		out = append(out, toFullItem(&records[i]))
	}
	return out, nil
}

func (s *SQLiteStore) Settle(ctx context.Context, id, state, effectID string, now time.Time) error {
	if s == nil || s.db == nil {
		return fault.New(ErrorCodeInvalidArgument, "broadcast store must not be nil")
	}
	if id == "" {
		return fault.New(ErrorCodeInvalidArgument, "broadcast id must not be empty")
	}
	if !validSettleState(state) {
		return fault.New(ErrorCodeInvalidArgument, "settle state is not terminal")
	}
	result, err := sqlite.Conn(ctx, s.db).ExecContext(ctx,
		`UPDATE broadcast_item SET state = ?, effect_id = ?, updated_at = ?
		WHERE id = ? AND state IN ('`+StateHeld+`','`+StateScheduled+`','`+StateStarted+`','`+StateUnknown+`')`,
		state, nullableText(effectID), sqlite.FormatTime(now.UTC()), id)
	if err != nil {
		return fault.Wrap(ErrorCodeUnavailable, "settle broadcast item", sqlite.Classify(err))
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fault.Wrap(ErrorCodeUnavailable, "settle broadcast item", sqlite.Classify(err))
	}
	if affected == 0 {
		if _, findErr := s.Item(ctx, id); findErr != nil {
			return findErr
		}
		return fault.New(ErrorCodeConflict, "broadcast item is already terminal")
	}
	return nil
}

func (s *SQLiteStore) NoteAttempt(ctx context.Context, id string, attempt int64, now time.Time) error {
	if s == nil || s.db == nil {
		return fault.New(ErrorCodeInvalidArgument, "broadcast store must not be nil")
	}
	if id == "" {
		return fault.New(ErrorCodeInvalidArgument, "broadcast id must not be empty")
	}
	if attempt < 0 {
		return fault.New(ErrorCodeInvalidArgument, "attempt count must not be negative")
	}
	_, err := sqlite.Conn(ctx, s.db).ExecContext(ctx,
		`UPDATE broadcast_item SET attempt_count = CASE WHEN attempt_count < ? THEN ? ELSE attempt_count END,
		updated_at = ? WHERE id = ?`,
		attempt, attempt, sqlite.FormatTime(now.UTC()), id)
	if err != nil {
		return fault.Wrap(ErrorCodeUnavailable, "note broadcast attempt", sqlite.Classify(err))
	}
	return nil
}

func (s *SQLiteStore) ListItems(ctx context.Context, states []string, limit int) ([]ItemRecord, error) {
	if s == nil || s.db == nil {
		return nil, fault.New(ErrorCodeInvalidArgument, "broadcast store must not be nil")
	}
	if limit <= 0 {
		return nil, fault.New(ErrorCodeInvalidArgument, "list limit must be positive")
	}
	for _, state := range states {
		if !validBroadcastState(state) {
			return nil, fault.New(ErrorCodeInvalidArgument, "broadcast state filter is not valid")
		}
	}
	if len(states) == 0 {
		return s.listBroadcastItems(ctx,
			`SELECT `+selectBroadcastItemColumns+` FROM broadcast_item ORDER BY created_at DESC, id DESC LIMIT ?`,
			limit)
	}
	merged := []ItemRecord{}
	for _, state := range states {
		items, err := s.listBroadcastItems(ctx,
			`SELECT `+selectBroadcastItemColumns+` FROM broadcast_item WHERE state = ? ORDER BY created_at DESC, id DESC LIMIT ?`,
			state, limit)
		if err != nil {
			return nil, err
		}
		merged = append(merged, items...)
	}
	slices.SortFunc(merged, func(a, b ItemRecord) int {
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

func (s *SQLiteStore) listBroadcastItems(ctx context.Context, query string, args ...any) ([]ItemRecord, error) {
	rows, err := sqlite.Conn(ctx, s.db).QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fault.Wrap(ErrorCodeUnavailable, "list broadcast items", sqlite.Classify(err))
	}
	defer func() { _ = rows.Close() }()
	items := []ItemRecord{}
	for rows.Next() {
		item, err := scanBroadcastItem(rows)
		if err != nil {
			_ = rows.Close()
			return nil, err
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fault.Wrap(ErrorCodeUnavailable, "list broadcast items", sqlite.Classify(err))
	}
	return items, nil
}
