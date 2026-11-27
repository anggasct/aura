package effect

import (
	"context"
	"crypto/rand"
	"database/sql"
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

func newLeaseToken() string {
	return rand.Text()
}

func leaseLostError(id string) error {
	return codedError(ErrorCodeLeaseLost, fmt.Sprintf("effect: intent %s lease is held by another holder", id), nil)
}

func (j *Journal) renewLease(ctx context.Context, held *Intent) (time.Time, error) {
	expiresAt := j.clock.Now().Add(leaseDuration)
	res, err := sqlite.Conn(ctx, j.db).ExecContext(ctx, `
		UPDATE effect_intent
		SET lease_expires_at = ?
		WHERE id = ? AND state = ? AND lease_token IS ?`,
		fmtLeaseTime(expiresAt), held.ID, string(StateStarted), nullableString(held.LeaseToken),
	)
	if err != nil {
		return time.Time{}, fmt.Errorf("effect: renew lease of intent %s: %w", held.ID, err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return time.Time{}, fmt.Errorf("effect: read lease renewal rows affected: %w", err)
	}
	if affected != 1 {
		return time.Time{}, leaseLostError(held.ID)
	}
	return expiresAt, nil
}

func (j *Journal) takeOver(ctx context.Context, id string) (*Intent, error) {
	if id == "" {
		return nil, codedError(ErrorCodeInvalidArgument, "effect: id must not be empty", nil)
	}
	token := newLeaseToken()
	now := j.clock.Now()
	res, err := sqlite.Conn(ctx, j.db).ExecContext(ctx, `
		UPDATE effect_intent
		SET lease_token = ?, lease_expires_at = ?, updated_at = ?
		WHERE id = ? AND state = ? AND (lease_expires_at IS NULL OR lease_expires_at < ?)`,
		token, fmtLeaseTime(now.Add(leaseDuration)), fmtTime(now), id, string(StateStarted), fmtLeaseTime(now),
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
	if err != nil {
		return time.Time{}, false, fmt.Errorf("effect: read lease of intent %s: %w", id, err)
	}
	if state != StateStarted || !raw.Valid {
		return time.Time{}, false, nil
	}
	expiresAt, err := parseLeaseTime(raw.String)
	if err != nil {
		return time.Time{}, false, err
	}
	return expiresAt, true, nil
}

type leaseKeeper struct {
	journal        *Journal
	held           *Intent
	cancelProvider context.CancelFunc
	cancelOnLapse  bool
	expiresAt      time.Time
}

func (j *Journal) keepLease(ctx context.Context, held *Intent, cancelProvider context.CancelFunc, p Provider) (stop func()) {
	takeover, ok := p.(TakeoverProvider)
	keeper := &leaseKeeper{
		journal:        j,
		held:           held,
		cancelProvider: cancelProvider,
		cancelOnLapse:  ok && takeover.SupportsTakeover(),
		expiresAt:      j.clock.Now().Add(leaseDuration),
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
	}
}

func (k *leaseKeeper) run(ctx context.Context) {
	clock := k.journal.clock
	for {
		select {
		case <-ctx.Done():
			return
		case <-clock.Timer(leaseRenewEvery):
		}
		err := k.renew(ctx)
		switch {
		case err == nil, ctx.Err() != nil:
		case fault.HasCode(err, ErrorCodeLeaseLost):
			k.cancelProvider()
			return
		default:
			k.journal.logger.WarnContext(ctx, "effect lease renewal failed",
				"component", "effect", "effect_id", k.held.ID, "error", err)
			if k.cancelOnLapse && !clock.Now().Before(k.expiresAt) {
				k.cancelProvider()
				return
			}
		}
	}
}

func (k *leaseKeeper) renew(ctx context.Context) error {
	expiresAt, err := k.journal.renewLease(ctx, k.held)
	if err != nil {
		return err
	}
	k.expiresAt = expiresAt
	return nil
}
