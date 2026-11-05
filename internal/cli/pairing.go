package cli

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/anggasct/aura/internal/channel/discord"
	"github.com/anggasct/aura/internal/config"
	"github.com/anggasct/aura/internal/principal"
	"github.com/anggasct/aura/internal/store"
	"github.com/anggasct/aura/internal/telemetry"
)

const (
	pairingCodeLength      = 8
	pairingCodeAlphabet    = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"
	pairingCodeTTL         = time.Hour
	pairingRequestCooldown = 10 * time.Minute
	pairingPendingCap      = 5
	pairingLockoutFailures = 5
	pairingLockoutWindow   = time.Hour
	pairingLockoutDuration = time.Hour
)

const (
	accessActionPrincipalGrant  = "principal_grant"
	accessActionPrincipalRevoke = "principal_grant_revoke"
)

type pairingService struct {
	pairings store.PairingStore
	grants   store.PrincipalGrantStore
	recorder *telemetry.AccessRecorder
	now      func() time.Time
	newCode  func() (string, error)
}

func newPairingService(db *sql.DB, recorder *telemetry.AccessRecorder) *pairingService {
	return &pairingService{
		pairings: store.NewPairingStore(db),
		grants:   store.NewPrincipalGrantStore(db),
		recorder: recorder,
		now:      time.Now,
		newCode:  newPairingCode,
	}
}

func (p *pairingService) Granted(ctx context.Context, principalID, account string) (bool, error) {
	return p.grants.Active(ctx, principalID, account)
}

func (p *pairingService) Request(ctx context.Context, principalID, account string) (discord.PairingResult, error) {
	now := p.now().UTC()
	channel, _, _ := strings.Cut(principalID, ":")
	if _, locked, err := pairingLock(ctx, p.pairings, channel, now); err != nil {
		return discord.PairingResult{}, err
	} else if locked {
		p.recorder.PairingRequest(ctx, discord.PairingOutcomeLocked)
		return discord.PairingResult{Outcome: discord.PairingOutcomeLocked}, nil
	}
	code, err := p.newCode()
	if err != nil {
		return discord.PairingResult{}, err
	}
	outcome, err := p.pairings.Issue(ctx, &store.PairingIssue{
		CodeSHA256:    pairingCodeDigest(code),
		PrincipalID:   principalID,
		Account:       account,
		Now:           now,
		TTL:           pairingCodeTTL,
		Cooldown:      pairingRequestCooldown,
		MaxPerAccount: pairingPendingCap,
	})
	if err != nil {
		return discord.PairingResult{}, err
	}
	if outcome != store.PairingIssued {
		p.recorder.PairingRequest(ctx, discord.PairingOutcomePending)
		return discord.PairingResult{Outcome: discord.PairingOutcomePending}, nil
	}
	p.recorder.PairingRequest(ctx, discord.PairingOutcomeIssued)
	return discord.PairingResult{Outcome: discord.PairingOutcomeIssued, Code: code}, nil
}

func newPairingCode() (string, error) {
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

func normalizePairingCode(raw string) string {
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

func pairingCodeDigest(code string) string {
	sum := sha256.Sum256([]byte(normalizePairingCode(code)))
	return hex.EncodeToString(sum[:])
}

func pairingLock(ctx context.Context, pairings store.PairingStore, channel string, now time.Time) (time.Time, bool, error) {
	failures, err := pairings.FailedApprovals(ctx, channel, now.Add(-pairingLockoutWindow-pairingLockoutDuration))
	if err != nil {
		return time.Time{}, false, err
	}
	until, locked := pairingLockedUntil(failures, now)
	return until, locked, nil
}

func pairingLockedUntil(failures []time.Time, now time.Time) (time.Time, bool) {
	var until time.Time
	for i := 0; i+pairingLockoutFailures-1 < len(failures); i++ {
		last := failures[i+pairingLockoutFailures-1]
		if last.Sub(failures[i]) <= pairingLockoutWindow && last.Add(pairingLockoutDuration).After(until) {
			until = last.Add(pairingLockoutDuration)
		}
	}
	return until, now.Before(until)
}

func pairingChannel(channel string) bool {
	return channel == principal.ChannelDiscord || channel == principal.ChannelTelegram
}

func parsePrincipalArg(channel, raw string) (string, error) {
	if !pairingChannel(channel) {
		return "", &usageError{err: errors.New("channel must be discord or telegram")}
	}
	externalID := strings.TrimPrefix(raw, channel+":")
	if _, _, err := principal.ParseIdentity(channel + ":" + externalID); err != nil {
		return "", &usageError{err: fmt.Errorf("principal must be a %s user id", channel)}
	}
	return principal.Namespaced(channel, externalID), nil
}

func openPairingStores(cmd *cobra.Command, gf *globalFlags) (*sql.DB, store.PairingStore, store.PrincipalGrantStore, error) {
	result, err := config.Load(gf.configPath)
	if err != nil {
		return nil, nil, nil, err
	}
	db, err := openStorage(cmd.Context(), result.Config)
	if err != nil {
		return nil, nil, nil, err
	}
	return db, store.NewPairingStore(db), store.NewPrincipalGrantStore(db), nil
}

func newPairingCmd(gf *globalFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "pairing",
		Short: "Approve and manage DM pairing requests",
	}
	cmd.AddCommand(newPairingListCmd(gf), newPairingApproveCmd(gf), newPairingRevokeCmd(gf), newPairingClearCmd(gf))
	return cmd
}

func newPairingListCmd(gf *globalFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List pending pairing requests",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			db, pairings, _, err := openPairingStores(cmd, gf)
			if err != nil {
				return err
			}
			defer func() { _ = db.Close() }()
			pending, err := pairings.Pending(cmd.Context(), time.Now().UTC())
			if err != nil {
				return err
			}
			if len(pending) == 0 {
				return writeChildLines(cmd, []string{"no pending pairing requests"})
			}
			lines := []string{"ACCOUNT\tPRINCIPAL\tEXPIRES"}
			for i := range pending {
				lines = append(lines, fmt.Sprintf("%s\t%s\t%s", pending[i].Account, pending[i].PrincipalID, pending[i].ExpiresAt.UTC().Format(time.RFC3339)))
			}
			return writeChildLines(cmd, lines)
		},
	}
}

func newPairingApproveCmd(gf *globalFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "approve <channel> <code>",
		Short: "Grant trusted access to the sender of a pairing code",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			channel := args[0]
			if !pairingChannel(channel) {
				return &usageError{err: errors.New("channel must be discord or telegram")}
			}
			db, pairings, _, err := openPairingStores(cmd, gf)
			if err != nil {
				return err
			}
			defer func() { _ = db.Close() }()
			ctx := cmd.Context()
			now := time.Now().UTC()
			if until, locked, err := pairingLock(ctx, pairings, channel, now); err != nil {
				return err
			} else if locked {
				return &exitCodeError{code: 1, err: fmt.Errorf("pairing_locked: too many wrong codes; approvals for %s reopen at %s", channel, until.Format(time.RFC3339))}
			}
			request, found, err := pairings.ApproveChannel(ctx, channel, pairingCodeDigest(args[1]), principal.OwnerID, now)
			if err != nil {
				return err
			}
			if !found {
				if err := recordAccessAction(ctx, db, &accessAction{Action: store.AccessActionPairingInvalid, Channel: channel}, channel, now); err != nil {
					return err
				}
				return &exitCodeError{code: 1, err: errors.New("pairing_invalid: no pending pairing request has this code")}
			}
			if err := recordAccessAction(ctx, db, &accessAction{Action: accessActionPrincipalGrant, Principal: request.PrincipalID, Account: request.Account, Channel: channel}, request.PrincipalID, now); err != nil {
				return err
			}
			return writeChildLines(cmd, []string{"granted " + request.PrincipalID + " on " + request.Account})
		},
	}
}

func newPairingRevokeCmd(gf *globalFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "revoke <channel> <principal>",
		Short: "Revoke a principal's paired trusted access",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			principalID, err := parsePrincipalArg(args[0], args[1])
			if err != nil {
				return err
			}
			db, _, grants, err := openPairingStores(cmd, gf)
			if err != nil {
				return err
			}
			defer func() { _ = db.Close() }()
			now := time.Now().UTC()
			accounts, err := grants.Revoke(cmd.Context(), principalID, now)
			if err != nil {
				return err
			}
			if len(accounts) == 0 {
				return &exitCodeError{code: 1, err: errors.New("grant_not_found: principal has no paired access; allowlisted users are removed from the config")}
			}
			lines := make([]string, 0, len(accounts))
			for _, account := range accounts {
				if err := recordAccessAction(cmd.Context(), db, &accessAction{Action: accessActionPrincipalRevoke, Principal: principalID, Account: account, Channel: args[0]}, principalID+"-"+account, now); err != nil {
					return err
				}
				lines = append(lines, "revoked "+principalID+" on "+account)
			}
			return writeChildLines(cmd, lines)
		},
	}
}

func newPairingClearCmd(gf *globalFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "clear-pending",
		Short: "Drop every pending pairing request",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			db, pairings, _, err := openPairingStores(cmd, gf)
			if err != nil {
				return err
			}
			defer func() { _ = db.Close() }()
			cleared, err := pairings.ClearPending(cmd.Context())
			if err != nil {
				return err
			}
			return writeChildLines(cmd, []string{fmt.Sprintf("cleared %d pending pairing requests", cleared)})
		},
	}
}
