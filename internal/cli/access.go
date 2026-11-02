package cli

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/spf13/cobra"

	auraagent "github.com/anggasct/aura/internal/agent"
	"github.com/anggasct/aura/internal/channel/discord"
	"github.com/anggasct/aura/internal/config"
	"github.com/anggasct/aura/internal/principal"
	"github.com/anggasct/aura/internal/store"
	"github.com/anggasct/aura/internal/telemetry"
	"github.com/anggasct/aura/internal/usage"
)

const (
	accessActionStandingGrant       = "standing_grant"
	accessActionStandingGrantRevoke = "standing_grant_revoke"
)

type accessPolicy struct {
	registry *auraagent.Registry
	grants   store.StandingGrantStore
	recorder *telemetry.AccessRecorder
	now      func() time.Time
}

func newAccessPolicy(registry *auraagent.Registry, db *sql.DB, recorder *telemetry.AccessRecorder) *accessPolicy {
	return &accessPolicy{registry: registry, grants: store.NewStandingGrantStore(db), recorder: recorder, now: time.Now}
}

func (p *accessPolicy) ToolCeiling(agentID string, class principal.Class) ([]string, bool) {
	definition, ok := p.registry.Lookup(agentID)
	if !ok {
		return nil, false
	}
	ceiling, _ := definition.ToolCeiling(class)
	return ceiling, true
}

func (p *accessPolicy) StandingGrant(ctx context.Context, principalID, agentID, toolName string) (bool, error) {
	definition, ok := p.registry.Lookup(agentID)
	if !ok || !slices.Contains(definition.PrincipalTools, toolName) {
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
	grants store.StandingGrantStore
	now    func() time.Time
}

func newStandingGrantRecorder(db *sql.DB) *standingGrantRecorder {
	return &standingGrantRecorder{db: db, grants: store.NewStandingGrantStore(db), now: time.Now}
}

func (r *standingGrantRecorder) RecordStandingGrant(ctx context.Context, request *discord.StandingGrantRequest) error {
	if request == nil {
		return errors.New("cli: standing grant request must not be nil")
	}
	at := r.now().UTC()
	grant, err := r.grants.Grant(ctx, &store.StandingGrant{
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
	Channel   string `json:"channel,omitempty"`
	ExpiresAt string `json:"expires_at,omitempty"`
	At        string `json:"at"`
}

func recordAccessOwnerAction(ctx context.Context, db *sql.DB, action string, grant *store.StandingGrant, at time.Time) error {
	return recordAccessAction(ctx, db, &accessAction{
		Action: action, GrantID: grant.ID, Principal: grant.PrincipalID, Persona: grant.AgentID, Tool: grant.ToolName,
		ExpiresAt: grant.ExpiresAt.UTC().Format(time.RFC3339Nano),
	}, grant.ID, at)
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
	_, _, err = store.NewEventStore(db).UpsertEvent(ctx, &store.RuntimeEvent{
		ID:            "access-owner-action-" + action.Action + "-" + subject + "-" + action.At,
		SessionID:     sessionID,
		TurnID:        "owner-action",
		InvocationID:  "owner-action",
		Author:        principal.OwnerID,
		Kind:          store.AccessOwnerActionEventKind,
		SchemaVersion: 1,
		Payload:       payload,
		CreatedAt:     at.UTC(),
	})
	return err
}

func discordAccessSettings(cfg *config.Config, db *sql.DB, recorder *telemetry.AccessRecorder) discord.AccessSettings {
	settings := discord.AccessSettings{
		Grants:         newStandingGrantRecorder(db),
		Pairing:        newPairingService(db, recorder),
		Denied:         recorder.Denied,
		ApprovalRouted: recorder.ApprovalRouted,
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
	return settings
}

func openStandingGrants(cmd *cobra.Command, gf *globalFlags) (*sql.DB, store.StandingGrantStore, error) {
	result, err := config.Load(gf.configPath)
	if err != nil {
		return nil, nil, err
	}
	db, err := openStorage(cmd.Context(), result.Config)
	if err != nil {
		return nil, nil, err
	}
	return db, store.NewStandingGrantStore(db), nil
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
			index := slices.IndexFunc(active, func(grant store.StandingGrant) bool { return grant.ID == args[0] })
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
