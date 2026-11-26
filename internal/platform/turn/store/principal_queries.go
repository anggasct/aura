package turnstore

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/anggasct/aura/internal/platform/sqlite"
)

type PrincipalActivity struct {
	PrincipalID string
	Sessions    int
	LastActive  time.Time
}

func (s *TurnStore) AccessActionTimes(ctx context.Context, kind, action, channel string) ([]time.Time, error) {
	rows, err := sqlite.Conn(ctx, s.db).QueryContext(ctx, `
SELECT created_at FROM runtime_event
WHERE kind = ? AND json_extract(payload_json, '$.action') = ? AND json_extract(payload_json, '$.channel') = ?`,
		kind, action, channel)
	if err != nil {
		return nil, classify(fmt.Errorf("read failed pairing approvals: %w", err))
	}
	defer func() { _ = rows.Close() }()
	var times []time.Time
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, classify(fmt.Errorf("scan failed pairing approval: %w", err))
		}
		at, err := sqlite.ParseTime(raw)
		if err != nil {
			return nil, fmt.Errorf("parse failed pairing approval time: %w", err)
		}
		times = append(times, at)
	}
	if err := rows.Err(); err != nil {
		return nil, classify(fmt.Errorf("read failed pairing approvals: %w", err))
	}
	return times, nil
}

func (s *TurnStore) PrincipalActivity(ctx context.Context) ([]PrincipalActivity, error) {
	rows, err := sqlite.Conn(ctx, s.db).QueryContext(ctx, `SELECT owner_id, updated_at FROM session WHERE owner_id LIKE '%:%' AND id NOT GLOB 'wf-*'`)
	if err != nil {
		return nil, classify(fmt.Errorf("read principal sessions: %w", err))
	}
	defer func() { _ = rows.Close() }()
	byPrincipal := map[string]*PrincipalActivity{}
	for rows.Next() {
		var owner, updatedAt string
		if err := rows.Scan(&owner, &updatedAt); err != nil {
			return nil, classify(fmt.Errorf("scan principal session: %w", err))
		}
		at, err := sqlite.ParseTime(updatedAt)
		if err != nil {
			return nil, fmt.Errorf("parse principal session time: %w", err)
		}
		activity, ok := byPrincipal[owner]
		if !ok {
			activity = &PrincipalActivity{PrincipalID: owner}
			byPrincipal[owner] = activity
		}
		activity.Sessions++
		if at.After(activity.LastActive) {
			activity.LastActive = at
		}
	}
	if err := rows.Err(); err != nil {
		return nil, classify(fmt.Errorf("read principal sessions: %w", err))
	}
	activities := make([]PrincipalActivity, 0, len(byPrincipal))
	for _, activity := range byPrincipal {
		activities = append(activities, *activity)
	}
	slices.SortFunc(activities, func(a, b PrincipalActivity) int { return strings.Compare(a.PrincipalID, b.PrincipalID) })
	return activities, nil
}
