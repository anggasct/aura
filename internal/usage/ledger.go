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
	InvocationID             string
	Attempt                  int
	ModelDefinitionID        string
	KnownInputTokens         int64
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

func (l *Ledger) Reserve(ctx context.Context, req ReserveRequest) (*Reservation, error) {
	if err := validateReserveRequest(req); err != nil {
		return nil, err
	}

	tx, err := beginTx(ctx, l.db, "reserve")
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	if existing, ok, err := l.reservationByKey(ctx, tx, req.InvocationID, req.Attempt); err != nil {
		return nil, err
	} else if ok {
		return existing, nil
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
		price = &Price{Currency: l.currency, MaxReservationRate: 100}
	}

	reserved := price.ReserveCostMicros(req.KnownInputTokens, req.RequestedMaxOutputTokens)
	day, month := windowKeys(now)
	expiresAt := now.Add(l.reservationTTL)

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
			// A concurrent reservation for the same key won the insert; the
			// idempotent answer is that row, not a conflict. The in-tx
			// re-read can miss it under a deferred (read-committed)
			// snapshot, so fall back to the database outside the
			// transaction: the violation proves the winner committed, and a
			// fresh snapshot sees it. Note the read reuses the pool while
			// the transaction still holds a connection; a pool pinned to one
			// connection would deadlock, so a caller using MaxOpenConns(1)
			// must account for that.
			existing, rerr := reservationAfterConflict(
				func() (*Reservation, bool, error) { return l.reservationByKey(ctx, tx, invocationID, attempt) },
				func() (*Reservation, bool, error) { return l.reservationByKey(ctx, l.db, invocationID, attempt) },
			)
			if rerr != nil && !errors.Is(rerr, errReservationNotFound) {
				return nil, rerr
			}
			if existing != nil {
				return existing, nil
			}
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

func reservationAfterConflict(lookupInTx, lookupOutside func() (*Reservation, bool, error)) (*Reservation, error) {
	if existing, ok, err := lookupInTx(); err != nil {
		return nil, err
	} else if ok {
		return existing, nil
	}
	if existing, ok, err := lookupOutside(); err != nil {
		return nil, err
	} else if ok {
		return existing, nil
	}
	return nil, errReservationNotFound
}

func settlementAfterConflict(lookupInTx, lookupOutside func() (*Settlement, error)) (*Settlement, error) {
	if winner, err := lookupInTx(); err == nil && winner != nil {
		return winner, nil
	} else if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	if winner, err := lookupOutside(); err == nil && winner != nil {
		return winner, nil
	} else if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	return nil, errSettlementNotFound
}

func (l *Ledger) reservationByKey(ctx context.Context, q queryer, invocationID string, attempt int) (r *Reservation, ok bool, err error) {
	var (
		row       Reservation
		expiresAt string
		createdAt string
	)
	err = q.QueryRowContext(ctx, `
		SELECT id, invocation_id, attempt, model_definition_id, window_day, window_month,
			reserved_cost_micros, state, expires_at, created_at
		FROM usage_reservation WHERE invocation_id = ? AND attempt = ?`, invocationID, attempt).
		Scan(&row.ID, &row.InvocationID, &row.Attempt, &row.ModelDefinitionID, &row.WindowDay, &row.WindowMonth,
			&row.ReservedCostMicros, &row.State, &expiresAt, &createdAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("usage: read reservation by key: %w", err)
	}
	if row.ExpiresAt, err = time.Parse(time.RFC3339Nano, expiresAt); err != nil {
		return nil, false, fmt.Errorf("usage: parse reservation expires_at: %w", err)
	}
	if row.CreatedAt, err = time.Parse(time.RFC3339Nano, createdAt); err != nil {
		return nil, false, fmt.Errorf("usage: parse reservation created_at: %w", err)
	}
	return &row, true, nil
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

	tx, err := beginTx(ctx, l.db, "settle")
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

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
		accounting = accountingEstimated
		cost = reserved
	} else {
		cost = price.CostMicros(req.Usage)
		if cost <= 0 {
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
			winner, rerr := settlementAfterConflict(
				func() (*Settlement, error) {
					var w Settlement
					if err := tx.QueryRowContext(ctx, `
						SELECT id, reservation_id, cost_micros, accounting, price_version
						FROM usage_entry WHERE reservation_id = ?`, req.ReservationID).
						Scan(&w.ID, &w.ReservationID, &w.CostMicros, &w.Accounting, &w.PriceVersion); err != nil {
						return nil, err
					}
					return &w, nil
				},
				func() (*Settlement, error) {
					var w Settlement
					if err := l.db.QueryRowContext(ctx, `
						SELECT id, reservation_id, cost_micros, accounting, price_version
						FROM usage_entry WHERE reservation_id = ?`, req.ReservationID).
						Scan(&w.ID, &w.ReservationID, &w.CostMicros, &w.Accounting, &w.PriceVersion); err != nil {
						return nil, err
					}
					return &w, nil
				},
			)
			if rerr != nil && !errors.Is(rerr, errSettlementNotFound) {
				return nil, rerr
			}
			if winner != nil {
				return winner, nil
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

func (l *Ledger) capsEnabled() bool {
	return l.dailyCap > 0 || l.monthlyCap > 0
}

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
