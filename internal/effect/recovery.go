package effect

import (
	"context"
	"fmt"
)

// Claim atomically transitions a started intent to unknown and reports whether
// this caller won. The transition is a single conditional UPDATE
// (state = 'started'), so concurrent Claim or Recover calls cannot both win
// the same record: exactly one caller proceeds per intent.
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
	return affected == 1, nil
}

// RecoveryReport summarizes a startup recovery sweep.
type RecoveryReport struct {
	// Scanned is the number of started intents observed at the start of the
	// sweep. Another worker may have claimed some before this caller.
	Scanned int
	// Claimed is the number of started intents this caller transitioned to
	// unknown. The sum of Claimed across all concurrent workers equals the
	// number of started intents that existed; no intent is claimed twice.
	Claimed int
}

// Recover transitions every still-started intent to unknown at startup. Started
// is ambiguous after a crash: the provider may already have observed the
// request, and without provider reconciliation (a later step) the only safe
// resolution is unknown. Each record is claimed via the conditional Claim
// transition, so concurrent Recover calls partition the work without
// double-claiming.
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
