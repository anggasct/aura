package discord

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	auraagent "github.com/anggasct/aura/internal/agent"
	"github.com/anggasct/aura/internal/config"
	"github.com/anggasct/aura/internal/principal"
)

const (
	DeniedReasonAudience = "audience"

	ApprovalRoutedApproved    = "approved"
	ApprovalRoutedStanding    = "standing"
	ApprovalRoutedRejected    = "rejected"
	ApprovalRoutedTimeout     = "timeout"
	ApprovalRoutedUnavailable = "unavailable"
)

type StandingGrantRecorder interface {
	RecordStandingGrant(ctx context.Context, grant *StandingGrantRequest) error
}

type StandingGrantRequest struct {
	PrincipalID string
	AgentID     string
	ToolName    string
	OwnerID     string
	TTL         time.Duration
}

type AccessSettings struct {
	ApprovalTimeout  time.Duration
	StandingGrantTTL time.Duration
	Grants           StandingGrantRecorder
	Denied           func(ctx context.Context, reason string)
	ApprovalRouted   func(ctx context.Context, result string)
}

func (a *Adapter) SetAccess(settings AccessSettings) {
	if a == nil {
		return
	}
	a.access = settings
}

func (a *Adapter) servesPublic() bool {
	return a.router != nil && a.router.ServesPublic(config.BindingChannelDiscord, a.account.Name)
}

func (a *Adapter) personaDefinition(persona string) (auraagent.Definition, bool) {
	if a.router == nil {
		return auraagent.Definition{}, false
	}
	return a.router.Persona(persona)
}

func (a *Adapter) personaAdmits(ctx context.Context, persona string, class principal.Class) bool {
	definition, ok := a.personaDefinition(persona)
	if ok && definition.Admits(class) {
		return true
	}
	if a.access.Denied != nil {
		a.access.Denied(ctx, DeniedReasonAudience)
	}
	return false
}

func (a *Adapter) observeRouted(ctx context.Context, result string) {
	if a.access.ApprovalRouted != nil {
		a.access.ApprovalRouted(ctx, result)
	}
}

func (a *Adapter) ownerApprovalTimeout(definition *auraagent.Definition) time.Duration {
	return cmp.Or(definition.ApprovalTimeout, a.access.ApprovalTimeout, approvalFallbackTTL)
}

func (a *Adapter) offersStandingGrant(class principal.Class) bool {
	return class == principal.ClassTrusted && a.access.Grants != nil && a.access.StandingGrantTTL > 0
}

func standingGrantLabel(ttl time.Duration) string {
	switch {
	case ttl == 24*time.Hour:
		return "Allow 1 day"
	case ttl%(24*time.Hour) == 0:
		return fmt.Sprintf("Allow %d days", ttl/(24*time.Hour))
	case ttl%time.Hour == 0:
		return fmt.Sprintf("Allow %d hours", ttl/time.Hour)
	default:
		return "Allow " + ttl.String()
	}
}

func personaLabel(definition *auraagent.Definition) string {
	if definition.Name == "" || definition.Name == definition.ID {
		return definition.ID
	}
	return definition.Name + " (" + definition.ID + ")"
}

func (a *Adapter) ownerDMChannel(ctx context.Context, token, ownerID string) (string, error) {
	a.ownerDMsMu.Lock()
	cached, ok := a.ownerDMs[ownerID]
	a.ownerDMsMu.Unlock()
	if ok {
		return cached, nil
	}
	payload, err := json.Marshal(map[string]string{"recipient_id": ownerID})
	if err != nil {
		return "", Errorf(ErrorCodeProtocolInvalid, "owner dm request is not serializable")
	}
	call, cancel := context.WithTimeout(ctx, restTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(call, http.MethodPost, a.restBase+"/users/@me/channels", bytes.NewReader(payload))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bot "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := a.httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, approvalHTTPBytes))
	if err != nil {
		return "", err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", Errorf(ErrorCodeApprovalUnavailable, "owner dm channel could not be opened")
	}
	var channel struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(body, &channel); err != nil || !isChannelID(channel.ID) {
		return "", Errorf(ErrorCodeApprovalUnavailable, "owner dm channel has no id")
	}
	a.ownerDMsMu.Lock()
	a.ownerDMs[ownerID] = channel.ID
	a.ownerDMsMu.Unlock()
	return channel.ID, nil
}

func (a *Adapter) postNotice(ctx context.Context, token, channelID, text string) {
	if channelID == "" || text == "" {
		return
	}
	payload, err := json.Marshal(map[string]any{
		"content":          neutralizeMentions(text),
		"allowed_mentions": map[string]any{"parse": []string{}},
	})
	if err != nil {
		return
	}
	call, cancel := context.WithTimeout(ctx, restTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(call, http.MethodPost, a.restBase+"/channels/"+channelID+"/messages", bytes.NewReader(payload))
	if err != nil {
		return
	}
	req.Header.Set("Authorization", "Bot "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := a.httpClient.Do(req)
	if err != nil {
		a.logger.WarnContext(ctx, "access notice delivery failed", "component", "discord")
		return
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, approvalHTTPBytes))
	_ = resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		a.logger.WarnContext(ctx, "access notice delivery failed", "component", "discord", "status", resp.StatusCode)
	}
}
