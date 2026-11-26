package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/anggasct/aura/internal/platform/durable"
	"github.com/anggasct/aura/internal/platform/fault"
	dbsqlite "github.com/anggasct/aura/internal/platform/sqlite"
	"github.com/anggasct/aura/internal/platform/turn"
	runtimesessions "github.com/anggasct/aura/internal/platform/turn/sessions"
)

const (
	idempotencyWindow          = 24 * time.Hour
	acceptedEventSchemaVersion = 1
	turnStateActive            = "active"
	turnStateQueued            = "queued"
)

var errAbortSessionTx = errors.New("abort session transaction")

type turnStore interface {
	ClaimDedupe(ctx context.Context, claim *turn.DedupeClaim) (originalTurnID string, created bool, err error)
	AllocateSequence(ctx context.Context, sessionID string) (uint64, error)
	AppendEvent(ctx context.Context, e *turn.Event) error
	TurnUsed(ctx context.Context, turnID string) (bool, error)
}

type SessionStoreOptions struct {
	DB    *sql.DB
	Clock durable.Clock
	Turns turnStore
}

type SessionStore struct {
	db    *sql.DB
	clock durable.Clock
	turns turnStore
}

func NewSessionStore(opts *SessionStoreOptions) (*SessionStore, error) {
	if opts == nil {
		return nil, invalidArgument("durable session store options are required")
	}
	if opts.DB == nil {
		return nil, invalidArgument("durable session store requires a database")
	}
	if opts.Turns == nil {
		return nil, invalidArgument("durable session store requires a turn store")
	}
	clock := opts.Clock
	if clock == nil {
		clock = durable.RealClock()
	}
	return &SessionStore{db: opts.DB, clock: clock, turns: opts.Turns}, nil
}

func (s *SessionStore) inTx(ctx context.Context, fn func(ctx context.Context, q dbsqlite.Querier) (commit bool, err error)) error {
	err := dbsqlite.RunInTx(ctx, s.db, func(ctx context.Context) error {
		commit, err := fn(ctx, dbsqlite.Conn(ctx, s.db))
		if err != nil {
			return err
		}
		if !commit {
			return errAbortSessionTx
		}
		return nil
	})
	if errors.Is(err, errAbortSessionTx) {
		return nil
	}
	return err
}

func (s *SessionStore) Admit(ctx context.Context, req *runtimesessions.AdmitRequest) (runtimesessions.AdmitResult, error) {
	if err := validateAdmitRequest(req); err != nil {
		return runtimesessions.AdmitResult{}, err
	}
	var result runtimesessions.AdmitResult
	err := s.inTx(ctx, func(ctx context.Context, q dbsqlite.Querier) (bool, error) {
		adm := &admission{q: q, turns: s.turns, req: req, now: s.clock.Now().UTC()}
		var commit bool
		var err error
		result, commit, err = adm.run(ctx)
		return commit, err
	})
	if err != nil {
		return runtimesessions.AdmitResult{}, err
	}
	return result, nil
}

func admissionRefused(detail string) error {
	return fault.New(turn.ErrorCodeInvalidArgument, detail)
}

func validateAdmitRequest(req *runtimesessions.AdmitRequest) error {
	switch {
	case req == nil:
		return admissionRefused("durable session admit requires a request")
	case req.Turn.TurnID == "":
		return admissionRefused("durable session admit requires a turn id")
	case req.AcceptedEventID == "":
		return admissionRefused("durable session admit requires an accepted event id")
	case req.MaxPending <= 0:
		return admissionRefused("durable session admit requires a positive max pending bound")
	}
	if err := req.Turn.Validate(); err != nil {
		return fmt.Errorf("durable session admit: %w", err)
	}
	return nil
}

type admission struct {
	q     dbsqlite.Querier
	turns turnStore
	req   *runtimesessions.AdmitRequest
	now   time.Time
}

func (a *admission) run(ctx context.Context) (runtimesessions.AdmitResult, bool, error) {
	desc := &a.req.Turn
	if desc.IdempotencyKey != "" {
		original, created, err := a.turns.ClaimDedupe(ctx, &turn.DedupeClaim{
			Source:     desc.Origin,
			ExternalID: desc.IdempotencyKey,
			TurnID:     desc.TurnID,
			Now:        a.now,
			ExpiresAt:  a.now.Add(idempotencyWindow),
		})
		if err != nil {
			return runtimesessions.AdmitResult{}, false, fmt.Errorf("durable session admit: %w", err)
		}
		if !created {
			return runtimesessions.AdmitResult{Replayed: true, OriginalTurnID: original}, false, nil
		}
	}
	if err := rejectHeldTurn(ctx, a.q, desc.TurnID); err != nil {
		return runtimesessions.AdmitResult{}, false, err
	}
	if err := rejectUsedTurn(ctx, a.q, a.turns, desc.TurnID); err != nil {
		return runtimesessions.AdmitResult{}, false, err
	}
	held, err := countHeldTurns(ctx, a.q, desc.SessionID)
	if err != nil {
		return runtimesessions.AdmitResult{}, false, err
	}
	if held.total() >= a.req.MaxPending {
		return runtimesessions.AdmitResult{Overloaded: true}, false, nil
	}
	accepted, err := a.appendAccepted(ctx)
	if err != nil {
		return runtimesessions.AdmitResult{}, false, err
	}
	state, err := held.grantState()
	if err != nil {
		return runtimesessions.AdmitResult{}, false, err
	}
	if err := a.insertTurn(ctx, queueEntry{state: state, sequence: accepted.Sequence}); err != nil {
		return runtimesessions.AdmitResult{}, false, err
	}
	result := runtimesessions.AdmitResult{
		Sequence:       accepted.Sequence,
		EventPayload:   accepted.Payload,
		EventCreatedAt: accepted.CreatedAt.UTC().Format(time.RFC3339Nano),
	}
	if state == turnStateActive {
		result.ToStart = &runtimesessions.QueuedTurn{Descriptor: *desc, Sequence: accepted.Sequence}
	}
	return result, true, nil
}

func (a *admission) appendAccepted(ctx context.Context) (*turn.Event, error) {
	desc := &a.req.Turn
	payload, err := turn.AcceptedPayload(desc.Origin, desc.PrincipalID, desc.AgentID, desc.Persona)
	if err != nil {
		return nil, fmt.Errorf("durable session admit: encode accepted payload: %w", err)
	}
	sequence, err := a.turns.AllocateSequence(ctx, desc.SessionID)
	if err != nil {
		return nil, fmt.Errorf("durable session admit: %w", err)
	}
	accepted := &turn.Event{
		ID:            a.req.AcceptedEventID,
		SessionID:     desc.SessionID,
		Sequence:      sequence,
		TurnID:        desc.TurnID,
		Author:        desc.PrincipalID,
		Kind:          turn.EventKindTurnAccepted,
		SchemaVersion: acceptedEventSchemaVersion,
		Payload:       payload,
		CreatedAt:     a.now,
	}
	if err := a.turns.AppendEvent(ctx, accepted); err != nil {
		return nil, fmt.Errorf("durable session admit: %w", err)
	}
	return accepted, nil
}

type queueEntry struct {
	state    string
	sequence uint64
}

func (a *admission) insertTurn(ctx context.Context, entry queueEntry) error {
	desc := &a.req.Turn
	descriptorJSON, err := json.Marshal(desc)
	if err != nil {
		return fmt.Errorf("durable session admit: encode turn: %w", err)
	}
	stamp := a.now.Format(time.RFC3339Nano)
	if _, err := a.q.ExecContext(ctx, `INSERT INTO durable_session_turn (turn_id, session_id, sequence, state, descriptor_json, deadline, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, desc.TurnID, desc.SessionID, entry.sequence, entry.state, string(descriptorJSON), deadlineColumn(desc.Deadline), stamp, stamp); err != nil {
		return fmt.Errorf("durable session admit: write turn: %w", err)
	}
	return nil
}

func deadlineColumn(deadline time.Time) sql.NullString {
	if deadline.IsZero() {
		return sql.NullString{}
	}
	return sql.NullString{String: deadline.UTC().Format(time.RFC3339Nano), Valid: true}
}

func rejectHeldTurn(ctx context.Context, q dbsqlite.Querier, turnID string) error {
	var held bool
	if err := q.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM durable_session_turn WHERE turn_id = ?)`, turnID).Scan(&held); err != nil {
		return fmt.Errorf("durable session admit: read turn: %w", err)
	}
	if held {
		return admissionRefused(fmt.Sprintf("turn %q is already held by a session", turnID))
	}
	return nil
}

func rejectUsedTurn(ctx context.Context, q dbsqlite.Querier, turns turnStore, turnID string) error {
	var used bool
	if err := q.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM durable_run WHERE key = ?)`, turnID).Scan(&used); err != nil {
		return fmt.Errorf("durable session admit: read turn use: %w", err)
	}
	if !used {
		var err error
		used, err = turns.TurnUsed(ctx, turnID)
		if err != nil {
			return fmt.Errorf("durable session admit: read turn use: %w", err)
		}
	}
	if used {
		return admissionRefused(fmt.Sprintf("turn %q was already used", turnID))
	}
	return nil
}

type heldTurns struct {
	active int
	queued int
}

func (h heldTurns) total() int {
	return h.active + h.queued
}

func (h heldTurns) grantState() (string, error) {
	switch {
	case h.active > 0:
		return turnStateQueued, nil
	case h.queued > 0:
		return "", fault.New(ErrorCodeSessionStateInvalid, "session holds queued turns and no active turn")
	default:
		return turnStateActive, nil
	}
}

func countHeldTurns(ctx context.Context, q dbsqlite.Querier, sessionID string) (heldTurns, error) {
	var held heldTurns
	if err := q.QueryRowContext(ctx, `SELECT COALESCE(SUM(state = 'active'), 0), COALESCE(SUM(state = 'queued'), 0)
FROM durable_session_turn WHERE session_id = ?`, sessionID).Scan(&held.active, &held.queued); err != nil {
		return heldTurns{}, fmt.Errorf("durable session admit: count turns: %w", err)
	}
	return held, nil
}

type turnRef struct {
	sessionID string
	turnID    string
}

func (s *SessionStore) Release(ctx context.Context, sessionID, turnID string) (runtimesessions.ReleaseResult, error) {
	if sessionID == "" {
		return runtimesessions.ReleaseResult{}, invalidArgument("durable session release requires a session id")
	}
	if turnID == "" {
		return runtimesessions.ReleaseResult{}, invalidArgument("durable session release requires a turn id")
	}
	var result runtimesessions.ReleaseResult
	err := s.inTx(ctx, func(ctx context.Context, q dbsqlite.Querier) (bool, error) {
		var err error
		result, err = s.release(ctx, q, turnRef{sessionID: sessionID, turnID: turnID})
		return true, err
	})
	if err != nil {
		return runtimesessions.ReleaseResult{}, err
	}
	return result, nil
}

func (s *SessionStore) release(ctx context.Context, q dbsqlite.Querier, ref turnRef) (runtimesessions.ReleaseResult, error) {
	var holder, state string
	err := q.QueryRowContext(ctx, `SELECT session_id, state FROM durable_session_turn WHERE turn_id = ?`, ref.turnID).Scan(&holder, &state)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		depth, err := queuedDepth(ctx, q, ref.sessionID)
		return runtimesessions.ReleaseResult{QueueDepth: depth}, err
	case err != nil:
		return runtimesessions.ReleaseResult{}, fmt.Errorf("durable session release: read turn: %w", err)
	case holder != ref.sessionID:
		return runtimesessions.ReleaseResult{}, invalidArgument(fmt.Sprintf("release for turn %q does not belong to the session", ref.turnID))
	case state != turnStateActive:
		return runtimesessions.ReleaseResult{}, invalidArgument(fmt.Sprintf("release for turn %q does not match the active turn", ref.turnID))
	}
	if _, err := q.ExecContext(ctx, `DELETE FROM durable_session_turn WHERE turn_id = ?`, ref.turnID); err != nil {
		return runtimesessions.ReleaseResult{}, fmt.Errorf("durable session release: delete turn: %w", err)
	}
	next, found, err := s.grantNext(ctx, q, ref.sessionID)
	if err != nil {
		return runtimesessions.ReleaseResult{}, err
	}
	depth, err := queuedDepth(ctx, q, ref.sessionID)
	if err != nil {
		return runtimesessions.ReleaseResult{}, err
	}
	result := runtimesessions.ReleaseResult{QueueDepth: depth}
	if found {
		result.ToStart = &next
	}
	return result, nil
}

func (s *SessionStore) grantNext(ctx context.Context, q dbsqlite.Querier, sessionID string) (runtimesessions.QueuedTurn, bool, error) {
	var turnID, descriptorJSON string
	var sequence uint64
	err := q.QueryRowContext(ctx, `SELECT turn_id, descriptor_json, sequence FROM durable_session_turn
WHERE session_id = ? AND state = 'queued' ORDER BY sequence LIMIT 1`, sessionID).Scan(&turnID, &descriptorJSON, &sequence)
	if errors.Is(err, sql.ErrNoRows) {
		return runtimesessions.QueuedTurn{}, false, nil
	}
	if err != nil {
		return runtimesessions.QueuedTurn{}, false, fmt.Errorf("durable session release: read next turn: %w", err)
	}
	desc, err := decodeDescriptor(descriptorJSON)
	if err != nil {
		return runtimesessions.QueuedTurn{}, false, fmt.Errorf("durable session release: %w", err)
	}
	if _, err := q.ExecContext(ctx, `UPDATE durable_session_turn SET state = 'active', updated_at = ? WHERE turn_id = ?`,
		s.clock.Now().UTC().Format(time.RFC3339Nano), turnID); err != nil {
		return runtimesessions.QueuedTurn{}, false, fmt.Errorf("durable session release: grant next turn: %w", err)
	}
	return runtimesessions.QueuedTurn{Descriptor: desc, Sequence: sequence}, true, nil
}

func queuedDepth(ctx context.Context, q dbsqlite.Querier, sessionID string) (int, error) {
	var depth int
	if err := q.QueryRowContext(ctx, `SELECT COUNT(*) FROM durable_session_turn WHERE session_id = ? AND state = 'queued'`, sessionID).Scan(&depth); err != nil {
		return 0, fmt.Errorf("durable session release: count queued turns: %w", err)
	}
	return depth, nil
}

func decodeDescriptor(raw string) (runtimesessions.Descriptor, error) {
	var desc runtimesessions.Descriptor
	if err := json.Unmarshal([]byte(raw), &desc); err != nil {
		return runtimesessions.Descriptor{}, fmt.Errorf("decode turn descriptor: %w", err)
	}
	return desc, nil
}

func (s *SessionStore) Status(ctx context.Context, sessionID string) (runtimesessions.StatusResult, error) {
	if sessionID == "" {
		return runtimesessions.StatusResult{}, invalidArgument("durable session status requires a session id")
	}
	var result runtimesessions.StatusResult
	var activeID, descriptorJSON, deadline sql.NullString
	var activeSequence sql.Null[uint64]
	if err := dbsqlite.Conn(ctx, s.db).QueryRowContext(ctx, `SELECT
    (SELECT COUNT(*) FROM durable_session_turn WHERE session_id = ?1 AND state = 'queued'),
    (SELECT COALESCE(MAX(sequence), 0) FROM durable_session_turn WHERE session_id = ?1),
    active.turn_id, active.descriptor_json, active.sequence, active.deadline
FROM (SELECT ?1 AS session_id) AS wanted
LEFT JOIN durable_session_turn AS active ON active.session_id = wanted.session_id AND active.state = 'active'`, sessionID).
		Scan(&result.QueueDepth, &result.LastSequence, &activeID, &descriptorJSON, &activeSequence, &deadline); err != nil {
		return runtimesessions.StatusResult{}, fmt.Errorf("durable session status: %w", err)
	}
	if !activeID.Valid {
		return result, nil
	}
	desc, err := decodeDescriptor(descriptorJSON.String)
	if err != nil {
		return runtimesessions.StatusResult{}, fmt.Errorf("durable session status: %w", err)
	}
	result.ActiveTurnID = activeID.String
	result.Active = &runtimesessions.ActiveTurn{Descriptor: desc, Sequence: activeSequence.V}
	if deadline.Valid {
		at, err := time.Parse(time.RFC3339Nano, deadline.String)
		if err != nil {
			return runtimesessions.StatusResult{}, fmt.Errorf("durable session status: parse deadline: %w", err)
		}
		result.Deadline = at.UTC().Format(time.RFC3339Nano)
	}
	return result, nil
}

func (s *SessionStore) ActiveTurns(ctx context.Context) ([]runtimesessions.QueuedTurn, error) {
	rows, err := dbsqlite.Conn(ctx, s.db).QueryContext(ctx, `SELECT turn_id, descriptor_json, sequence FROM durable_session_turn WHERE state = 'active' ORDER BY rowid`)
	if err != nil {
		return nil, fmt.Errorf("durable session active turns: %w", err)
	}
	defer func() { _ = rows.Close() }()
	active := []runtimesessions.QueuedTurn{}
	for rows.Next() {
		var turnID, descriptorJSON string
		var sequence uint64
		if err := rows.Scan(&turnID, &descriptorJSON, &sequence); err != nil {
			return nil, fmt.Errorf("durable session active turns: read turn: %w", err)
		}
		desc, err := decodeDescriptor(descriptorJSON)
		if err != nil {
			return nil, fmt.Errorf("durable session active turns: turn %q: %w", turnID, err)
		}
		active = append(active, runtimesessions.QueuedTurn{Descriptor: desc, Sequence: sequence})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("durable session active turns: %w", err)
	}
	return active, nil
}

func (s *SessionStore) Abort(ctx context.Context, sessionID string) (runtimesessions.AbortResult, error) {
	if sessionID == "" {
		return runtimesessions.AbortResult{}, invalidArgument("durable session abort requires a session id")
	}
	var dropped []runtimesessions.QueuedTurn
	err := s.inTx(ctx, func(ctx context.Context, q dbsqlite.Querier) (bool, error) {
		var err error
		dropped, err = heldInGrantOrder(ctx, q, sessionID)
		if err != nil {
			return false, err
		}
		if _, err := q.ExecContext(ctx, `DELETE FROM durable_session_turn WHERE session_id = ?`, sessionID); err != nil {
			return false, fmt.Errorf("durable session abort: delete turns: %w", err)
		}
		return true, nil
	})
	if err != nil {
		return runtimesessions.AbortResult{}, err
	}
	return runtimesessions.AbortResult{Dropped: dropped}, nil
}

func heldInGrantOrder(ctx context.Context, q dbsqlite.Querier, sessionID string) ([]runtimesessions.QueuedTurn, error) {
	rows, err := q.QueryContext(ctx, `SELECT descriptor_json, sequence FROM durable_session_turn
WHERE session_id = ? ORDER BY CASE state WHEN 'active' THEN 0 ELSE 1 END, sequence`, sessionID)
	if err != nil {
		return nil, fmt.Errorf("durable session abort: read turns: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var held []runtimesessions.QueuedTurn
	for rows.Next() {
		var descriptorJSON string
		var sequence uint64
		if err := rows.Scan(&descriptorJSON, &sequence); err != nil {
			return nil, fmt.Errorf("durable session abort: read turn: %w", err)
		}
		desc, err := decodeDescriptor(descriptorJSON)
		if err != nil {
			return nil, fmt.Errorf("durable session abort: %w", err)
		}
		held = append(held, runtimesessions.QueuedTurn{Descriptor: desc, Sequence: sequence})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("durable session abort: read turns: %w", err)
	}
	return held, nil
}
