package effect

import (
	"context"
	"fmt"
)

func (j *Journal) Claim(ctx context.Context, id string) (bool, error) {
	if id == "" {
		return false, codedError(ErrorCodeInvalidArgument, "effect: id must not be empty", nil)
	}
	res, err := j.db.ExecContext(ctx, `
		UPDATE effect_intent
		SET state = ?, updated_at = ?
		WHERE id = ? AND state = ?`,
		string(StateUnknown), fmtTime(j.now()), id, string(StateStarted),
	)
	if err != nil {
		return false, fmt.Errorf("effect: claim intent %s: %w", id, err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("effect: read claim rows affected: %w", err)
	}
	if affected != 1 {
		return false, nil
	}
	intent, err := j.Get(ctx, id)
	if err != nil {
		return false, err
	}
	j.observe(ctx, intent, "recovered_unknown")
	return true, nil
}

type RecoveryReport struct {
	Scanned int
	Claimed int
}

func (j *Journal) Recover(ctx context.Context) (RecoveryReport, error) {
	started, err := j.ListByState(ctx, StateStarted, 0)
	if err != nil {
		return RecoveryReport{}, err
	}
	report := RecoveryReport{Scanned: len(started)}
	for i := range started {
		claimed, err := j.Claim(ctx, started[i].ID)
		if err != nil {
			return report, err
		}
		if claimed {
			report.Claimed++
		}
	}
	return report, nil
}
