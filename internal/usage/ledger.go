package usage

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"
)

const (
	stateActive     = "active"
	stateSettled    = "settled"
	stateExpired    = "expired"
	stateReconciled = "reconciled"

	accountingReported   = "reported"
	accountingEstimated  = "estimated"
	accountingReconciled = "reconciled"
)

// Ledger enforces UTC daily/monthly budget caps using durable reservations
// and exactly-once settlements. All money is integer USD micros.
type Ledger struct {
	db             *sql.DB
	prices         *PriceRegistry
	currency       string
	dailyCap       int64
	monthlyCap     int64
	reservationTTL time.Duration
	now            func() time.Time
	logger         *slog.Logger
}

type LedgerOptions struct {
	Prices           *PriceRegistry
	Currency         string
	DailyCapMicros   int64
	MonthlyCapMicros int64
	ReservationTTL   time.Duration
	Now              func() time.Time
	Logger           *slog.Logger
}

// NewLedger builds a ledger over an existing SQLite handle whose schema
// includes the usage tables (migration v2). A zero daily and monthly cap
// disables budget enforcement; reservations are still recorded.
func NewLedger(db *sql.DB, opts LedgerOptions) (*Ledger, error) {
	if db == nil {
		return nil, codedError(ErrorCodeInvalidArgument, "usage: database handle must not be nil", nil)
	}
	if opts.Prices == nil {
		opts.Prices = NewPriceRegistry()
	}
	if opts.Currency == "" {
		opts.Currency = "USD"
	}
	if opts.ReservationTTL <= 0 {
		opts.ReservationTTL = time.Hour
	}
	if opts.Now == nil {
		opts.Now = func() time.Time { return time.Now().UTC() }
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	return &Ledger{
		db:             db,
		prices:         opts.Prices,
		currency:       opts.Currency,
		dailyCap:       opts.DailyCapMicros,
		monthlyCap:     opts.MonthlyCapMicros,
		reservationTTL: opts.ReservationTTL,
		now:            opts.Now,
		logger:         opts.Logger,
	}, nil
}

type ReserveRequest struct {
	// InvocationID identifies the model invocation; together with Attempt it
	// uniquely names the reservation (retries and fallbacks are separate
	// attempts).
	InvocationID string
	Attempt      int
	// ModelDefinitionID must match a registered price record.
	ModelDefinitionID string
	// KnownInputTokens is the actual input size at reserve time.
	KnownInputTokens int64
	// RequestedMaxOutputTokens bounds the conservative reservation.
	RequestedMaxOutputTokens int64
}

type Reservation struct {
	ID                 string
	InvocationID       string
	Attempt            int
	ModelDefinitionID  string
	WindowDay          string
	WindowMonth        string
	ReservedCostMicros int64
	State              string
	ExpiresAt          time.Time
	CreatedAt          time.Time
}

// Reserve atomically checks the UTC daily and monthly windows against the
// caps and records a conservative reservation. An attempt with no applicable
// price is rejected when a cap is enabled; it never reserves at zero cost.
func (l *Ledger) Reserve(ctx context.Context, req ReserveRequest) (*Reservation, error) {
	if err := validateReserveRequest(req); err != nil {
		return nil, err
	}
	now := l.now()
	price, err := l.prices.At(req.ModelDefinitionID, l.currency, now)
	if err != nil {
		return nil, err
	}
	if price == nil {
		if l.capsEnabled() {
			return nil, codedError(ErrorCodePriceNotFound,
				fmt.Sprintf("usage: no price for model definition %q in currency %q", req.ModelDefinitionID, l.currency), nil)
		}
		// No cap enabled: record a zero-cost reservation marker without
		// enforcement; the price may simply not be registered yet.
		price = &Price{Currency: l.currency, MaxReservationRate: 100}
	}

	reserved := price.ReserveCostMicros(req.KnownInputTokens, req.RequestedMaxOutputTokens)
	day, month := windowKeys(now)
	expiresAt := now.Add(l.reservationTTL)

	tx, err := l.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("usage: begin reserve transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if l.capsEnabled() {
		dayUsed, monthUsed, err := l.windowUsed(ctx, tx, day, month, reserved)
		if err != nil {
			return nil, err
		}
		if l.dailyCap > 0 && dayUsed > l.dailyCap {
			return nil, codedError(ErrorCodeBudgetExceeded,
				fmt.Sprintf("usage: daily budget %d exceeds cap %d", dayUsed, l.dailyCap), nil)
		}
		if l.monthlyCap > 0 && monthUsed > l.monthlyCap {
			return nil, codedError(ErrorCodeBudgetExceeded,
				fmt.Sprintf("usage: monthly budget %d exceeds cap %d", monthUsed, l.monthlyCap), nil)
		}
	}

	id, err := randomID()
	if err != nil {
		return nil, err
	}
	invocationID := req.InvocationID
	attempt := req.Attempt
	_, err = tx.ExecContext(ctx, `
		INSERT INTO usage_reservation
			(id, invocation_id, attempt, model_definition_id, window_day, window_month,
			 reserved_cost_micros, state, expires_at, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, invocationID, attempt, req.ModelDefinitionID, day, month,
		reserved, stateActive, fmtTime(expiresAt), fmtTime(now), fmtTime(now),
	)
	if err != nil {
		if isUniqueViolation(err) {
			return nil, codedError(ErrorCodeReservationConflict,
				fmt.Sprintf("usage: reservation for invocation %q attempt %d already exists", invocationID, attempt), err)
		}
		return nil, fmt.Errorf("usage: insert reservation: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("usage: commit reservation: %w", err)
	}
	return &Reservation{
		ID:                 id,
		InvocationID:       invocationID,
		Attempt:            attempt,
		ModelDefinitionID:  req.ModelDefinitionID,
		WindowDay:          day,
		WindowMonth:        month,
		ReservedCostMicros: reserved,
		State:              stateActive,
		ExpiresAt:          expiresAt,
		CreatedAt:          now,
	}, nil
}

func validateReserveRequest(req ReserveRequest) error {
	if req.InvocationID == "" {
		return codedError(ErrorCodeInvalidArgument, "usage: invocation_id must not be empty", nil)
	}
	if req.Attempt < 0 {
		return codedError(ErrorCodeInvalidArgument, "usage: attempt must not be negative", nil)
	}
	if req.ModelDefinitionID == "" {
		return codedError(ErrorCodeInvalidArgument, "usage: model_definition_id must not be empty", nil)
	}
	if req.KnownInputTokens < 0 || req.RequestedMaxOutputTokens < 0 {
		return codedError(ErrorCodeInvalidArgument, "usage: token counts must not be negative", nil)
	}
	return nil
}

type SettleRequest struct {
	ReservationID   string
	ProviderUsageID string
	Usage           Usage
	UsageJSON       json.RawMessage
}

type Settlement struct {
	ID            string
	ReservationID string
	CostMicros    int64
	Accounting    string
	PriceVersion  string
}

// Settle closes a reservation exactly once. A duplicate settlement for the
// same reservation returns the existing entry (idempotent). A settlement for
// a reservation already associated with a different provider usage identity
// is a conflict. Missing provider usage settles conservatively as estimated
// at the reserved amount, never at zero.
func (l *Ledger) Settle(ctx context.Context, req *SettleRequest) (*Settlement, error) {
	if req == nil {
		return nil, codedError(ErrorCodeInvalidArgument, "usage: settle request must not be nil", nil)
	}
	if req.ReservationID == "" {
		return nil, codedError(ErrorCodeInvalidArgument, "usage: reservation_id must not be empty", nil)
	}
	if !req.Usage.valid() {
		return nil, codedError(ErrorCodeInvalidArgument, "usage: usage token counts must not be negative", nil)
	}
	now := l.now()

	tx, err := l.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("usage: begin settle transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// Idempotency: an existing entry for this reservation is the answer; a
	// different provider usage identity is a mismatch conflict.
	var existing Settlement
	var existingProviderUsage sql.NullString
	err = tx.QueryRowContext(ctx, `
		SELECT id, reservation_id, cost_micros, accounting, price_version, provider_usage_id
		FROM usage_entry WHERE reservation_id = ?`, req.ReservationID).
		Scan(&existing.ID, &existing.ReservationID, &existing.CostMicros, &existing.Accounting, &existing.PriceVersion, &existingProviderUsage)
	if err == nil {
		if req.ProviderUsageID != "" && existingProviderUsage.Valid && req.ProviderUsageID != existingProviderUsage.String {
			return nil, codedError(ErrorCodeUsageMismatch,
				fmt.Sprintf("usage: reservation %q settled with provider usage %q, got %q",
					req.ReservationID, existingProviderUsage.String, req.ProviderUsageID), nil)
		}
		return &existing, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("usage: read existing settlement: %w", err)
	}

	var (
		state             string
		modelDefinitionID string
		reserved          int64
	)
	err = tx.QueryRowContext(ctx, `
		SELECT state, model_definition_id, reserved_cost_micros
		FROM usage_reservation WHERE id = ?`, req.ReservationID).
		Scan(&state, &modelDefinitionID, &reserved)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, codedError(ErrorCodeReservationNotFound,
			fmt.Sprintf("usage: reservation %q not found", req.ReservationID), nil)
	}
	if err != nil {
		return nil, fmt.Errorf("usage: read reservation: %w", err)
	}
	if state == stateSettled || state == stateReconciled {
		return nil, codedError(ErrorCodeReservationConflict,
			fmt.Sprintf("usage: reservation %q is already %s", req.ReservationID, state), nil)
	}

	price, err := l.prices.At(modelDefinitionID, l.currency, now)
	if err != nil {
		return nil, err
	}

	accounting := accountingReported
	var cost int64
	if price == nil || price.Currency == "" {
		// Conservative: no price on settlement (may have been unregistered
		// since reserve) settles at the reserved amount, never at zero.
		accounting = accountingEstimated
		cost = reserved
	} else {
		cost = price.CostMicros(req.Usage)
		if cost <= 0 {
			// Missing or zero-reported usage: charge the reservation.
			accounting = accountingEstimated
			cost = reserved
		}
	}

	entryID, err := randomID()
	if err != nil {
		return nil, err
	}
	usageJSON := req.UsageJSON
	if len(usageJSON) == 0 {
		usageJSON = json.RawMessage("{}")
	}
	providerUsageID := req.ProviderUsageID
	_, err = tx.ExecContext(ctx, `
		INSERT INTO usage_entry
			(id, reservation_id, provider_usage_id, input_tokens, output_tokens,
			 usage_json, cost_micros, accounting, price_version, recorded_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		entryID, req.ReservationID, nullable(providerUsageID),
		req.Usage.InputTokens, req.Usage.OutputTokens,
		string(usageJSON), cost, accounting, priceVersion(price), fmtTime(now),
	)
	if err != nil {
		if isUniqueViolation(err) {
			// Concurrent duplicate settlement: read the winner.
			var winner Settlement
			if rerr := tx.QueryRowContext(ctx, `
				SELECT id, reservation_id, cost_micros, accounting, price_version
				FROM usage_entry WHERE reservation_id = ?`, req.ReservationID).
				Scan(&winner.ID, &winner.ReservationID, &winner.CostMicros, &winner.Accounting, &winner.PriceVersion); rerr == nil {
				return &winner, nil
			}
			return nil, codedError(ErrorCodeReservationConflict,
				fmt.Sprintf("usage: reservation %q already settled", req.ReservationID), err)
		}
		return nil, fmt.Errorf("usage: insert settlement: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE usage_reservation SET state = ?, updated_at = ? WHERE id = ?`,
		stateSettled, fmtTime(now), req.ReservationID); err != nil {
		return nil, fmt.Errorf("usage: settle reservation: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("usage: commit settlement: %w", err)
	}
	return &Settlement{
		ID:            entryID,
		ReservationID: req.ReservationID,
		CostMicros:    cost,
		Accounting:    accounting,
		PriceVersion:  priceVersion(price),
	}, nil
}

// ExpireStale marks reservations past their TTL as expired. Expired
// reservations remain counted toward the caps until reconciled, so a crash or
// cancellation never silently frees budget.
func (l *Ledger) ExpireStale(ctx context.Context) (int, error) {
	res, err := l.db.ExecContext(ctx, `
		UPDATE usage_reservation SET state = ?, updated_at = ?
		WHERE state = ? AND expires_at <= ?`,
		stateExpired, fmtTime(l.now()), stateActive, fmtTime(l.now()))
	if err != nil {
		return 0, fmt.Errorf("usage: expire stale reservations: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("usage: read expired count: %w", err)
	}
	return int(n), nil
}

// Reconcile releases expired reservations: it converts them to reconciled so
// they stop counting toward the caps. Settlement after reconcile is a
// conflict.
func (l *Ledger) Reconcile(ctx context.Context) (int, error) {
	res, err := l.db.ExecContext(ctx, `
		UPDATE usage_reservation SET state = ?, updated_at = ?
		WHERE state = ?`,
		stateReconciled, fmtTime(l.now()), stateExpired)
	if err != nil {
		return 0, fmt.Errorf("usage: reconcile reservations: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("usage: read reconciled count: %w", err)
	}
	return int(n), nil
}

type WindowUsage struct {
	Day        string
	Month      string
	UsedMicros int64
}

// WindowUsage reports the counted spend for a UTC day/month: active and
// expired reservations count their reserved amount, settled reservations
// count their entry cost, reconciled reservations count nothing.
func (l *Ledger) WindowUsage(ctx context.Context, day, month string) (int64, error) {
	var used int64
	err := l.db.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(CASE WHEN e.id IS NOT NULL THEN e.cost_micros ELSE r.reserved_cost_micros END), 0)
		FROM usage_reservation r
		LEFT JOIN usage_entry e ON e.reservation_id = r.id
		WHERE r.window_day = ? AND r.window_month = ? AND r.state IN (?, ?, ?)`,
		day, month, stateActive, stateExpired, stateSettled).Scan(&used)
	if err != nil {
		return 0, fmt.Errorf("usage: read window usage: %w", err)
	}
	return used, nil
}

// capsEnabled reports whether budget enforcement is on.
func (l *Ledger) capsEnabled() bool {
	return l.dailyCap > 0 || l.monthlyCap > 0
}

// windowUsed sums counted spend for the day window and the whole month
// window. Active and expired reservations count at reserved cost, settled
// reservations at entry cost; the pending amount is added to both totals.
func (l *Ledger) windowUsed(ctx context.Context, q queryer, day, month string, pending int64) (dayUsed, monthUsed int64, err error) {
	dayUsed, err = l.windowSum(ctx, q, `r.window_day = ? AND r.window_month = ?`, day, month)
	if err != nil {
		return 0, 0, err
	}
	monthUsed, err = l.windowSum(ctx, q, `r.window_month = ?`, month)
	if err != nil {
		return 0, 0, err
	}
	return dayUsed + pending, monthUsed + pending, nil
}

func (l *Ledger) windowSum(ctx context.Context, q queryer, where string, args ...any) (int64, error) {
	var used int64
	query := `
		SELECT COALESCE(SUM(CASE WHEN e.id IS NOT NULL THEN e.cost_micros ELSE r.reserved_cost_micros END), 0)
		FROM usage_reservation r
		LEFT JOIN usage_entry e ON e.reservation_id = r.id
		WHERE ` + where + ` AND r.state IN (?, ?, ?)`
	args = append(args, stateActive, stateExpired, stateSettled)
	if err := q.QueryRowContext(ctx, query, args...).Scan(&used); err != nil {
		return 0, fmt.Errorf("usage: read window usage: %w", err)
	}
	return used, nil
}

type queryer interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

func windowKeys(now time.Time) (day, month string) {
	return now.Format("2006-01-02"), now.Format("2006-01")
}

func fmtTime(t time.Time) string {
	return t.UTC().Format(time.RFC3339Nano)
}

func priceVersion(p *Price) string {
	if p == nil {
		return ""
	}
	return p.EffectiveFrom.UTC().Format("2006-01-02")
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func randomID() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("usage: generate id: %w", err)
	}
	return hex.EncodeToString(buf), nil
}
