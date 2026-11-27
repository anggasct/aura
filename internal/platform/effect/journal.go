package effect

import (
	"cmp"
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

	"github.com/anggasct/aura/internal/platform/durable"
	"github.com/anggasct/aura/internal/platform/sqlite"
	"github.com/anggasct/aura/internal/platform/turn"
)

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

func (s State) valid() bool {
	switch s {
	case StatePrepared, StateStarted, StateSucceeded, StateUnknown, StateFailed:
		return true
	default:
		return false
	}
}

const EventKindToolRequested = "tool.requested"

const EventKindChannelRequested = "channel.requested"

const toolRequestedSchemaVersion uint16 = 1

const channelRequestedSchemaVersion uint16 = 1

type toolRequestedPayload struct {
	EffectIntentID string         `json:"effect_intent_id"`
	ToolCallID     string         `json:"tool_call_id"`
	Provider       string         `json:"provider"`
	Operation      string         `json:"operation"`
	Classification Classification `json:"classification"`
	IdempotencyKey string         `json:"idempotency_key"`
	RequestDigest  string         `json:"request_digest"`
	RetryOf        string         `json:"retry_of,omitempty"`
}

type channelRequestedPayload struct {
	EffectIntentID string         `json:"effect_intent_id"`
	Provider       string         `json:"provider"`
	Operation      string         `json:"operation"`
	Classification Classification `json:"classification"`
	IdempotencyKey string         `json:"idempotency_key"`
	RequestDigest  string         `json:"request_digest"`
}

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
	LeaseToken      string
	leaseExpiresAt  time.Time
}

type PrepareRequest struct {
	SessionID      string
	TurnID         string
	ToolCallID     string
	IdempotencyKey string
	Provider       string
	Operation      string
	Classification Classification
	Request        json.RawMessage
	EventKind      string

	EventID          string
	EventSequence    uint64
	AllocateSequence bool
	EventInvocation  string
	EventBranch      string
	EventAuthor      string
}

type Journal struct {
	db       *sql.DB
	clock    durable.Clock
	logger   *slog.Logger
	observer Observer
	events   EventPublisher
	turns    TurnStore
}

type EventPublisher interface {
	Publish(*turn.Event)
}

type TurnStore interface {
	AllocateSequence(ctx context.Context, sessionID string) (uint64, error)
	AppendEvent(ctx context.Context, e *turn.Event) error
	SessionOwner(ctx context.Context, sessionID string) (string, error)
}

type Options struct {
	Clock    durable.Clock
	Logger   *slog.Logger
	Observer Observer
	Events   EventPublisher
	Turns    TurnStore
}

func NewJournal(db *sql.DB, opts Options) (*Journal, error) {
	if db == nil {
		return nil, codedError(ErrorCodeInvalidArgument, "effect: database handle must not be nil", nil)
	}
	if opts.Turns == nil {
		return nil, codedError(ErrorCodeInvalidArgument, "effect: turn store must not be nil", nil)
	}
	if opts.Clock == nil {
		opts.Clock = durable.RealClock()
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.Observer == nil {
		opts.Observer = observerFunc(noopObserver)
	}
	return &Journal{db: db, clock: opts.Clock, logger: opts.Logger, observer: opts.Observer, events: opts.Events, turns: opts.Turns}, nil
}

const insertIntentSQL = `
INSERT INTO effect_intent (
    id, session_id, turn_id, tool_call_id, idempotency_key, provider, operation,
    classification, state, request_digest, request_json, provider_receipt_json,
    safe_error_code, retry_of, prepared_at, started_at, finished_at, reconciled_at, updated_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, NULL, NULL, NULL, ?, NULL, NULL, NULL, ?)`

const insertIntentSQLWithRetry = `
INSERT INTO effect_intent (
    id, session_id, turn_id, tool_call_id, idempotency_key, provider, operation,
    classification, state, request_digest, request_json, provider_receipt_json,
    safe_error_code, retry_of, prepared_at, started_at, finished_at, reconciled_at, updated_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, NULL, NULL, ?, ?, NULL, NULL, NULL, ?)`

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

func (j *Journal) Prepare(ctx context.Context, req *PrepareRequest) (*Intent, error) {
	if req == nil {
		return nil, codedError(ErrorCodeInvalidArgument, "effect: prepare request must not be nil", nil)
	}
	if err := validatePrepare(req); err != nil {
		return nil, err
	}
	normalizedRequest, err := normalizeRequest(req.Request)
	if err != nil {
		return nil, err
	}
	digest := digestRequest(normalizedRequest)

	var intent *Intent
	var reused bool
	err = sqlite.RunInTx(ctx, j.db, func(ctx context.Context) error {
		q := sqlite.Conn(ctx, j.db)
		existing, lookupErr := intentByKey(ctx, q, req.Provider, req.Operation, req.IdempotencyKey)
		if lookupErr != nil && !errors.Is(lookupErr, errIntentNotFound) {
			return lookupErr
		}
		if existing != nil {
			if existing.RequestDigest != digest {
				return codedError(ErrorCodeIdempotencyConflict,
					fmt.Sprintf("effect: idempotency key %q already bound to a different request", req.IdempotencyKey), nil)
			}
			intent = existing
			reused = true
			return nil
		}

		now := j.clock.Now()
		intentID, idErr := randomID()
		if idErr != nil {
			return idErr
		}
		_, execErr := q.ExecContext(ctx, insertIntentSQL,
			intentID, req.SessionID, req.TurnID, req.ToolCallID, req.IdempotencyKey, req.Provider, req.Operation,
			string(req.Classification), string(StatePrepared), digest, string(normalizedRequest),
			fmtTime(now), fmtTime(now),
		)
		if execErr != nil {
			if isUniqueViolation(execErr) {
				resolved, conflictErr := j.resolvePrepareConflict(ctx, req, digest, execErr)
				if conflictErr != nil {
					return conflictErr
				}
				intent = resolved
				reused = true
				return nil
			}
			return fmt.Errorf("effect: insert intent: %w", execErr)
		}

		kind := req.EventKind
		if kind == "" {
			kind = EventKindToolRequested
		}
		var payload json.RawMessage
		var sequence uint64
		var schemaVersion uint16
		if kind == EventKindChannelRequested || req.AllocateSequence {
			var seqErr error
			sequence, seqErr = j.turns.AllocateSequence(ctx, req.SessionID)
			if seqErr != nil {
				return seqErr
			}
		}
		if kind == EventKindChannelRequested {
			var marshalErr error
			payload, marshalErr = json.Marshal(channelRequestedPayload{
				EffectIntentID: intentID,
				Provider:       req.Provider,
				Operation:      req.Operation,
				Classification: req.Classification,
				IdempotencyKey: req.IdempotencyKey,
				RequestDigest:  digest,
			})
			if marshalErr != nil {
				return fmt.Errorf("effect: marshal channel.requested payload: %w", marshalErr)
			}
			schemaVersion = channelRequestedSchemaVersion
		} else {
			var marshalErr error
			payload, marshalErr = json.Marshal(toolRequestedPayload{
				EffectIntentID: intentID,
				ToolCallID:     req.ToolCallID,
				Provider:       req.Provider,
				Operation:      req.Operation,
				Classification: req.Classification,
				IdempotencyKey: req.IdempotencyKey,
				RequestDigest:  digest,
			})
			if marshalErr != nil {
				return fmt.Errorf("effect: marshal tool.requested payload: %w", marshalErr)
			}
			if !req.AllocateSequence {
				sequence = req.EventSequence
			}
			schemaVersion = toolRequestedSchemaVersion
		}
		eventID := req.EventID
		if eventID == "" {
			var evIDErr error
			eventID, evIDErr = randomID()
			if evIDErr != nil {
				return evIDErr
			}
		}
		ev := turn.Event{
			ID:            eventID,
			SessionID:     req.SessionID,
			Sequence:      sequence,
			TurnID:        req.TurnID,
			InvocationID:  req.EventInvocation,
			Branch:        req.EventBranch,
			Author:        req.EventAuthor,
			Kind:          kind,
			SchemaVersion: schemaVersion,
			Payload:       payload,
			CreatedAt:     now,
		}
		if appendErr := j.turns.AppendEvent(ctx, &ev); appendErr != nil {
			return appendErr
		}
		if j.events != nil {
			published := ev
			sqlite.AfterCommit(ctx, j.db, func() {
				j.events.Publish(&published)
			})
		}
		intent = &Intent{
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
			RequestJSON:    normalizedRequest,
			PreparedAt:     now,
			UpdatedAt:      now,
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if !reused {
		j.observe(ctx, intent, "prepared")
	}
	return intent, nil
}

func (j *Journal) Lookup(ctx context.Context, provider, operation, idempotencyKey string, request json.RawMessage) (*Intent, bool, error) {
	normalized, err := normalizeRequest(request)
	if err != nil {
		return nil, false, err
	}
	existing, err := intentByKey(ctx, sqlite.Conn(ctx, j.db), provider, operation, idempotencyKey)
	if errors.Is(err, errIntentNotFound) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	if existing.RequestDigest != digestRequest(normalized) {
		return nil, false, codedError(ErrorCodeIdempotencyConflict,
			fmt.Sprintf("effect: idempotency key %q already bound to a different request", idempotencyKey), nil)
	}
	return existing, true, nil
}

func (j *Journal) resolvePrepareConflict(ctx context.Context, req *PrepareRequest, digest string, cause error) (*Intent, error) {
	existing, err := intentByKey(ctx, sqlite.Conn(ctx, j.db), req.Provider, req.Operation, req.IdempotencyKey)
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
	return nil, codedError(ErrorCodeIdempotencyKeyInUse,
		fmt.Sprintf("effect: idempotency key %q already in use", req.IdempotencyKey), cause)
}

func validatePrepare(req *PrepareRequest) error {
	var problems []error
	if req.SessionID == "" {
		problems = append(problems, errors.New("session_id must not be empty"))
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
	switch req.EventKind {
	case "", EventKindToolRequested:
		if req.TurnID == "" {
			problems = append(problems, errors.New("turn_id must not be empty"))
		}
		if req.ToolCallID == "" {
			problems = append(problems, errors.New("tool_call_id must not be empty"))
		}
		switch {
		case req.AllocateSequence && req.EventSequence != 0:
			problems = append(problems, errors.New("event_sequence must be zero when the journal allocates it"))
		case !req.AllocateSequence && req.EventSequence == 0:
			problems = append(problems, errors.New("event_sequence must be positive"))
		}
	case EventKindChannelRequested:
		if req.TurnID != "" {
			problems = append(problems, errors.New("turn_id must be empty for channel requests"))
		}
		if req.ToolCallID != "" {
			problems = append(problems, errors.New("tool_call_id must be empty for channel requests"))
		}
		if req.EventSequence != 0 {
			problems = append(problems, errors.New("event_sequence must be zero for channel requests; the journal allocates it"))
		}
	default:
		problems = append(problems, fmt.Errorf("event_kind %q is not supported", req.EventKind))
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

func (j *Journal) Start(ctx context.Context, id string) (*Intent, error) {
	token := newLeaseToken()
	expiresAt := j.clock.Now().Add(leaseDuration)
	intent, err := j.transition(ctx, id, &transitionSpec{
		from:           []State{StatePrepared},
		to:             StateStarted,
		setStartedAt:   true,
		leaseToken:     token,
		leaseExpiresAt: expiresAt,
		invalidCode:    ErrorCodeTransitionInvalid,
	})
	if err != nil {
		return nil, err
	}
	intent.LeaseToken = token
	intent.leaseExpiresAt = expiresAt
	return intent, nil
}

func (j *Journal) Succeed(ctx context.Context, id, token string, receipt json.RawMessage) (*Intent, error) {
	if len(receipt) > 0 && !json.Valid(receipt) {
		return nil, codedError(ErrorCodeInvalidArgument, "effect: receipt must be valid JSON", nil)
	}
	return j.transition(ctx, id, &transitionSpec{
		from:          []State{StateStarted},
		to:            StateSucceeded,
		receipt:       receipt,
		setFinishedAt: true,
		fenced:        true,
		fenceToken:    token,
		invalidCode:   ErrorCodeTransitionInvalid,
	})
}

func (j *Journal) succeedWith(ctx context.Context, started *Intent, outcome *Outcome) (*Intent, error) {
	if started == nil {
		return nil, codedError(ErrorCodeInvalidArgument, "effect: started intent must not be nil", nil)
	}
	if outcome == nil {
		return nil, codedError(ErrorCodeInvalidArgument, "effect: outcome must not be nil", nil)
	}
	if len(outcome.Receipt) > 0 && !json.Valid(outcome.Receipt) {
		return nil, codedError(ErrorCodeInvalidArgument, "effect: receipt must be valid JSON", nil)
	}
	spec := &transitionSpec{
		from:          []State{StateStarted},
		to:            StateSucceeded,
		receipt:       outcome.Receipt,
		setFinishedAt: true,
		fenced:        true,
		fenceToken:    started.LeaseToken,
		invalidCode:   ErrorCodeTransitionInvalid,
	}
	offered := outcome.Result != nil
	spec.recordResult(started.ID, started.RequestDigest, outcome.Result)
	intent, err := j.transition(ctx, started.ID, spec)
	if err != nil {
		return nil, err
	}
	if (offered && spec.result == nil) || outcome.UnrecordedResult {
		j.observe(ctx, intent, observationResultNotRecorded)
	}
	return intent, nil
}

func (j *Journal) Fail(ctx context.Context, id, token, safeErrorCode string) (*Intent, error) {
	return j.transition(ctx, id, &transitionSpec{
		from:          []State{StatePrepared, StateStarted},
		to:            StateFailed,
		safeErrorCode: safeErrorCode,
		setFinishedAt: true,
		fenced:        true,
		fenceToken:    token,
		invalidCode:   ErrorCodeTransitionInvalid,
	})
}

func (j *Journal) MarkUnknown(ctx context.Context, id, token string) (*Intent, error) {
	return j.transition(ctx, id, &transitionSpec{
		from:        []State{StateStarted},
		to:          StateUnknown,
		fenced:      true,
		fenceToken:  token,
		invalidCode: ErrorCodeTransitionInvalid,
	})
}

type Resolution struct {
	Succeeded     bool
	Receipt       json.RawMessage
	Result        json.RawMessage
	SafeErrorCode string
}

func (j *Journal) Resolve(ctx context.Context, id string, res Resolution) (*Intent, error) {
	if res.Succeeded {
		if len(res.Receipt) > 0 && !json.Valid(res.Receipt) {
			return nil, codedError(ErrorCodeInvalidArgument, "effect: receipt must be valid JSON", nil)
		}
		spec := &transitionSpec{
			from:            []State{StateUnknown},
			to:              StateSucceeded,
			receipt:         res.Receipt,
			setFinishedAt:   true,
			setReconciledAt: true,
			invalidCode:     ErrorCodeTransitionInvalid,
		}
		if res.Result != nil {
			unknown, err := j.Get(ctx, id)
			if err != nil {
				return nil, err
			}
			spec.recordResult(id, unknown.RequestDigest, res.Result)
		}
		intent, err := j.transition(ctx, id, spec)
		if err != nil {
			return nil, err
		}
		if res.Result != nil && spec.result == nil {
			j.observe(ctx, intent, observationResultNotRecorded)
		}
		return intent, nil
	}
	return j.transition(ctx, id, &transitionSpec{
		from:            []State{StateUnknown},
		to:              StateFailed,
		safeErrorCode:   cmp.Or(res.SafeErrorCode, "effect_reconciliation_failed"),
		setFinishedAt:   true,
		setReconciledAt: true,
		invalidCode:     ErrorCodeTransitionInvalid,
	})
}

type transitionSpec struct {
	from            []State
	to              State
	receipt         json.RawMessage
	result          json.RawMessage
	resultDigest    string
	safeErrorCode   string
	setStartedAt    bool
	setFinishedAt   bool
	setReconciledAt bool
	leaseToken      string
	leaseExpiresAt  time.Time
	fenced          bool
	fenceToken      string
	invalidCode     ErrorCode
}

func (s *transitionSpec) recordResult(id, requestDigest string, result json.RawMessage) {
	if result == nil || !recordable(result) {
		return
	}
	s.result = result
	s.resultDigest = resultDigest(id, requestDigest, result)
}

func (j *Journal) transition(ctx context.Context, id string, spec *transitionSpec) (*Intent, error) {
	if id == "" {
		return nil, codedError(ErrorCodeInvalidArgument, "effect: id must not be empty", nil)
	}
	err := sqlite.RunInTx(ctx, j.db, func(ctx context.Context) error {
		q := sqlite.Conn(ctx, j.db)
		var currentState State
		var storedToken sql.NullString
		err := q.QueryRowContext(ctx, `SELECT state, lease_token FROM effect_intent WHERE id = ?`, id).Scan(&currentState, &storedToken)
		if errors.Is(err, sql.ErrNoRows) {
			return codedError(ErrorCodeNotFound, fmt.Sprintf("effect: intent %s not found", id), nil)
		}
		if err != nil {
			return fmt.Errorf("effect: read intent %s state: %w", id, err)
		}
		if spec.fenced && currentState == StateStarted && spec.fenceToken == "" {
			return codedError(ErrorCodeInvalidArgument, "effect: a started intent is settled only with its lease token", nil)
		}
		if !stateIn(currentState, spec.from) {
			if spec.fenced && currentState != StatePrepared && storedToken.String != spec.fenceToken {
				return leaseLostError(id)
			}
			return codedError(spec.invalidCode,
				fmt.Sprintf("effect: intent %s is %s, cannot transition to %s", id, currentState, spec.to), nil)
		}

		now := j.clock.Now()
		var receiptArg any
		if spec.receipt != nil {
			receiptArg = string(spec.receipt)
		}
		var resultArg any
		if spec.result != nil {
			resultArg = string(spec.result)
		}
		var leaseExpiresArg any
		if !spec.leaseExpiresAt.IsZero() {
			leaseExpiresArg = fmtLeaseTime(spec.leaseExpiresAt)
		}
		res, err := q.ExecContext(ctx, `
		UPDATE effect_intent
		SET state = ?,
		    updated_at = ?,
		    started_at = CASE WHEN ? THEN ? ELSE started_at END,
		    finished_at = CASE WHEN ? THEN ? ELSE finished_at END,
		    reconciled_at = CASE WHEN ? THEN ? ELSE reconciled_at END,
		    provider_receipt_json = ?,
		    result_json = ?,
		    result_digest = ?,
		    safe_error_code = ?,
		    lease_token = COALESCE(?, lease_token),
		    lease_expires_at = COALESCE(?, lease_expires_at)
		WHERE id = ? AND state = ? AND (? OR lease_token IS ?)`,
			string(spec.to), fmtTime(now),
			boolToInt(spec.setStartedAt), fmtTime(now),
			boolToInt(spec.setFinishedAt), fmtTime(now),
			boolToInt(spec.setReconciledAt), fmtTime(now),
			receiptArg, resultArg, nullableString(spec.resultDigest), nullableString(spec.safeErrorCode),
			nullableString(spec.leaseToken), leaseExpiresArg,
			id, string(currentState), boolToInt(!spec.fenced), nullableString(spec.fenceToken),
		)
		if err != nil {
			return fmt.Errorf("effect: transition intent %s to %s: %w", id, spec.to, err)
		}
		affected, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("effect: read transition rows affected: %w", err)
		}
		if affected == 0 && spec.fenced {
			return leaseLostError(id)
		}
		if affected == 0 {
			return codedError(spec.invalidCode,
				fmt.Sprintf("effect: intent %s changed state before transition to %s", id, spec.to), nil)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	intent, err := j.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	j.observe(ctx, intent, string(spec.to))
	return intent, nil
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
	intent, err := scanIntent(sqlite.Conn(ctx, j.db).QueryRowContext(ctx, selectIntentByIDSQL, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, codedError(ErrorCodeNotFound, fmt.Sprintf("effect: intent %s not found", id), nil)
	}
	if err != nil {
		return nil, fmt.Errorf("effect: get intent %s: %w", id, err)
	}
	return intent, nil
}

func (j *Journal) ListByState(ctx context.Context, state State, limit int) ([]Intent, error) {
	if limit < 0 {
		return nil, codedError(ErrorCodeInvalidArgument, "effect: limit must not be negative", nil)
	}
	if !state.valid() {
		return nil, codedError(ErrorCodeInvalidArgument, "effect: state is invalid", nil)
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
	rows, err := sqlite.Conn(ctx, j.db).QueryContext(ctx, query, args...)
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

func fmtLeaseTime(t time.Time) string {
	return t.UTC().Format(leaseTimeLayout)
}

func parseLeaseTime(raw string) (time.Time, error) {
	t, err := time.Parse(leaseTimeLayout, raw)
	if err != nil {
		return time.Time{}, fmt.Errorf("effect: parse lease_expires_at: %w", err)
	}
	return t, nil
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
