package effect

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/sqlite"
)

const (
	leaseDuration   = 60 * time.Second
	leaseRenewEvery = 20 * time.Second
	leaseTimeLayout = "2006-01-02T15:04:05.000000000Z"
)

type TakeoverProvider interface {
	Provider
	SupportsTakeover() bool
}

var errLeaseRevoked = errors.New("effect: lease revoked")

func newLeaseToken() string {
	return rand.Text()
}

func leaseLostError(id string) error {
	return codedError(ErrorCodeLeaseLost, fmt.Sprintf("effect: intent %s lease is held by another holder", id), nil)
}

func (j *Journal) renewLease(ctx context.Context, held *Intent) (time.Time, error) {
	if held == nil || held.LeaseToken == "" {
		return time.Time{}, codedError(ErrorCodeInvalidArgument, "effect: a lease renewal needs the held intent and its token", nil)
	}
	expiresAt := j.clock.Now().Add(leaseDuration)
	res, err := sqlite.Conn(ctx, j.db).ExecContext(ctx, `
		UPDATE effect_intent
		SET lease_expires_at = ?
		WHERE id = ? AND state = ? AND lease_token IS ?`,
		FormatLeaseTime(expiresAt), held.ID, string(StateStarted), nullableString(held.LeaseToken),
	)
	if err != nil {
		return time.Time{}, fmt.Errorf("effect: renew lease of intent %s: %w", held.ID, err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return time.Time{}, fmt.Errorf("effect: read lease renewal rows affected: %w", err)
	}
	if affected == 1 {
		return expiresAt, nil
	}
	return time.Time{}, j.renewalRefusal(ctx, held)
}

func (j *Journal) renewalRefusal(ctx context.Context, held *Intent) error {
	var storedToken sql.NullString
	err := sqlite.Conn(ctx, j.db).QueryRowContext(ctx,
		`SELECT lease_token FROM effect_intent WHERE id = ?`, held.ID).Scan(&storedToken)
	if errors.Is(err, sql.ErrNoRows) {
		return codedError(ErrorCodeNotFound, fmt.Sprintf("effect: intent %s not found", held.ID), nil)
	}
	if err != nil {
		return fmt.Errorf("effect: read lease of intent %s: %w", held.ID, err)
	}
	if storedToken.String != held.LeaseToken {
		return leaseLostError(held.ID)
	}
	return codedError(ErrorCodeTransitionInvalid, fmt.Sprintf("effect: intent %s is no longer started", held.ID), nil)
}

func (j *Journal) takeOver(ctx context.Context, id string) (*Intent, error) {
	if id == "" {
		return nil, codedError(ErrorCodeInvalidArgument, "effect: id must not be empty", nil)
	}
	token := newLeaseToken()
	now := j.clock.Now()
	expiresAt := now.Add(leaseDuration)
	res, err := sqlite.Conn(ctx, j.db).ExecContext(ctx, `
		UPDATE effect_intent
		SET lease_token = ?, lease_expires_at = ?, updated_at = ?
		WHERE id = ? AND state = ? AND (lease_expires_at IS NULL OR lease_expires_at < ?)`,
		token, FormatLeaseTime(expiresAt), fmtTime(now), id, string(StateStarted), FormatLeaseTime(now),
	)
	if err != nil {
		return nil, fmt.Errorf("effect: take over intent %s: %w", id, err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return nil, fmt.Errorf("effect: read takeover rows affected: %w", err)
	}
	current, err := j.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if affected == 1 {
		current.LeaseToken = token
		current.leaseExpiresAt = expiresAt
		j.observe(ctx, current, "lease_taken_over")
		return current, nil
	}
	if current.State == StateStarted {
		return nil, codedError(ErrorCodeInProgress, fmt.Sprintf("effect: intent %s is held under a live lease", id), nil)
	}
	return nil, codedError(ErrorCodeTransitionInvalid,
		fmt.Sprintf("effect: intent %s is %s, only started intents resume", id, current.State), nil)
}

func (j *Journal) liveLeaseExpiry(ctx context.Context, id string) (time.Time, bool, error) {
	var raw sql.NullString
	var state State
	err := sqlite.Conn(ctx, j.db).QueryRowContext(ctx,
		`SELECT state, lease_expires_at FROM effect_intent WHERE id = ?`, id).Scan(&state, &raw)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, false, nil
	}
	if err != nil {
		return time.Time{}, false, fmt.Errorf("effect: read lease of intent %s: %w", id, err)
	}
	if state != StateStarted || !raw.Valid {
		return time.Time{}, false, nil
	}
	expiresAt, err := ParseLeaseTime(raw.String)
	if err != nil {
		return time.Time{}, false, err
	}
	return expiresAt, true, nil
}

type leaseKeeper struct {
	journal       *Journal
	held          *Intent
	revoke        context.CancelCauseFunc
	cancelOnLapse bool
	expiresAt     time.Time
}

func (j *Journal) keepLease(ctx context.Context, held *Intent, revoke context.CancelCauseFunc, p Provider) (stop func(), err error) {
	if held == nil || held.LeaseToken == "" || revoke == nil {
		return nil, codedError(ErrorCodeInvalidArgument, "effect: a lease keeper needs the held intent, its token and a revoke function", nil)
	}
	takeover, ok := p.(TakeoverProvider)
	keeper := &leaseKeeper{
		journal:       j,
		held:          held,
		revoke:        revoke,
		cancelOnLapse: ok && takeover.SupportsTakeover(),
		expiresAt:     held.leaseExpiresAt,
	}
	keeperCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		keeper.run(keeperCtx)
	}()
	return func() {
		cancel()
		<-done
	}, nil
}

func (k *leaseKeeper) run(ctx context.Context) {
	clock := k.journal.clock
	for {
		tick := clock.Timer(leaseRenewEvery)
		var lapse <-chan time.Time
		if k.cancelOnLapse {
			lapse = clock.Timer(k.expiresAt.Sub(clock.Now()))
		}
		select {
		case <-ctx.Done():
			return
		case <-tick:
		case <-lapse:
			select {
			case <-tick:
			default:
				k.revoke(errLeaseRevoked)
				return
			}
		}
		if !k.renewOnce(ctx) {
			return
		}
	}
}

func (k *leaseKeeper) renewOnce(ctx context.Context) (keepRenewing bool) {
	err := k.renew(ctx)
	switch {
	case err == nil:
		return true
	case ctx.Err() != nil:
		return false
	case fault.HasCode(err, ErrorCodeLeaseLost):
		k.logStopped(ctx, "lease_lost")
		k.revoke(errLeaseRevoked)
		return false
	case fault.HasCode(err, ErrorCodeTransitionInvalid), fault.HasCode(err, ErrorCodeNotFound):
		k.logStopped(ctx, "not_started")
		return false
	}
	k.journal.logger.WarnContext(ctx, "effect lease renewal failed",
		"component", "effect", "effect_id", k.held.ID, "error", err)
	if k.cancelOnLapse && !k.journal.clock.Now().Before(k.expiresAt) {
		k.revoke(errLeaseRevoked)
		return false
	}
	return true
}

func (k *leaseKeeper) logStopped(ctx context.Context, status string) {
	k.journal.logger.WarnContext(ctx, "effect lease renewal stopped",
		"component", "effect", "effect_id", k.held.ID, "status", status)
}

func (k *leaseKeeper) renew(ctx context.Context) error {
	expiresAt, err := k.journal.renewLease(ctx, k.held)
	if err != nil {
		return err
	}
	k.expiresAt = expiresAt
	return nil
}
