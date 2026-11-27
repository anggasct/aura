package operation

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/anggasct/aura/internal/platform/effect"
	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/sqlite"
)

const (
	maxKeyLength     = 256
	ReceiptRetention = 30 * 24 * time.Hour
)

var errReceiptExists = errors.New("operation: a receipt already exists for the key")

type receipt struct {
	key       string
	operation string
	digest    string
	surface   Surface
	result    json.RawMessage
	createdAt time.Time
}

type receiptStore struct {
	db *sql.DB
}

func (s *receiptStore) lookup(ctx context.Context, key string) (receipt, bool, error) {
	var found receipt
	var surface, result, createdAt string
	err := sqlite.Conn(ctx, s.db).QueryRowContext(ctx, `
		SELECT idempotency_key, operation_id, request_digest, surface, result_json, created_at
		FROM operation_receipt
		WHERE idempotency_key = ?`, key,
	).Scan(&found.key, &found.operation, &found.digest, &surface, &result, &createdAt)
	if errors.Is(err, sql.ErrNoRows) {
		return receipt{}, false, nil
	}
	if err != nil {
		return receipt{}, false, fmt.Errorf("operation: read receipt: %w", err)
	}
	created, err := effect.ParseLeaseTime(createdAt)
	if err != nil {
		return receipt{}, false, wrapError(ErrorCodeFailed, "a receipt has an unreadable time", err)
	}
	found.surface = Surface(surface)
	found.result = json.RawMessage(result)
	found.createdAt = created
	return found, true, nil
}

func (s *receiptStore) insert(ctx context.Context, row *receipt) error {
	if row == nil {
		return nilArgument("receipt")
	}
	if row.key == "" || len(row.key) > maxKeyLength {
		return NewError(ErrorCodeInvalidArgument, "operation: the idempotency key must be 1 to 256 bytes")
	}
	if row.surface != SurfaceChat && row.surface != SurfaceAgent {
		return NewError(ErrorCodeInvalidArgument, "operation: a receipt belongs to the chat or agent path")
	}
	if !json.Valid(row.result) {
		return NewError(ErrorCodeInvalidArgument, "operation: a receipt result must be valid JSON")
	}
	res, err := sqlite.Conn(ctx, s.db).ExecContext(ctx, `
		INSERT INTO operation_receipt (idempotency_key, operation_id, request_digest, surface, result_json, created_at)
		VALUES (?, ?, ?, ?, ?, ?)`,
		row.key, row.operation, row.digest, string(row.surface), string(row.result), effect.FormatLeaseTime(row.createdAt),
	)
	if err != nil {
		if fault.HasCode(sqlite.Classify(err), sqlite.ErrorCodeConstraintUnique) || fault.HasCode(sqlite.Classify(err), sqlite.ErrorCodeConstraintPrimaryKey) {
			return errReceiptExists
		}
		return fmt.Errorf("operation: insert receipt: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("operation: read receipt rows affected: %w", err)
	}
	if affected != 1 {
		return NewError(ErrorCodeFailed, "operation: a receipt insert changed no row")
	}
	return nil
}

func PruneReceipts(ctx context.Context, db *sql.DB, before time.Time) (int, error) {
	if db == nil {
		return 0, nilArgument("database")
	}
	if before.IsZero() {
		return 0, NewError(ErrorCodeInvalidArgument, "operation: the retention cutoff must not be zero")
	}
	res, err := sqlite.Conn(ctx, db).ExecContext(ctx,
		`DELETE FROM operation_receipt WHERE created_at < ?`, effect.FormatLeaseTime(before))
	if err != nil {
		return 0, fmt.Errorf("operation: prune receipts: %w", err)
	}
	deleted, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("operation: read pruned receipts: %w", err)
	}
	if deleted > math.MaxInt {
		return 0, NewError(ErrorCodeFailed, "operation: the pruned receipt count overflows")
	}
	return int(deleted), nil
}

func receiptData(descriptor *Descriptor, data json.RawMessage) (json.RawMessage, error) {
	var secret []string
	for i := range descriptor.Output.Fields {
		if descriptor.Output.Fields[i].Secret {
			secret = append(secret, descriptor.Output.Fields[i].Name)
		}
	}
	if len(secret) == 0 {
		return data, nil
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(data, &object); err != nil {
		return nil, wrapError(ErrorCodeFailed, "a result is not a JSON object", err)
	}
	for _, name := range secret {
		delete(object, name)
	}
	encoded, err := encodeJSON(object)
	if err != nil {
		return nil, err
	}
	return encoded, nil
}
