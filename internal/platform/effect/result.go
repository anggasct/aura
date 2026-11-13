package effect

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"unicode/utf8"
)

const MaxResultBytes = 1 << 20

const (
	resultDigestVersion          = "effect-result/v1"
	observationResultNotRecorded = "result_not_recorded"
	observationResultReplayed    = "result_replayed"
)

const selectRecordedResultSQL = `
SELECT id, state, request_digest, result_json, result_digest
FROM effect_intent
WHERE id = ?`

func recordable(result json.RawMessage) bool {
	return len(result) <= MaxResultBytes && utf8.Valid(result) && json.Valid(result)
}

func resultDigest(id, requestDigest string, result json.RawMessage) string {
	sum := sha256.Sum256([]byte(resultDigestVersion + "\n" + id + "\n" + requestDigest + "\n" + string(result)))
	return hex.EncodeToString(sum[:])
}

func (j *Journal) RecordedResult(ctx context.Context, intent *Intent) (json.RawMessage, error) {
	if intent == nil {
		return nil, codedError(ErrorCodeInvalidArgument, "effect: intent must not be nil", nil)
	}
	var (
		id, requestDigest string
		state             State
		result, digest    sql.NullString
	)
	err := j.db.QueryRowContext(ctx, selectRecordedResultSQL, intent.ID).Scan(&id, &state, &requestDigest, &result, &digest)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, codedError(ErrorCodeNotFound, fmt.Sprintf("effect: intent %s not found", intent.ID), nil)
	}
	if err != nil {
		return nil, fmt.Errorf("effect: read recorded result of intent %s: %w", intent.ID, err)
	}
	if state != StateSucceeded || !result.Valid || !digest.Valid {
		return nil, codedError(ErrorCodeResultUnavailable,
			fmt.Sprintf("effect: intent %s has no recorded result", id), nil)
	}
	record := json.RawMessage(result.String)
	if resultDigest(id, requestDigest, record) != digest.String {
		return nil, codedError(ErrorCodeResultCorrupt,
			fmt.Sprintf("effect: recorded result of intent %s does not match its digest", id), nil)
	}
	j.observe(ctx, intent, observationResultReplayed)
	return record, nil
}
