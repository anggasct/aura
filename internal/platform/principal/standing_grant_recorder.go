package principal

import (
	"context"
	"time"

	"github.com/anggasct/aura/internal/platform/fault"
)

const (
	AccessActionStandingGrant       = "standing_grant"
	AccessActionStandingGrantRevoke = "standing_grant_revoke"
	AccessActionAPIKeyCreate        = "apikey_create"
	AccessActionAPIKeyRevoke        = "apikey_revoke"
	AccessActionPrincipalGrant      = "principal_grant"
	AccessActionPrincipalRevoke     = "principal_grant_revoke"
	AccessActionPrincipalForget     = "principal_forget"
)

type OwnerAction struct {
	Action    string
	GrantID   string
	Principal string
	Persona   string
	Tool      string
	Account   string
	Plugin    string
	Version   string
	Channel   string
	ExpiresAt string
}

type OwnerActionRecorder interface {
	RecordOwnerAction(ctx context.Context, action *OwnerAction, subject string, at time.Time) error
	RecordOwnerRekey(ctx context.Context, report RekeyReport, at time.Time) error
}

type StandingGrantRequest struct {
	PrincipalID string
	AgentID     string
	ToolName    string
	OwnerID     string
	TTL         time.Duration
}

type StandingGrantRecorder struct {
	grants  StandingGrantStore
	actions OwnerActionRecorder
	now     func() time.Time
}

func NewStandingGrantRecorder(grants StandingGrantStore, actions OwnerActionRecorder) *StandingGrantRecorder {
	return &StandingGrantRecorder{
		grants:  grants,
		actions: actions,
		now:     time.Now,
	}
}

func (r *StandingGrantRecorder) RecordStandingGrant(ctx context.Context, request *StandingGrantRequest) error {
	if r == nil || r.grants == nil {
		return errNilArgument("standing grant store")
	}
	if request == nil {
		return fault.New(ErrorCodeInvalidArgument, "standing grant request must not be nil")
	}
	at := r.now().UTC()
	grant, err := r.grants.Grant(ctx, &StandingGrant{
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
	if r.actions == nil {
		return nil
	}
	return recordStandingGrantOwnerAction(ctx, r.actions, AccessActionStandingGrant, &grant, at)
}

func recordStandingGrantOwnerAction(ctx context.Context, actions OwnerActionRecorder, action string, grant *StandingGrant, at time.Time) error {
	if actions == nil {
		return nil
	}
	return actions.RecordOwnerAction(ctx, &OwnerAction{
		Action:    action,
		GrantID:   grant.ID,
		Principal: grant.PrincipalID,
		Persona:   grant.AgentID,
		Tool:      grant.ToolName,
		ExpiresAt: grant.ExpiresAt.UTC().Format(time.RFC3339Nano),
	}, grant.ID, at)
}
