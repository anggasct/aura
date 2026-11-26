package principal

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"
)

const (
	PairingOutcomeIssued  = "issued"
	PairingOutcomePending = "pending"
	PairingOutcomeLocked  = "locked"

	pairingCodeLength      = 8
	pairingCodeAlphabet    = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"
	pairingCodeTTL         = time.Hour
	pairingRequestCooldown = 10 * time.Minute
	pairingPendingCap      = 5
	pairingLockoutFailures = 5
	pairingLockoutWindow   = time.Hour
	pairingLockoutDuration = time.Hour
)

type PairingResult struct {
	Outcome string
	Code    string
}

type PairingObserver func(ctx context.Context, outcome string)

type PairingService struct {
	pairings  PairingStore
	grants    PrincipalGrantStore
	onRequest PairingObserver
	now       func() time.Time
	newCode   func() (string, error)
}

func NewPairingService(pairings PairingStore, grants PrincipalGrantStore, onRequest PairingObserver) *PairingService {
	if pairings == nil || grants == nil {
		return nil
	}
	return &PairingService{
		pairings:  pairings,
		grants:    grants,
		onRequest: onRequest,
		now:       time.Now,
		newCode:   NewPairingCode,
	}
}

func (p *PairingService) WithClock(now func() time.Time) {
	if p != nil && now != nil {
		p.now = now
	}
}

func (p *PairingService) Granted(ctx context.Context, principalID, account string) (bool, error) {
	return p.grants.Active(ctx, principalID, account)
}

func (p *PairingService) Request(ctx context.Context, principalID, account string) (PairingResult, error) {
	now := p.now().UTC()
	channel, _, _ := strings.Cut(principalID, ":")
	if _, locked, err := PairingLock(ctx, p.pairings, channel, now); err != nil {
		return PairingResult{}, err
	} else if locked {
		if p.onRequest != nil {
			p.onRequest(ctx, PairingOutcomeLocked)
		}
		return PairingResult{Outcome: PairingOutcomeLocked}, nil
	}
	code, err := p.newCode()
	if err != nil {
		return PairingResult{}, err
	}
	outcome, err := p.pairings.Issue(ctx, &PairingIssue{
		CodeSHA256:    PairingCodeDigest(code),
		PrincipalID:   principalID,
		Account:       account,
		Now:           now,
		TTL:           pairingCodeTTL,
		Cooldown:      pairingRequestCooldown,
		MaxPerAccount: pairingPendingCap,
	})
	if err != nil {
		return PairingResult{}, err
	}
	if outcome != PairingIssued {
		if p.onRequest != nil {
			p.onRequest(ctx, PairingOutcomePending)
		}
		return PairingResult{Outcome: PairingOutcomePending}, nil
	}
	if p.onRequest != nil {
		p.onRequest(ctx, PairingOutcomeIssued)
	}
	return PairingResult{Outcome: PairingOutcomeIssued, Code: code}, nil
}

func NewPairingCode() (string, error) {
	var raw [pairingCodeLength]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("mint pairing code: %w", err)
	}
	code := make([]byte, pairingCodeLength)
	for i, b := range raw {
		code[i] = pairingCodeAlphabet[int(b)%len(pairingCodeAlphabet)]
	}
	return string(code), nil
}

func NormalizePairingCode(raw string) string {
	var code strings.Builder
	for _, r := range strings.ToUpper(strings.TrimSpace(raw)) {
		switch r {
		case '-', ' ':
			continue
		case 'O':
			r = '0'
		case 'I', 'L':
			r = '1'
		}
		code.WriteRune(r)
	}
	return code.String()
}

func PairingCodeDigest(code string) string {
	sum := sha256.Sum256([]byte(NormalizePairingCode(code)))
	return hex.EncodeToString(sum[:])
}

func PairingLock(ctx context.Context, pairings PairingStore, channel string, now time.Time) (time.Time, bool, error) {
	failures, err := pairings.FailedApprovals(ctx, channel, now.Add(-pairingLockoutWindow-pairingLockoutDuration))
	if err != nil {
		return time.Time{}, false, err
	}
	until, locked := PairingLockedUntil(failures, now)
	return until, locked, nil
}

func PairingLockedUntil(failures []time.Time, now time.Time) (time.Time, bool) {
	var until time.Time
	for i := 0; i+pairingLockoutFailures-1 < len(failures); i++ {
		last := failures[i+pairingLockoutFailures-1]
		if last.Sub(failures[i]) <= pairingLockoutWindow && last.Add(pairingLockoutDuration).After(until) {
			until = last.Add(pairingLockoutDuration)
		}
	}
	return until, now.Before(until)
}

func PairingChannel(channel string) bool {
	return channel == ChannelDiscord || channel == ChannelTelegram
}
