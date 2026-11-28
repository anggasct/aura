package turnstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/anggasct/aura/internal/platform/principal"
	"github.com/anggasct/aura/internal/platform/sqlite"
	"github.com/anggasct/aura/internal/platform/turn"
)

type TaintReader interface {
	SessionTaint(ctx context.Context, sessionID, turnID, principalID string) (turn.TaintMark, bool, error)
	SessionMarks(ctx context.Context, sessionIDs []string) (map[string]uint64, error)
	SessionTaintAt(ctx context.Context, sessionID string, atSequence uint64, principalID string) (turn.TaintMark, bool, error)
}

type OtherPrincipalMarkChecker interface {
	HasOtherPrincipalMark(ctx context.Context, sessionID, excludeTurnID, principalID string) (bool, error)
}

type sqliteTaintReader struct {
	db *sql.DB
}

func NewTaintReader(db *sql.DB) TaintReader {
	return &sqliteTaintReader{db: db}
}

func (r *sqliteTaintReader) SessionTaint(ctx context.Context, sessionID, turnID, principalID string) (turn.TaintMark, bool, error) {
	return querySessionTaint(ctx, sqlite.Conn(ctx, r.db), sessionID, turnID, principalID)
}

func (r *sqliteTaintReader) SessionMarks(ctx context.Context, sessionIDs []string) (map[string]uint64, error) {
	return querySessionMarks(ctx, sqlite.Conn(ctx, r.db), sessionIDs)
}

func (r *sqliteTaintReader) HasOtherPrincipalMark(ctx context.Context, sessionID, excludeTurnID, principalID string) (bool, error) {
	return queryHasOtherPrincipalMark(ctx, sqlite.Conn(ctx, r.db), sessionID, excludeTurnID, principalID)
}

func (r *sqliteTaintReader) SessionTaintAt(ctx context.Context, sessionID string, atSequence uint64, principalID string) (turn.TaintMark, bool, error) {
	return querySessionTaintAt(ctx, sqlite.Conn(ctx, r.db), sessionID, atSequence, principalID)
}

func (s *sqliteSessionService) SessionTaint(ctx context.Context, sessionID, turnID, principalID string) (turn.TaintMark, bool, error) {
	return querySessionTaint(ctx, sqlite.Conn(ctx, s.db), sessionID, turnID, principalID)
}

func (s *sqliteSessionService) SessionMarks(ctx context.Context, sessionIDs []string) (map[string]uint64, error) {
	return querySessionMarks(ctx, sqlite.Conn(ctx, s.db), sessionIDs)
}

func (s *sqliteSessionService) HasOtherPrincipalMark(ctx context.Context, sessionID, excludeTurnID, principalID string) (bool, error) {
	return queryHasOtherPrincipalMark(ctx, sqlite.Conn(ctx, s.db), sessionID, excludeTurnID, principalID)
}

func (s *sqliteSessionService) SessionTaintAt(ctx context.Context, sessionID string, atSequence uint64, principalID string) (turn.TaintMark, bool, error) {
	return querySessionTaintAt(ctx, sqlite.Conn(ctx, s.db), sessionID, atSequence, principalID)
}

func (s *TurnStore) SessionTaint(ctx context.Context, sessionID, turnID, principalID string) (turn.TaintMark, bool, error) {
	return querySessionTaint(ctx, sqlite.Conn(ctx, s.db), sessionID, turnID, principalID)
}

func (s *TurnStore) SessionMarks(ctx context.Context, sessionIDs []string) (map[string]uint64, error) {
	return querySessionMarks(ctx, sqlite.Conn(ctx, s.db), sessionIDs)
}

func (s *TurnStore) HasOtherPrincipalMark(ctx context.Context, sessionID, excludeTurnID, principalID string) (bool, error) {
	return queryHasOtherPrincipalMark(ctx, sqlite.Conn(ctx, s.db), sessionID, excludeTurnID, principalID)
}

func (s *TurnStore) SessionTaintAt(ctx context.Context, sessionID string, atSequence uint64, principalID string) (turn.TaintMark, bool, error) {
	return querySessionTaintAt(ctx, sqlite.Conn(ctx, s.db), sessionID, atSequence, principalID)
}

func effectiveMarkSequence(ownSeq uint64, mark turn.TaintMark) uint64 {
	if mark.AtSequence > 0 {
		return uint64(mark.AtSequence)
	}
	return ownSeq
}

func decodeMarkFailClosed(payload []byte) turn.TaintMark {
	mark, ok := turn.DecodeTaintMark(payload)
	if !ok {
		return turn.TaintMark{Source: turn.TaintSourceLegacy}
	}
	return mark
}

func querySessionTaint(ctx context.Context, q sqlite.Querier, sessionID, turnID, principalID string) (turn.TaintMark, bool, error) {
	if sessionID == "" {
		return turn.TaintMark{}, false, nil
	}
	rows, err := q.QueryContext(ctx,
		`SELECT sequence, turn_id, payload_json FROM runtime_event WHERE session_id = ? AND kind = ? ORDER BY sequence ASC`,
		sessionID, turn.EventKindSessionTainted,
	)
	if err != nil {
		return turn.TaintMark{}, false, classify(fmt.Errorf("read taint marks for session %s: %w", sessionID, err))
	}
	defer func() { _ = rows.Close() }()

	var best turn.TaintMark
	var bestEff uint64
	var bestOwn uint64
	found := false

	for rows.Next() {
		var seq int64
		var evTurnID, payload string
		if err := rows.Scan(&seq, &evTurnID, &payload); err != nil {
			return turn.TaintMark{}, false, classify(fmt.Errorf("scan taint mark for session %s: %w", sessionID, err))
		}
		if turnID != "" && evTurnID == turnID {
			continue
		}
		ownSeq, err := sequenceFromDB(seq)
		if err != nil {
			return turn.TaintMark{}, false, err
		}
		mark := decodeMarkFailClosed([]byte(payload))
		if mark.Source == turn.TaintSourceOtherPrincipal && principalID != "" && mark.Principal == principalID {
			continue
		}
		effSeq := effectiveMarkSequence(ownSeq, mark)
		if !found || effSeq < bestEff || (effSeq == bestEff && ownSeq < bestOwn) {
			best = mark
			bestEff = effSeq
			bestOwn = ownSeq
			found = true
		}
	}
	if err := rows.Err(); err != nil {
		return turn.TaintMark{}, false, classify(fmt.Errorf("read taint marks for session %s: %w", sessionID, err))
	}
	return best, found, nil
}

func querySessionMarks(ctx context.Context, q sqlite.Querier, sessionIDs []string) (map[string]uint64, error) {
	result := map[string]uint64{}
	if len(sessionIDs) == 0 {
		return result, nil
	}
	encoded, err := json.Marshal(sessionIDs)
	if err != nil {
		return nil, fmt.Errorf("encode session ids: %w", err)
	}
	rows, err := q.QueryContext(ctx,
		`SELECT session_id, sequence, payload_json
		 FROM runtime_event
		 WHERE session_id IN (SELECT value FROM json_each(?))
		   AND kind = ?
		 ORDER BY session_id ASC, sequence ASC`,
		string(encoded), turn.EventKindSessionTainted,
	)
	if err != nil {
		return nil, classify(fmt.Errorf("read session taint marks: %w", err))
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var sessionID, payload string
		var seq int64
		if err := rows.Scan(&sessionID, &seq, &payload); err != nil {
			return nil, classify(fmt.Errorf("scan session taint mark: %w", err))
		}
		ownSeq, err := sequenceFromDB(seq)
		if err != nil {
			return nil, err
		}
		mark := decodeMarkFailClosed([]byte(payload))
		if mark.Source == turn.TaintSourceOtherPrincipal && mark.Principal == principal.OwnerID {
			continue
		}
		effSeq := effectiveMarkSequence(ownSeq, mark)
		if existing, exists := result[sessionID]; !exists || effSeq < existing {
			result[sessionID] = effSeq
		}
	}
	if err := rows.Err(); err != nil {
		return nil, classify(fmt.Errorf("read session taint marks: %w", err))
	}
	return result, nil
}

func queryHasOtherPrincipalMark(ctx context.Context, q sqlite.Querier, sessionID, excludeTurnID, principalID string) (bool, error) {
	if sessionID == "" || principalID == "" {
		return false, nil
	}
	rows, err := q.QueryContext(ctx,
		`SELECT turn_id, payload_json FROM runtime_event WHERE session_id = ? AND kind = ? ORDER BY sequence ASC`,
		sessionID, turn.EventKindSessionTainted,
	)
	if err != nil {
		return false, classify(fmt.Errorf("check other principal mark for session %s: %w", sessionID, err))
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var evTurnID, payload string
		if err := rows.Scan(&evTurnID, &payload); err != nil {
			return false, classify(fmt.Errorf("scan other principal mark for session %s: %w", sessionID, err))
		}
		if excludeTurnID != "" && evTurnID == excludeTurnID {
			continue
		}
		mark, ok := turn.DecodeTaintMark([]byte(payload))
		if ok && mark.Source == turn.TaintSourceOtherPrincipal && mark.Principal == principalID {
			return true, nil
		}
	}
	if err := rows.Err(); err != nil {
		return false, classify(fmt.Errorf("check other principal mark for session %s: %w", sessionID, err))
	}
	return false, nil
}

func querySessionTaintAt(ctx context.Context, q sqlite.Querier, sessionID string, atSequence uint64, principalID string) (turn.TaintMark, bool, error) {
	if sessionID == "" || atSequence == 0 {
		return turn.TaintMark{}, false, nil
	}
	rows, err := q.QueryContext(ctx,
		`SELECT sequence, payload_json FROM runtime_event WHERE session_id = ? AND kind = ? ORDER BY sequence ASC`,
		sessionID, turn.EventKindSessionTainted,
	)
	if err != nil {
		return turn.TaintMark{}, false, classify(fmt.Errorf("read taint marks for session %s: %w", sessionID, err))
	}
	defer func() { _ = rows.Close() }()

	var best turn.TaintMark
	var bestEff uint64
	var bestOwn uint64
	found := false

	for rows.Next() {
		var seq int64
		var payload string
		if err := rows.Scan(&seq, &payload); err != nil {
			return turn.TaintMark{}, false, classify(fmt.Errorf("scan taint mark for session %s: %w", sessionID, err))
		}
		ownSeq, err := sequenceFromDB(seq)
		if err != nil {
			return turn.TaintMark{}, false, err
		}
		mark := decodeMarkFailClosed([]byte(payload))
		if mark.Source == turn.TaintSourceOtherPrincipal && principalID != "" && mark.Principal == principalID {
			continue
		}
		effSeq := effectiveMarkSequence(ownSeq, mark)
		if effSeq > atSequence {
			continue
		}
		if !found || effSeq < bestEff || (effSeq == bestEff && ownSeq < bestOwn) {
			best = mark
			bestEff = effSeq
			bestOwn = ownSeq
			found = true
		}
	}
	if err := rows.Err(); err != nil {
		return turn.TaintMark{}, false, classify(fmt.Errorf("read taint marks for session %s: %w", sessionID, err))
	}
	return best, found, nil
}
