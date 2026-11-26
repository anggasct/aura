package turnstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"unicode/utf8"

	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/sqlite"
	"github.com/anggasct/aura/internal/platform/turn"
)

type TurnStore struct {
	db *sql.DB
}

func NewTurnStore(db *sql.DB) *TurnStore {
	return &TurnStore{db: db}
}

func (s *TurnStore) ListSessionEvents(ctx context.Context, sessionID string, filter turn.EventFilter) ([]turn.Event, error) {
	if filter.Limit < 0 {
		return nil, fault.New(ErrorCodeInvalidArgument, "limit must not be negative")
	}
	after, err := sequenceToDB(filter.AfterSequence)
	if err != nil {
		return nil, err
	}
	query := `SELECT ` + selectRuntimeEventColumns + ` FROM runtime_event WHERE session_id = ? AND sequence > ?`
	args := []any{sessionID, after}
	if filter.Kind != "" {
		query += ` AND kind = ?`
		args = append(args, filter.Kind)
	}
	if filter.IDPrefix != "" {
		query += ` AND substr(id, 1, ?) = ?`
		args = append(args, utf8.RuneCountInString(filter.IDPrefix), filter.IDPrefix)
	}
	query += ` ORDER BY sequence ASC`
	if filter.Limit > 0 {
		query += ` LIMIT ?`
		args = append(args, filter.Limit)
	}
	rows, err := sqlite.Conn(ctx, s.db).QueryContext(ctx, query, args...)
	if err != nil {
		return nil, classify(fmt.Errorf("list events for session %s: %w", sessionID, err))
	}
	defer func() { _ = rows.Close() }()

	var events []turn.Event
	for rows.Next() {
		e, err := scanRuntimeEvent(rows)
		if err != nil {
			return nil, fmt.Errorf("list events for session %s: %w", sessionID, err)
		}
		events = append(events, e)
	}
	if err := rows.Err(); err != nil {
		return nil, classify(fmt.Errorf("list events for session %s: %w", sessionID, err))
	}
	return events, nil
}

func (s *TurnStore) CreateSession(ctx context.Context, sess *turn.Session) error {
	return createSession(ctx, sqlite.Conn(ctx, s.db), sess)
}

func (s *TurnStore) AllocateSequence(ctx context.Context, sessionID string) (uint64, error) {
	if sessionID == "" {
		return 0, fault.New(ErrorCodeInvalidArgument, "session id must not be empty")
	}
	q := sqlite.Conn(ctx, s.db)
	if !rowExists(ctx, q, `SELECT 1 FROM session WHERE id = ?`, sessionID) {
		return 0, fault.New(ErrorCodeSessionNotFound, "session does not exist")
	}
	sequence, err := assignNextSequence(ctx, q, sessionID)
	if err != nil {
		return 0, classify(fmt.Errorf("allocate sequence: %w", err))
	}
	return sequence, nil
}

func (s *TurnStore) AppendEvent(ctx context.Context, e *turn.Event) error {
	if e == nil {
		return errNilArgument("event")
	}
	return appendEvent(ctx, sqlite.Conn(ctx, s.db), e)
}

func (s *TurnStore) ClaimDedupe(ctx context.Context, claim *turn.DedupeClaim) (originalTurnID string, created bool, err error) {
	return claimDedupe(ctx, sqlite.Conn(ctx, s.db), claim)
}

func (s *TurnStore) TurnUsed(ctx context.Context, turnID string) (bool, error) {
	var used bool
	if err := sqlite.Conn(ctx, s.db).QueryRowContext(ctx,
		`SELECT EXISTS (SELECT 1 FROM runtime_event WHERE turn_id = ?)`, turnID,
	).Scan(&used); err != nil {
		return false, classify(fmt.Errorf("read turn use %s: %w", turnID, err))
	}
	return used, nil
}

func (s *TurnStore) SessionOwner(ctx context.Context, sessionID string) (string, error) {
	var ownerID string
	if err := sqlite.Conn(ctx, s.db).QueryRowContext(ctx,
		`SELECT owner_id FROM session WHERE id = ?`, sessionID,
	).Scan(&ownerID); err != nil {
		return "", classify(fmt.Errorf("read session %s owner: %w", sessionID, err))
	}
	return ownerID, nil
}

func (s *TurnStore) SessionIDs(ctx context.Context, ownerID string, runSessions bool) ([]string, error) {
	query := `SELECT id FROM session WHERE owner_id = ? AND id NOT GLOB 'wf-*'`
	if runSessions {
		query = `SELECT id FROM session WHERE owner_id = ? AND id GLOB 'wf-*'`
	}
	rows, err := sqlite.Conn(ctx, s.db).QueryContext(ctx, query, ownerID)
	if err != nil {
		return nil, classify(fmt.Errorf("read session ids: %w", err))
	}
	defer func() { _ = rows.Close() }()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, classify(fmt.Errorf("scan session id: %w", err))
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, classify(fmt.Errorf("read session ids: %w", err))
	}
	return ids, nil
}

func (s *TurnStore) FilterSessions(ctx context.Context, ownerID string, ids []string) ([]string, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	encoded, err := json.Marshal(ids)
	if err != nil {
		return nil, fmt.Errorf("encode session ids: %w", err)
	}
	rows, err := sqlite.Conn(ctx, s.db).QueryContext(ctx,
		`SELECT id FROM session WHERE owner_id = ? AND id IN (SELECT value FROM json_each(?))`,
		ownerID, string(encoded),
	)
	if err != nil {
		return nil, classify(fmt.Errorf("filter sessions: %w", err))
	}
	defer func() { _ = rows.Close() }()
	var found []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, classify(fmt.Errorf("scan filtered session: %w", err))
		}
		found = append(found, id)
	}
	if err := rows.Err(); err != nil {
		return nil, classify(fmt.Errorf("filter sessions: %w", err))
	}
	return found, nil
}

func (s *TurnStore) DeleteSessionsByID(ctx context.Context, ownerID string, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	encoded, err := json.Marshal(ids)
	if err != nil {
		return fmt.Errorf("encode session ids: %w", err)
	}
	if _, err := sqlite.Conn(ctx, s.db).ExecContext(ctx,
		`DELETE FROM session WHERE owner_id = ? AND id IN (SELECT value FROM json_each(?))`,
		ownerID, string(encoded),
	); err != nil {
		return classify(fmt.Errorf("delete sessions: %w", err))
	}
	return nil
}

type acceptedTurnPair struct {
	SessionID string `json:"session_id"`
	TurnID    string `json:"turn_id"`
}

func (s *TurnStore) AcceptedTurns(ctx context.Context, turns []turn.SessionTurn) ([]turn.SessionTurn, error) {
	if len(turns) == 0 {
		return nil, nil
	}
	pairs := make([]acceptedTurnPair, len(turns))
	for i, t := range turns {
		pairs[i] = acceptedTurnPair{SessionID: t.SessionID, TurnID: t.TurnID}
	}
	encoded, err := json.Marshal(pairs)
	if err != nil {
		return nil, fmt.Errorf("encode accepted turn pairs: %w", err)
	}
	rows, err := sqlite.Conn(ctx, s.db).QueryContext(ctx, `
		SELECT DISTINCT runtime_event.session_id, runtime_event.turn_id
		FROM runtime_event
		JOIN json_each(?) AS wanted
		  ON runtime_event.session_id = json_extract(wanted.value, '$.session_id')
		 AND runtime_event.turn_id = json_extract(wanted.value, '$.turn_id')
		WHERE runtime_event.kind = ?`,
		string(encoded), turn.EventKindTurnAccepted,
	)
	if err != nil {
		return nil, classify(fmt.Errorf("read accepted turns: %w", err))
	}
	defer func() { _ = rows.Close() }()
	var accepted []turn.SessionTurn
	for rows.Next() {
		var pair turn.SessionTurn
		if err := rows.Scan(&pair.SessionID, &pair.TurnID); err != nil {
			return nil, classify(fmt.Errorf("scan accepted turn: %w", err))
		}
		accepted = append(accepted, pair)
	}
	if err := rows.Err(); err != nil {
		return nil, classify(fmt.Errorf("read accepted turns: %w", err))
	}
	return accepted, nil
}
