package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/spf13/cobra"

	appconfig "github.com/anggasct/aura/internal/app/config"
	"github.com/anggasct/aura/internal/channels/kit/access"
	"github.com/anggasct/aura/internal/channels/kit/approval"
	personaeval "github.com/anggasct/aura/internal/modules/eval/persona"
	"github.com/anggasct/aura/internal/modules/handoff"
	auraagent "github.com/anggasct/aura/internal/platform/agent"
	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/principal"
	"github.com/anggasct/aura/internal/platform/telemetry"
	"github.com/anggasct/aura/internal/platform/turn"
	"github.com/anggasct/aura/internal/platform/turn/store"
	"github.com/anggasct/aura/internal/platform/usage"
)

const (
	accessActionStandingGrant       = "standing_grant"
	accessActionStandingGrantRevoke = "standing_grant_revoke"
)

type accessPolicy struct {
	registry *auraagent.Registry
	grants   principal.StandingGrantStore
	recorder *telemetry.AccessRecorder
	now      func() time.Time
}

func newAccessPolicy(registry *auraagent.Registry, db *sql.DB, recorder *telemetry.AccessRecorder) *accessPolicy {
	return &accessPolicy{registry: registry, grants: principal.NewStandingGrantStore(db), recorder: recorder, now: time.Now}
}

func (p *accessPolicy) ToolCeiling(agentID string, class principal.Class) ([]string, bool) {
	definition, ok := p.registry.Lookup(agentID)
	if !ok {
		return nil, false
	}
	ceiling, _ := definition.ToolCeiling(class)
	return ceiling, true
}

func (p *accessPolicy) StandingGrant(ctx context.Context, principalID, agentID, toolName, server string) (bool, error) {
	definition, ok := p.registry.Lookup(agentID)
	if !ok || !definition.AllowsPrincipalTool(toolName, server) {
		return false, nil
	}
	_, active, err := p.grants.Active(ctx, principalID, agentID, toolName, p.now().UTC())
	if err != nil {
		return false, err
	}
	if active {
		p.recorder.StandingGrantUsed(ctx)
	}
	return active, nil
}

type standingGrantRecorder struct {
	db     *sql.DB
	grants principal.StandingGrantStore
	now    func() time.Time
}

func newStandingGrantRecorder(db *sql.DB) *standingGrantRecorder {
	return &standingGrantRecorder{db: db, grants: principal.NewStandingGrantStore(db), now: time.Now}
}

func (r *standingGrantRecorder) RecordStandingGrant(ctx context.Context, request *approval.StandingGrantRequest) error {
	if request == nil {
		return errors.New("cli: standing grant request must not be nil")
	}
	at := r.now().UTC()
	grant, err := r.grants.Grant(ctx, &principal.StandingGrant{
		PrincipalID: request.PrincipalID,
		AgentID:     request.AgentID,
		ToolName:    request.ToolName,
		GrantedBy:   request.OwnerID,
		GrantedAt:   at,
		ExpiresAt:   at.Add(request.TTL),
	})
	if err != nil {
		return err
	}
	return recordAccessOwnerAction(ctx, r.db, accessActionStandingGrant, &grant, at)
}

type accessAction struct {
	Action    string `json:"action"`
	GrantID   string `json:"grant_id,omitempty"`
	Principal string `json:"principal_id,omitempty"`
	Persona   string `json:"agent_id,omitempty"`
	Tool      string `json:"tool_name,omitempty"`
	Account   string `json:"account,omitempty"`
	Plugin    string `json:"plugin_id,omitempty"`
	Version   string `json:"plugin_version,omitempty"`
	Channel   string `json:"channel,omitempty"`
	ExpiresAt string `json:"expires_at,omitempty"`
	At        string `json:"at"`
}

func recordAccessOwnerAction(ctx context.Context, db *sql.DB, action string, grant *principal.StandingGrant, at time.Time) error {
	return recordAccessAction(ctx, db, &accessAction{
		Action: action, GrantID: grant.ID, Principal: grant.PrincipalID, Persona: grant.AgentID, Tool: grant.ToolName,
		ExpiresAt: grant.ExpiresAt.UTC().Format(time.RFC3339Nano),
	}, grant.ID, at)
}

const profileOwnerSessionPrefix = "profile-"

func ensureOwnerActionSession(ctx context.Context, db *sql.DB, ownerID string) (string, error) {
	sessionID := profileOwnerSessionPrefix + ownerID
	sessions := turnstore.NewSessionService(db)
	if _, err := sessions.Get(ctx, sessionID); err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			return "", err
		}
		now := time.Now().UTC()
		if err := sessions.Create(ctx, &turn.Session{
			ID: sessionID, OwnerID: ownerID, CreatedAt: now, UpdatedAt: now, Metadata: json.RawMessage(`{}`),
		}); err != nil {
			if !fault.HasCode(err, turnstore.ErrorCodeSessionIDConflict) {
				return "", err
			}
		}
	}
	return sessionID, nil
}

func recordAccessAction(ctx context.Context, db *sql.DB, action *accessAction, subject string, at time.Time) error {
	sessionID, err := ensureOwnerActionSession(ctx, db, principal.OwnerID)
	if err != nil {
		return err
	}
	action.At = at.UTC().Format(time.RFC3339Nano)
	payload, err := json.Marshal(action)
	if err != nil {
		return fmt.Errorf("cli: marshal access owner action: %w", err)
	}
	_, _, err = turnstore.NewEventStore(db).UpsertEvent(ctx, &turn.Event{
		ID:            "access-owner-action-" + action.Action + "-" + subject + "-" + action.At,
		SessionID:     sessionID,
		TurnID:        "owner-action",
		InvocationID:  "owner-action",
		Author:        principal.OwnerID,
		Kind:          principal.AccessOwnerActionEventKind,
		SchemaVersion: 1,
		Payload:       payload,
		CreatedAt:     at.UTC(),
	})
	return err
}

func channelAccessSettings(cfg *appconfig.Config, db *sql.DB, recorder *telemetry.AccessRecorder, runs *erasureRuns) access.Settings {
	settings := access.Settings{
		Grants:         newStandingGrantRecorder(db),
		Denied:         recorder.Denied,
		ApprovalRouted: recorder.ApprovalRouted,
	}
	if db != nil {
		settings.Pairing = newPairingService(db, recorder)
	}
	if cfg.Access != nil {
		settings.ApprovalTimeout = time.Duration(cfg.Access.ApprovalTimeout)
		settings.StandingGrantTTL = time.Duration(cfg.Access.StandingGrantTTL)
	}
	if db != nil && cfg.Access != nil {
		if ledger, err := usage.NewLedger(db, usage.LedgerOptions{}); err == nil {
			settings.Budgets = newPrincipalBudgetGate(cfg.Access, ledger, recorder)
		}
	}
	if db != nil {
		_, artifactRoot, _, _ := storagePaths(cfg)
		settings.Erasure = &principalEraser{db: db, artifactRoot: artifactRoot, now: time.Now, runs: runs}
		if registry, err := buildAgentRegistry(cfg); err == nil {
			if evalStore, err := personaeval.NewSQLiteStore(db); err == nil {
				if gate, err := personaeval.NewGate(registry, evalStore); err == nil {
					settings.Evals = gate
				}
			}
		}
		if cfg.Access != nil {
			if handoffStore, err := handoff.NewSQLiteStore(db); err == nil {
				if gate, err := handoff.NewGate(handoffStore, time.Duration(cfg.Access.HandoffTimeout), time.Now); err == nil {
					settings.Handoffs = gate
				}
			}
		}
	}
	return settings
}

func openStandingGrants(cmd *cobra.Command, gf *globalFlags) (*sql.DB, principal.StandingGrantStore, error) {
	result, err := appconfig.Load(gf.configPath)
	if err != nil {
		return nil, nil, err
	}
	db, err := openStorage(cmd.Context(), result.Config)
	if err != nil {
		return nil, nil, err
	}
	return db, principal.NewStandingGrantStore(db), nil
}

func newGrantsCmd(gf *globalFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "grants",
		Short: "Inspect and revoke owner standing grants",
	}
	cmd.AddCommand(newGrantsListCmd(gf), newGrantsRevokeCmd(gf))
	return cmd
}

func newGrantsListCmd(gf *globalFlags) *cobra.Command {
	var principalID string
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List active standing grants",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			db, grants, err := openStandingGrants(cmd, gf)
			if err != nil {
				return err
			}
			defer func() { _ = db.Close() }()
			active, err := grants.List(cmd.Context(), principalID, time.Now().UTC())
			if err != nil {
				return err
			}
			if len(active) == 0 {
				return writeChildLines(cmd, []string{"no standing grants"})
			}
			lines := []string{"ID\tPRINCIPAL\tPERSONA\tTOOL\tEXPIRES"}
			for i := range active {
				grant := &active[i]
				lines = append(lines, fmt.Sprintf("%s\t%s\t%s\t%s\t%s", grant.ID, grant.PrincipalID, grant.AgentID, grant.ToolName, grant.ExpiresAt.UTC().Format(time.RFC3339)))
			}
			return writeChildLines(cmd, lines)
		},
	}
	cmd.Flags().StringVar(&principalID, "principal", "", "only grants for this principal id")
	return cmd
}

func newGrantsRevokeCmd(gf *globalFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "revoke <grant-id>",
		Short: "End a standing grant immediately",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			db, grants, err := openStandingGrants(cmd, gf)
			if err != nil {
				return err
			}
			defer func() { _ = db.Close() }()
			now := time.Now().UTC()
			active, err := grants.List(cmd.Context(), "", now)
			if err != nil {
				return err
			}
			index := slices.IndexFunc(active, func(grant principal.StandingGrant) bool { return grant.ID == args[0] })
			if index < 0 {
				return &exitCodeError{code: 1, err: errors.New("standing_grant_not_found: no active standing grant has this id")}
			}
			revoked, err := grants.Revoke(cmd.Context(), args[0], now)
			if err != nil {
				return err
			}
			if !revoked {
				return &exitCodeError{code: 1, err: errors.New("standing_grant_not_found: no active standing grant has this id")}
			}
			if err := recordAccessOwnerAction(cmd.Context(), db, accessActionStandingGrantRevoke, &active[index], now); err != nil {
				return err
			}
			return writeChildLines(cmd, []string{"revoked " + args[0]})
		},
	}
}
