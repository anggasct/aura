package usage

import (
	"context"
	"fmt"
	"time"
)

// Status is a point-in-time snapshot of budget consumption for the current
// UTC day and month. All amounts are integer USD micros.
type Status struct {
	Day                 string
	Month               string
	DailyCapMicros      int64
	MonthlyCapMicros    int64
	DayReservedMicros   int64
	DaySettledMicros    int64
	MonthReservedMicros int64
	MonthSettledMicros  int64
	ActiveReservations  int
}

// DayUsedMicros is the counted day spend (reserved + settled).
func (s *Status) DayUsedMicros() int64 { return s.DayReservedMicros + s.DaySettledMicros }

// MonthUsedMicros is the counted month spend (reserved + settled).
func (s *Status) MonthUsedMicros() int64 { return s.MonthReservedMicros + s.MonthSettledMicros }

// DayRemainingMicros is the remaining daily budget (cap - used). A zero cap
// means the daily budget is not enforced.
func (s *Status) DayRemainingMicros() int64 { return s.DailyCapMicros - s.DayUsedMicros() }

// MonthRemainingMicros is the remaining monthly budget (cap - used). A zero
// cap means the monthly budget is not enforced.
func (s *Status) MonthRemainingMicros() int64 { return s.MonthlyCapMicros - s.MonthUsedMicros() }

// Status reports current window consumption against the configured caps.
func (l *Ledger) Status(ctx context.Context) (*Status, error) {
	now := l.now()
	day, month := windowKeys(now)
	st := &Status{
		Day:              day,
		Month:            month,
		DailyCapMicros:   l.dailyCap,
		MonthlyCapMicros: l.monthlyCap,
	}

	if err := l.windowBreakdown(ctx, `r.window_day = ? AND r.window_month = ?`,
		[]any{day, month}, &st.DayReservedMicros, &st.DaySettledMicros); err != nil {
		return nil, err
	}
	if err := l.windowBreakdown(ctx, `r.window_month = ?`,
		[]any{month}, &st.MonthReservedMicros, &st.MonthSettledMicros); err != nil {
		return nil, err
	}

	if err := l.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM usage_reservation WHERE state = ?`, stateActive).
		Scan(&st.ActiveReservations); err != nil {
		return nil, fmt.Errorf("usage: count active reservations: %w", err)
	}
	return st, nil
}

// windowBreakdown splits counted spend for a window into reserved (active +
// expired reservations) and settled (entry costs).
func (l *Ledger) windowBreakdown(ctx context.Context, where string, args []any, reserved, settled *int64) error {
	query := `
		SELECT
			COALESCE(SUM(CASE WHEN r.state IN (?, ?) THEN r.reserved_cost_micros ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN e.id IS NOT NULL THEN e.cost_micros ELSE 0 END), 0)
		FROM usage_reservation r
		LEFT JOIN usage_entry e ON e.reservation_id = r.id
		WHERE ` + where + ` AND r.state IN (?, ?, ?)`
	full := append(append([]any{stateActive, stateExpired}, args...), stateActive, stateExpired, stateSettled)
	if err := l.db.QueryRowContext(ctx, query, full...).Scan(reserved, settled); err != nil {
		return fmt.Errorf("usage: read window breakdown: %w", err)
	}
	return nil
}

// Entry is one settled usage record joined with its reservation context.
type Entry struct {
	ID                string
	ReservationID     string
	ModelDefinitionID string
	InputTokens       int64
	OutputTokens      int64
	CostMicros        int64
	Accounting        string
	PriceVersion      string
	RecordedAt        time.Time
}

// Entries lists settlement entries newest first, capped at limit. A limit of
// 0 selects the default of 50; a negative limit is an invalid argument.
func (l *Ledger) Entries(ctx context.Context, limit int) ([]Entry, error) {
	if limit < 0 {
		return nil, codedError(ErrorCodeInvalidArgument, "usage: entries limit must not be negative", nil)
	}
	if limit == 0 {
		limit = 50
	}
	rows, err := l.db.QueryContext(ctx, `
		SELECT e.id, e.reservation_id, r.model_definition_id, e.input_tokens, e.output_tokens,
			e.cost_micros, e.accounting, e.price_version, e.recorded_at
		FROM usage_entry e
		JOIN usage_reservation r ON r.id = e.reservation_id
		ORDER BY e.recorded_at DESC, e.id DESC
		LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("usage: query entries: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var entries []Entry
	for rows.Next() {
		var en Entry
		var recordedAt string
		if err := rows.Scan(&en.ID, &en.ReservationID, &en.ModelDefinitionID, &en.InputTokens,
			&en.OutputTokens, &en.CostMicros, &en.Accounting, &en.PriceVersion, &recordedAt); err != nil {
			return nil, fmt.Errorf("usage: scan entry: %w", err)
		}
		en.RecordedAt, err = time.Parse(time.RFC3339Nano, recordedAt)
		if err != nil {
			return nil, fmt.Errorf("usage: parse entry recorded_at: %w", err)
		}
		entries = append(entries, en)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("usage: iterate entries: %w", err)
	}
	return entries, nil
}
