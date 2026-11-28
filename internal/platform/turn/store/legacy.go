package turnstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/anggasct/aura/internal/platform/sqlite"
	"github.com/anggasct/aura/internal/platform/turn"
)

type LegacySessionMark struct {
	SessionID  string
	AtSequence uint64
}

func MarkLegacySessions(ctx context.Context, db *sql.DB, toolMayTaint func(name string) bool) ([]LegacySessionMark, error) {
	if db == nil {
		return nil, errNilArgument("db")
	}
	sessions, err := listUnmarkedLegacySessions(ctx, db)
	if err != nil {
		return nil, err
	}
	var marked []LegacySessionMark
	for _, sess := range sessions {
		events, err := loadSessionEventsForLegacy(ctx, db, sess.id)
		if err != nil {
			return nil, err
		}
		firstSeq, found := turn.FirstLegacyUntrustedSequence(sess.ownerID, events, toolMayTaint)
		if !found {
			continue
		}
		inserted, err := insertLegacyTaintMark(ctx, db, sess.id, firstSeq)
		if err != nil {
			return nil, err
		}
		if inserted {
			marked = append(marked, LegacySessionMark{
				SessionID:  sess.id,
				AtSequence: firstSeq,
			})
		}
	}
	return marked, nil
}

func ListSessionEventsFrom(ctx context.Context, db *sql.DB, sessionID string, fromSeq uint64, kinds []string) ([]turn.Event, error) {
	if db == nil {
		return nil, errNilArgument("db")
	}
	if sessionID == "" || len(kinds) == 0 {
		return nil, nil
	}
	seqArg, err := sequenceToDB(fromSeq)
	if err != nil {
		return nil, err
	}
	encodedKinds, err := json.Marshal(kinds)
	if err != nil {
		return nil, fmt.Errorf("encode event kinds: %w", err)
	}
	rows, err := sqlite.Conn(ctx, db).QueryContext(ctx, `
		SELECT sequence, author, kind, payload_json
		FROM runtime_event
		WHERE session_id = ?
		  AND sequence >= ?
		  AND kind IN (SELECT value FROM json_each(?))
		ORDER BY sequence ASC`,
		sessionID, seqArg, string(encodedKinds),
	)
	if err != nil {
		return nil, classify(fmt.Errorf("list session events for %s: %w", sessionID, err))
	}
	defer func() { _ = rows.Close() }()
	var out []turn.Event
	for rows.Next() {
		var seq int64
		var author, kind, payload string
		if err := rows.Scan(&seq, &author, &kind, &payload); err != nil {
			return nil, classify(fmt.Errorf("scan session event: %w", err))
		}
		uSeq, err := sequenceFromDB(seq)
		if err != nil {
			return nil, err
		}
		out = append(out, turn.Event{
			SessionID: sessionID,
			Sequence:  uSeq,
			Author:    author,
			Kind:      kind,
			Payload:   json.RawMessage(payload),
		})
	}
	if err := rows.Err(); err != nil {
		return nil, classify(fmt.Errorf("iterate session events: %w", err))
	}
	return out, nil
}

type legacySessionRow struct {
	id      string
	ownerID string
}

func listUnmarkedLegacySessions(ctx context.Context, db *sql.DB) ([]legacySessionRow, error) {
	rows, err := sqlite.Conn(ctx, db).QueryContext(ctx, `
		SELECT s.id, s.owner_id
		FROM session s
		WHERE EXISTS (
			SELECT 1 FROM runtime_event e WHERE e.session_id = s.id
		)
		AND NOT EXISTS (
			SELECT 1 FROM runtime_event e WHERE e.session_id = s.id AND e.kind = ?
		)
		ORDER BY s.id ASC`,
		turn.EventKindSessionTainted,
	)
	if err != nil {
		return nil, classify(fmt.Errorf("list unmarked legacy sessions: %w", err))
	}
	defer func() { _ = rows.Close() }()
	var out []legacySessionRow
	for rows.Next() {
		var row legacySessionRow
		if err := rows.Scan(&row.id, &row.ownerID); err != nil {
			return nil, classify(fmt.Errorf("scan unmarked legacy session: %w", err))
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, classify(fmt.Errorf("iterate unmarked legacy sessions: %w", err))
	}
	return out, nil
}

func loadSessionEventsForLegacy(ctx context.Context, db *sql.DB, sessionID string) ([]turn.Event, error) {
	rows, err := sqlite.Conn(ctx, db).QueryContext(ctx, `
		SELECT sequence, author, kind, payload_json
		FROM runtime_event
		WHERE session_id = ?
		ORDER BY sequence ASC`,
		sessionID,
	)
	if err != nil {
		return nil, classify(fmt.Errorf("scan legacy session events for %s: %w", sessionID, err))
	}
	defer func() { _ = rows.Close() }()
	var events []turn.Event
	for rows.Next() {
		var seq int64
		var author, kind, payload string
		if err := rows.Scan(&seq, &author, &kind, &payload); err != nil {
			return nil, classify(fmt.Errorf("scan legacy event: %w", err))
		}
		uSeq, err := sequenceFromDB(seq)
		if err != nil {
			return nil, err
		}
		events = append(events, turn.Event{
			SessionID: sessionID,
			Sequence:  uSeq,
			Author:    author,
			Kind:      kind,
			Payload:   json.RawMessage(payload),
		})
	}
	if err := rows.Err(); err != nil {
		return nil, classify(fmt.Errorf("iterate legacy events: %w", err))
	}
	return events, nil
}

func insertLegacyTaintMark(ctx context.Context, db *sql.DB, sessionID string, atSequence uint64) (bool, error) {
	atSeq, err := sequenceToDB(atSequence)
	if err != nil {
		return false, err
	}
	payload, err := turn.EncodeTaintMark(turn.TaintMark{
		Source:     turn.TaintSourceLegacy,
		AtSequence: atSeq,
	})
	if err != nil {
		return false, err
	}
	eventID := "legacy-taint:" + sessionID
	inserted := false
	err = sqlite.RunInTx(ctx, db, func(ctx context.Context) error {
		q := sqlite.Conn(ctx, db)
		var exists bool
		if err := q.QueryRowContext(ctx,
			`SELECT EXISTS(SELECT 1 FROM runtime_event WHERE session_id = ? AND kind = ?)`,
			sessionID, turn.EventKindSessionTainted,
		).Scan(&exists); err != nil {
			return classify(fmt.Errorf("check existing taint mark: %w", err))
		}
		if exists {
			return nil
		}
		seq, err := assignNextSequence(ctx, q, sessionID)
		if err != nil {
			return err
		}
		ev := &turn.Event{
			ID:            eventID,
			SessionID:     sessionID,
			Sequence:      seq,
			Author:        "system",
			Kind:          turn.EventKindSessionTainted,
			SchemaVersion: 1,
			Payload:       payload,
			CreatedAt:     time.Now().UTC(),
		}
		if err := appendEvent(ctx, q, ev); err != nil {
			return err
		}
		inserted = true
		return nil
	})
	return inserted, err
}
