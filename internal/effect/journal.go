package effect

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/anggasct/aura/internal/store"
)

// Classification declares how an effectful operation may be safely recovered.
// It is set by tool/provider registration; the model cannot override it.
type Classification string

const (
	ClassificationReadOnly     Classification = "read_only"
	ClassificationIdempotent   Classification = "idempotent"
	ClassificationEffectful    Classification = "effectful"
	ClassificationIrreversible Classification = "irreversible"
)

func (c Classification) valid() bool {
	switch c {
	case ClassificationReadOnly, ClassificationIdempotent, ClassificationEffectful, ClassificationIrreversible:
		return true
	}
	return false
}

type State string

const (
	StatePrepared  State = "prepared"
	StateStarted   State = "started"
	StateSucceeded State = "succeeded"
	StateUnknown   State = "unknown"
	StateFailed    State = "failed"
)

// EventKindToolRequested marks a runtime event emitted atomically with the
// creation of a prepared effect intent.
const EventKindToolRequested = "tool.requested"

const toolRequestedSchemaVersion uint16 = 1

type toolRequestedPayload struct {
	EffectIntentID string         `json:"effect_intent_id"`
	ToolCallID     string         `json:"tool_call_id"`
	Provider       string         `json:"provider"`
	Operation      string         `json:"operation"`
	Classification Classification `json:"classification"`
	IdempotencyKey string         `json:"idempotency_key"`
	RequestDigest  string         `json:"request_digest"`
}

// Intent is one durable effectful-operation record.
type Intent struct {
	ID              string
	SessionID       string
	TurnID          string
	ToolCallID      string
	IdempotencyKey  string
	Provider        string
	Operation       string
	Classification  Classification
	State           State
	RequestDigest   string
	RequestJSON     json.RawMessage
	ProviderReceipt json.RawMessage
	SafeErrorCode   string
	RetryOf         string
	PreparedAt      time.Time
	StartedAt       *time.Time
	FinishedAt      *time.Time
	ReconciledAt    *time.Time
	UpdatedAt       time.Time
}

// PrepareRequest creates a prepared intent. Request must be canonical bytes:
// its digest is the idempotency replay key, so the caller owns normalization.
// The event identity fields name the tool.requested runtime event appended in
// the same transaction; the caller owns session sequence allocation.
type PrepareRequest struct {
	SessionID      string
	TurnID         string
	ToolCallID     string
	IdempotencyKey string
	Provider       string
	Operation      string
	Classification Classification
	Request        json.RawMessage

	EventID         string
	EventSequence   uint64
	EventInvocation string
	EventBranch     string
	EventAuthor     string
}

// Journal durably records effectful operation intent and drives the
// prepared->started->terminal state machine with crash-safe transitions and
// lease-based recovery that never executes the same intent twice.
type Journal struct {
	db     *sql.DB
	now    func() time.Time
	logger *slog.Logger
}

type Options struct {
	Now    func() time.Time
	Logger *slog.Logger
}

func NewJournal(db *sql.DB, opts Options) (*Journal, error) {
	if db == nil {
		return nil, codedError(ErrorCodeInvalidArgument, "effect: database handle must not be nil", nil)
	}
	if opts.Now == nil {
		opts.Now = func() time.Time { return time.Now().UTC() }
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	return &Journal{db: db, now: opts.Now, logger: opts.Logger}, nil
}

const insertIntentSQL = `
INSERT INTO effect_intent (
    id, session_id, turn_id, tool_call_id, idempotency_key, provider, operation,
    classification, state, request_digest, request_json, provider_receipt_json,
    safe_error_code, retry_of, prepared_at, started_at, finished_at, reconciled_at, updated_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, NULL, NULL, NULL, ?, NULL, NULL, NULL, ?)`

const selectIntentByKeySQL = `
SELECT id, session_id, turn_id, tool_call_id, idempotency_key, provider, operation,
       classification, state, request_digest, request_json, provider_receipt_json,
       safe_error_code, retry_of, prepared_at, started_at, finished_at, reconciled_at, updated_at
FROM effect_intent
WHERE provider = ? AND operation = ? AND idempotency_key = ?`

const selectIntentByIDSQL = `
SELECT id, session_id, turn_id, tool_call_id, idempotency_key, provider, operation,
       classification, state, request_digest, request_json, provider_receipt_json,
       safe_error_code, retry_of, prepared_at, started_at, finished_at, reconciled_at, updated_at
FROM effect_intent
WHERE id = ?`

// Prepare atomically records a prepared intent and its tool.requested runtime
// event before any provider invocation. The (provider, operation,
// idempotency_key) triple is unique: a replay with the same request digest
// returns the existing intent without appending a second event; a different
// digest is a conflict.
func (j *Journal) Prepare(ctx context.Context, req *PrepareRequest) (*Intent, error) {
	if req == nil {
		return nil, codedError(ErrorCodeInvalidArgument, "effect: prepare request must not be nil", nil)
	}
	if err := validatePrepare(req); err != nil {
		return nil, err
	}
	digest := digestRequest(req.Request)

	tx, err := j.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("effect: begin prepare transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	existing, err := intentByKey(ctx, tx, req.Provider, req.Operation, req.IdempotencyKey)
	if err != nil && !errors.Is(err, errIntentNotFound) {
		return nil, err
	}
	if existing != nil {
		if existing.RequestDigest != digest {
			return nil, codedError(ErrorCodeIdempotencyConflict,
				fmt.Sprintf("effect: idempotency key %q already bound to a different request", req.IdempotencyKey), nil)
		}
		if err := tx.Rollback(); err != nil {
			return nil, fmt.Errorf("effect: rollback idempotent prepare: %w", err)
		}
		return existing, nil
	}

	now := j.now()
	intentID, err := randomID()
	if err != nil {
		return nil, err
	}
	_, err = tx.ExecContext(ctx, insertIntentSQL,
		intentID, req.SessionID, req.TurnID, req.ToolCallID, req.IdempotencyKey, req.Provider, req.Operation,
		string(req.Classification), string(StatePrepared), digest, string(req.Request),
		fmtTime(now), fmtTime(now),
	)
	if err != nil {
		if isUniqueViolation(err) {
			return j.resolvePrepareConflict(ctx, req, digest, err)
		}
		return nil, fmt.Errorf("effect: insert intent: %w", err)
	}

	payload, err := json.Marshal(toolRequestedPayload{
		EffectIntentID: intentID,
		ToolCallID:     req.ToolCallID,
		Provider:       req.Provider,
		Operation:      req.Operation,
		Classification: req.Classification,
		IdempotencyKey: req.IdempotencyKey,
		RequestDigest:  digest,
	})
	if err != nil {
		return nil, fmt.Errorf("effect: marshal tool.requested payload: %w", err)
	}
	eventID := req.EventID
	if eventID == "" {
		eventID, err = randomID()
		if err != nil {
			return nil, err
		}
	}
	if err := store.AppendEventTx(ctx, tx, &store.RuntimeEvent{
		ID:            eventID,
		SessionID:     req.SessionID,
		Sequence:      req.EventSequence,
		TurnID:        req.TurnID,
		InvocationID:  req.EventInvocation,
		Branch:        req.EventBranch,
		Author:        req.EventAuthor,
		Kind:          EventKindToolRequested,
		SchemaVersion: toolRequestedSchemaVersion,
		Payload:       payload,
		CreatedAt:     now,
	}); err != nil {
		return nil, err
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("effect: commit prepare: %w", err)
	}
	return &Intent{
		ID:             intentID,
		SessionID:      req.SessionID,
		TurnID:         req.TurnID,
		ToolCallID:     req.ToolCallID,
		IdempotencyKey: req.IdempotencyKey,
		Provider:       req.Provider,
		Operation:      req.Operation,
		Classification: req.Classification,
		State:          StatePrepared,
		RequestDigest:  digest,
		RequestJSON:    req.Request,
		PreparedAt:     now,
		UpdatedAt:      now,
	}, nil
}

// resolvePrepareConflict handles a concurrent prepare that won the unique
// insert. The winner is read outside this transaction: a deferred snapshot can
// miss an uncommitted winner, but a fresh read sees it. A matching digest is
// the idempotent answer and returns the existing intent; anything else is a
// conflict. It never returns a nil intent with a nil error.
func (j *Journal) resolvePrepareConflict(ctx context.Context, req *PrepareRequest, digest string, cause error) (*Intent, error) {
	existing, err := intentByKey(ctx, j.db, req.Provider, req.Operation, req.IdempotencyKey)
	if err != nil && !errors.Is(err, errIntentNotFound) {
		return nil, err
	}
	if existing != nil {
		if existing.RequestDigest != digest {
			return nil, codedError(ErrorCodeIdempotencyConflict,
				fmt.Sprintf("effect: idempotency key %q already bound to a different request", req.IdempotencyKey), nil)
		}
		return existing, nil
	}
	return nil, codedError(ErrorCodeIdempotencyConflict,
		fmt.Sprintf("effect: idempotency key %q already in use", req.IdempotencyKey), cause)
}

func validatePrepare(req *PrepareRequest) error {
	var problems []error
	if req.SessionID == "" {
		problems = append(problems, errors.New("session_id must not be empty"))
	}
	if req.TurnID == "" {
		problems = append(problems, errors.New("turn_id must not be empty"))
	}
	if req.ToolCallID == "" {
		problems = append(problems, errors.New("tool_call_id must not be empty"))
	}
	if req.IdempotencyKey == "" {
		problems = append(problems, errors.New("idempotency_key must not be empty"))
	}
	if req.Provider == "" {
		problems = append(problems, errors.New("provider must not be empty"))
	}
	if req.Operation == "" {
		problems = append(problems, errors.New("operation must not be empty"))
	}
	if !req.Classification.valid() {
		problems = append(problems, codedError(ErrorCodeClassificationMissing, "classification is missing or invalid", nil))
		return errors.Join(problems...)
	}
	if len(req.Request) == 0 || !json.Valid(req.Request) {
		problems = append(problems, errors.New("request must be valid JSON"))
	}
	if req.EventSequence == 0 {
		problems = append(problems, errors.New("event_sequence must be positive"))
	}
	if len(problems) > 0 {
		return codedError(ErrorCodeInvalidArgument, "effect: invalid prepare request", errors.Join(problems...))
	}
	return nil
}

func digestRequest(req json.RawMessage) string {
	sum := sha256.Sum256(req)
	return hex.EncodeToString(sum[:])
}

// Start durably transitions a prepared intent to started. A caller must not
// invoke the provider until Start has returned: only a committed started row
// proves the intent will be observed by recovery.
func (j *Journal) Start(ctx context.Context, id string) (*Intent, error) {
	return j.transition(ctx, id, &transitionSpec{
		from:         []State{StatePrepared},
		to:           StateStarted,
		setStartedAt: true,
		invalidCode:  ErrorCodeTransitionInvalid,
	})
}

// Succeed records a provider receipt and transitions started to succeeded.
func (j *Journal) Succeed(ctx context.Context, id string, receipt json.RawMessage) (*Intent, error) {
	if len(receipt) > 0 && !json.Valid(receipt) {
		return nil, codedError(ErrorCodeInvalidArgument, "effect: receipt must be valid JSON", nil)
	}
	return j.transition(ctx, id, &transitionSpec{
		from:          []State{StateStarted},
		to:            StateSucceeded,
		receipt:       receipt,
		setFinishedAt: true,
		invalidCode:   ErrorCodeTransitionInvalid,
	})
}

// Fail records a definite safe error code and transitions prepared or started
// to failed (a prepared intent may fail on validation or policy before
// invocation).
func (j *Journal) Fail(ctx context.Context, id, safeErrorCode string) (*Intent, error) {
	return j.transition(ctx, id, &transitionSpec{
		from:          []State{StatePrepared, StateStarted},
		to:            StateFailed,
		safeErrorCode: safeErrorCode,
		setFinishedAt: true,
		invalidCode:   ErrorCodeTransitionInvalid,
	})
}

// MarkUnknown transitions started to unknown after an ambiguous outcome - a
// lost response, connection loss, process crash, or response-commit failure
// after invocation began. Recovery (Recover) makes the same transition for
// intents still started at startup.
func (j *Journal) MarkUnknown(ctx context.Context, id string) (*Intent, error) {
	return j.transition(ctx, id, &transitionSpec{
		from:        []State{StateStarted},
		to:          StateUnknown,
		invalidCode: ErrorCodeTransitionInvalid,
	})
}

type transitionSpec struct {
	from          []State
	to            State
	receipt       json.RawMessage
	safeErrorCode string
	setStartedAt  bool
	setFinishedAt bool
	invalidCode   ErrorCode
}

func (j *Journal) transition(ctx context.Context, id string, spec *transitionSpec) (*Intent, error) {
	if id == "" {
		return nil, codedError(ErrorCodeInvalidArgument, "effect: id must not be empty", nil)
	}
	// The store connection policy opens every connection with _txlock=immediate,
	// so this BEGIN takes the write lock up front: the SELECT-state-then-
	// conditional-UPDATE below serializes against any other writer on this row,
	// and a concurrent transition reads the committed state instead of a stale
	// snapshot.
	tx, err := j.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("effect: begin transition transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var currentState State
	err = tx.QueryRowContext(ctx, `SELECT state FROM effect_intent WHERE id = ?`, id).Scan(&currentState)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, codedError(ErrorCodeNotFound, fmt.Sprintf("effect: intent %s not found", id), nil)
	}
	if err != nil {
		return nil, fmt.Errorf("effect: read intent %s state: %w", id, err)
	}
	if !stateIn(currentState, spec.from) {
		return nil, codedError(spec.invalidCode,
			fmt.Sprintf("effect: intent %s is %s, cannot transition to %s", id, currentState, spec.to), nil)
	}

	now := j.now()
	var receiptArg any
	if spec.receipt != nil {
		receiptArg = string(spec.receipt)
	}
	// started_at and finished_at are advanced only on the transitions that
	// reach them; CASE WHEN keeps the statement static so a hostlie value can
	// never splice into the SQL text.
	res, err := tx.ExecContext(ctx, `
		UPDATE effect_intent
		SET state = ?,
		    updated_at = ?,
		    started_at = CASE WHEN ? THEN ? ELSE started_at END,
		    finished_at = CASE WHEN ? THEN ? ELSE finished_at END,
		    provider_receipt_json = ?,
		    safe_error_code = ?
		WHERE id = ? AND state = ?`,
		string(spec.to), fmtTime(now),
		boolToInt(spec.setStartedAt), fmtTime(now),
		boolToInt(spec.setFinishedAt), fmtTime(now),
		receiptArg, nullableString(spec.safeErrorCode),
		id, string(currentState),
	)
	if err != nil {
		return nil, fmt.Errorf("effect: transition intent %s to %s: %w", id, spec.to, err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return nil, fmt.Errorf("effect: read transition rows affected: %w", err)
	}
	if affected == 0 {
		return nil, codedError(spec.invalidCode,
			fmt.Sprintf("effect: intent %s changed state before transition to %s", id, spec.to), nil)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("effect: commit transition to %s: %w", spec.to, err)
	}
	return j.Get(ctx, id)
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func stateIn(s State, set []State) bool {
	for _, want := range set {
		if s == want {
			return true
		}
	}
	return false
}

func (j *Journal) Get(ctx context.Context, id string) (*Intent, error) {
	if id == "" {
		return nil, codedError(ErrorCodeInvalidArgument, "effect: id must not be empty", nil)
	}
	intent, err := scanIntent(j.db.QueryRowContext(ctx, selectIntentByIDSQL, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, codedError(ErrorCodeNotFound, fmt.Sprintf("effect: intent %s not found", id), nil)
	}
	if err != nil {
		return nil, fmt.Errorf("effect: get intent %s: %w", id, err)
	}
	return intent, nil
}

// ListByState returns intents in a given state ordered for recovery (oldest
// first). A negative limit is rejected; a zero limit means no bound.
func (j *Journal) ListByState(ctx context.Context, state State, limit int) ([]Intent, error) {
	if limit < 0 {
		return nil, codedError(ErrorCodeInvalidArgument, "effect: limit must not be negative", nil)
	}
	query := `
	SELECT id, session_id, turn_id, tool_call_id, idempotency_key, provider, operation,
	       classification, state, request_digest, request_json, provider_receipt_json,
	       safe_error_code, retry_of, prepared_at, started_at, finished_at, reconciled_at, updated_at
	FROM effect_intent
	WHERE state = ?
	ORDER BY updated_at ASC`
	args := []any{string(state)}
	if limit > 0 {
		query += `
	LIMIT ?`
		args = append(args, limit)
	}
	rows, err := j.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("effect: list intents in state %s: %w", state, err)
	}
	defer func() { _ = rows.Close() }()

	var intents []Intent
	for rows.Next() {
		intent, err := scanIntent(rows)
		if err != nil {
			return nil, fmt.Errorf("effect: list intents in state %s: %w", state, err)
		}
		intents = append(intents, *intent)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("effect: list intents in state %s: %w", state, err)
	}
	return intents, nil
}

type scanner interface {
	Scan(dest ...any) error
}

func intentByKey(ctx context.Context, q queryer, provider, operation, idempotencyKey string) (*Intent, error) {
	intent, err := scanIntent(q.QueryRowContext(ctx, selectIntentByKeySQL, provider, operation, idempotencyKey))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, errIntentNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("effect: read intent by key: %w", err)
	}
	return intent, nil
}

func scanIntent(s scanner) (*Intent, error) {
	var i Intent
	var classification, state string
	var request, receipt sql.NullString
	var safeErrorCode, retryOf, preparedAt, startedAt, finishedAt, reconciledAt, updatedAt sql.NullString
	if err := s.Scan(
		&i.ID, &i.SessionID, &i.TurnID, &i.ToolCallID, &i.IdempotencyKey, &i.Provider, &i.Operation,
		&classification, &state, &i.RequestDigest, &request, &receipt,
		&safeErrorCode, &retryOf, &preparedAt, &startedAt, &finishedAt, &reconciledAt, &updatedAt,
	); err != nil {
		return nil, err
	}
	i.Classification = Classification(classification)
	i.State = State(state)
	if request.Valid {
		i.RequestJSON = json.RawMessage(request.String)
	}
	if receipt.Valid {
		i.ProviderReceipt = json.RawMessage(receipt.String)
	}
	if safeErrorCode.Valid {
		i.SafeErrorCode = safeErrorCode.String
	}
	if retryOf.Valid {
		i.RetryOf = retryOf.String
	}
	prepared, err := parseTime(preparedAt.String, "prepared_at")
	if err != nil {
		return nil, err
	}
	i.PreparedAt = prepared
	if err := assignNullableTime(&i.StartedAt, startedAt, "started_at"); err != nil {
		return nil, err
	}
	if err := assignNullableTime(&i.FinishedAt, finishedAt, "finished_at"); err != nil {
		return nil, err
	}
	if err := assignNullableTime(&i.ReconciledAt, reconciledAt, "reconciled_at"); err != nil {
		return nil, err
	}
	updated, err := parseTime(updatedAt.String, "updated_at")
	if err != nil {
		return nil, err
	}
	i.UpdatedAt = updated
	return &i, nil
}

type queryer interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

func fmtTime(t time.Time) string {
	return t.UTC().Format(time.RFC3339Nano)
}

func parseTime(raw, what string) (time.Time, error) {
	t, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		return time.Time{}, fmt.Errorf("effect: parse %s: %w", what, err)
	}
	return t, nil
}

func assignNullableTime(dst **time.Time, v sql.NullString, what string) error {
	if !v.Valid {
		*dst = nil
		return nil
	}
	t, err := time.Parse(time.RFC3339Nano, v.String)
	if err != nil {
		return fmt.Errorf("effect: parse %s: %w", what, err)
	}
	*dst = &t
	return nil
}

func nullableString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func randomID() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("effect: generate id: %w", err)
	}
	return hex.EncodeToString(buf), nil
}
