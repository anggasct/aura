package effect

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/anggasct/aura/internal/platform/sqlite"
)

const DefaultTerminalRetention = 30 * 24 * time.Hour

type RetentionReport struct {
	Deleted         int
	PreservedAudits int
}

func (j *Journal) Prune(ctx context.Context, before time.Time) (RetentionReport, error) {
	if before.IsZero() {
		return RetentionReport{}, codedError(ErrorCodeInvalidArgument, "effect: retention cutoff must not be zero", nil)
	}
	var report RetentionReport
	err := sqlite.RunInTx(ctx, j.db, func(ctx context.Context) error {
		q := sqlite.Conn(ctx, j.db)
		var preserved int64
		if err := q.QueryRowContext(ctx, `
			SELECT COUNT(*)
			FROM effect_intent i
			WHERE i.state IN (?, ?)
			  AND i.finished_at IS NOT NULL
			  AND i.finished_at < ?
			  AND EXISTS (SELECT 1 FROM effect_approval a WHERE a.intent_id = i.id)`,
			string(StateSucceeded), string(StateFailed), fmtTime(before.UTC()),
		).Scan(&preserved); err != nil {
			return fmt.Errorf("effect: count retained audits: %w", err)
		}
		result, err := q.ExecContext(ctx, `
			DELETE FROM effect_intent
			WHERE state IN (?, ?)
			  AND finished_at IS NOT NULL
			  AND finished_at < ?
			  AND NOT EXISTS (SELECT 1 FROM effect_approval a WHERE a.intent_id = effect_intent.id)`,
			string(StateSucceeded), string(StateFailed), fmtTime(before.UTC()),
		)
		if err != nil {
			return fmt.Errorf("effect: prune terminal intents: %w", err)
		}
		deleted, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf("effect: read pruned intents: %w", err)
		}
		if deleted > math.MaxInt || preserved > math.MaxInt {
			return errors.New("effect: retention report exceeds host integer range")
		}
		report = RetentionReport{Deleted: int(deleted), PreservedAudits: int(preserved)}
		return nil
	})
	if err != nil {
		return RetentionReport{}, err
	}
	return report, nil
}

type Status struct {
	Counts        map[State]int
	OldestByState map[State]time.Time
}

func (j *Journal) Status(ctx context.Context) (Status, error) {
	rows, err := sqlite.Conn(ctx, j.db).QueryContext(ctx, `
		SELECT state, COUNT(*), MIN(updated_at)
		FROM effect_intent
		GROUP BY state`)
	if err != nil {
		return Status{}, fmt.Errorf("effect: query status: %w", err)
	}
	defer func() { _ = rows.Close() }()

	status := Status{Counts: map[State]int{}, OldestByState: map[State]time.Time{}}
	for rows.Next() {
		var rawState, oldest string
		var count int64
		if err := rows.Scan(&rawState, &count, &oldest); err != nil {
			return Status{}, fmt.Errorf("effect: scan status: %w", err)
		}
		if count < 0 || count > math.MaxInt {
			return Status{}, fmt.Errorf("effect: status count %d is out of range", count)
		}
		state := State(rawState)
		if !state.valid() {
			return Status{}, fmt.Errorf("effect: status contains invalid state %q", rawState)
		}
		parsed, err := parseTime(oldest, "status oldest updated_at")
		if err != nil {
			return Status{}, err
		}
		status.Counts[state] = int(count)
		status.OldestByState[state] = parsed
	}
	if err := rows.Err(); err != nil {
		return Status{}, fmt.Errorf("effect: read status rows: %w", err)
	}
	return status, nil
}

func (s Status) Count(state State) int {
	return s.Counts[state]
}
