package discord

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	auraagent "github.com/anggasct/aura/internal/agent"
	"github.com/anggasct/aura/internal/channel/access"
	"github.com/anggasct/aura/internal/command"
	"github.com/anggasct/aura/internal/config"
	"github.com/anggasct/aura/internal/principal"
	runtimeingress "github.com/anggasct/aura/internal/runtime/ingress"
)

const (
	DeniedReasonAudience    = access.DeniedReasonAudience
	DeniedReasonEvalMissing = access.DeniedReasonEvalMissing

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

const (
	PairingOutcomeIssued  = access.PairingOutcomeIssued
	PairingOutcomePending = access.PairingOutcomePending
	PairingOutcomeLocked  = access.PairingOutcomeLocked
)

type (
	PairingResult  = access.PairingResult
	PairingService = access.PairingService
)

type AccessSettings struct {
	ApprovalTimeout  time.Duration
	StandingGrantTTL time.Duration
	Grants           StandingGrantRecorder
	Pairing          PairingService
	Budgets          BudgetGate
	Erasure          PrincipalEraser
	Handoffs         HandoffGate
	Evals            EvalGate
	Denied           func(ctx context.Context, reason string)
	ApprovalRouted   func(ctx context.Context, result string)
}

const (
	BudgetRefusalRateLimited = access.BudgetRefusalRateLimited
	BudgetRefusalExhausted   = access.BudgetRefusalExhausted
)

type (
	PrincipalEraser = access.PrincipalEraser
	EvalGate        = access.EvalGate
	BudgetGate      = access.BudgetGate
)

func (a *Adapter) SetAccess(settings *AccessSettings) {
	if a == nil || settings == nil {
		return
	}
	a.access = *settings
}

func (a *Adapter) servesPublic() bool {
	return a.router != nil && a.router.ServesPublic(config.BindingChannelDiscord, a.account.Name)
}

func (a *Adapter) authorAccess(ctx context.Context) authorAccess {
	reach := authorAccess{servesPublic: a.servesPublic()}
	if a.access.Pairing == nil {
		return reach
	}
	granted := map[string]bool{}
	reach.granted = func(authorID string) bool {
		if cached, ok := granted[authorID]; ok {
			return cached
		}
		active, err := a.access.Pairing.Granted(ctx, principal.Namespaced(principal.ChannelDiscord, authorID), a.account.Name)
		if err != nil {
			a.logger.WarnContext(ctx, "principal grant lookup failed", "component", "discord")
			active = false
		}
		granted[authorID] = active
		return active
	}
	return reach
}

func (a *Adapter) tryPairing(ctx context.Context, msg *messagePayload) bool {
	if a.access.Pairing == nil || msg.GuildID != "" || !a.account.AcceptDMs {
		return false
	}
	if msg.Author.Bot || msg.Author.ID == "" || msg.Author.ID == a.selfID || msg.Content == "" || (msg.Type != 0 && msg.Type != 19) {
		return false
	}
	if _, class, admitted := resolveAuthor(a.account, a.owners, msg.Author.ID, a.authorAccess(ctx)); admitted && class != principal.ClassPublic {
		return false
	}
	persona, routed := a.routePersona(ctx, &auraagent.RouteScope{ChannelID: msg.ChannelID, UserID: msg.Author.ID})
	if !routed {
		return false
	}
	definition, ok := a.personaDefinition(persona)
	if !ok || !definition.Pairing || definition.Audience != config.AudienceTrusted {
		return false
	}
	result, err := a.access.Pairing.Request(ctx, principal.Namespaced(principal.ChannelDiscord, msg.Author.ID), a.account.Name)
	if err != nil {
		a.logger.WarnContext(ctx, "pairing request failed", "component", "discord")
		return true
	}
	var reply string
	switch result.Outcome {
	case PairingOutcomeIssued:
		reply = strings.ReplaceAll(definition.Message(config.MessagePairingCode), config.PairingCodePlaceholder, result.Code)
	case PairingOutcomeLocked:
		reply = definition.Message(config.MessagePairingLocked)
	default:
		reply = definition.Message(config.MessagePairingPending)
	}
	token, err := a.tokenRef.Resolve()
	if err != nil {
		a.logger.WarnContext(ctx, "pairing reply token unavailable", "component", "discord")
		return true
	}
	a.postNotice(ctx, token, msg.ChannelID, reply)
	return true
}

func (a *Adapter) budgetDrop(ctx context.Context, msg *messagePayload, envelope *runtimeingress.IngressEnvelope) dropReason {
	class := principal.ParseClass(envelope.PrincipalClass)
	if a.access.Budgets == nil || class == principal.ClassOwner {
		return ""
	}
	refusal, err := a.access.Budgets.Admit(ctx, envelope.PrincipalID, class)
	if err != nil {
		a.logger.WarnContext(ctx, "principal budget check failed", "component", "discord")
		return dropBudgetCheckFailed
	}
	if refusal == "" {
		return ""
	}
	reason := dropBudgetExhausted
	if refusal == BudgetRefusalRateLimited {
		reason = dropRateLimited
	}
	definition, _ := a.personaDefinition(envelope.AgentID)
	token, err := a.tokenRef.Resolve()
	if err != nil {
		a.logger.WarnContext(ctx, "budget reply token unavailable", "component", "discord")
		return reason
	}
	a.postNotice(ctx, token, msg.ChannelID, definition.Message(refusal))
	return reason
}

func (a *Adapter) forgetMe(ctx context.Context, msg *messagePayload, inv *command.Invocation, envelope *runtimeingress.IngressEnvelope) {
	definition, _ := a.personaDefinition(envelope.AgentID)
	token, err := a.tokenRef.Resolve()
	if err != nil {
		a.logger.WarnContext(ctx, "forgetme reply token unavailable", "component", "discord")
		return
	}
	if principal.ParseClass(envelope.PrincipalClass) == principal.ClassOwner || a.access.Erasure == nil {
		a.postNotice(ctx, token, msg.ChannelID, definition.Message(config.MessageNotPermitted))
		return
	}
	if len(inv.Args) != 1 || !strings.EqualFold(inv.Args[0], command.ArgConfirm) {
		a.postNotice(ctx, token, msg.ChannelID, definition.Message(config.MessageForgetmeConfirm))
		return
	}
	if err := a.access.Erasure.Forget(ctx, envelope.PrincipalID); err != nil {
		a.logger.WarnContext(ctx, "forgetme erasure failed", "component", "discord")
		return
	}
	a.postNotice(ctx, token, msg.ChannelID, definition.Message(config.MessageForgetmeDone))
}

func (a *Adapter) channelConversation(persona, channelID, principalID string, class principal.Class) string {
	conversation := "channel:" + channelID
	if class == principal.ClassOwner {
		return conversation
	}
	if definition, ok := a.personaDefinition(persona); ok && definition.GroupContext == config.GroupContextShared {
		return conversation
	}
	return conversation + "/principal:" + principalID
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

func (a *Adapter) personaEvaluated(ctx context.Context, msg *messagePayload, persona string, class principal.Class) bool {
	if class == principal.ClassOwner || a.access.Evals == nil {
		return true
	}
	definition, ok := a.personaDefinition(persona)
	if ok && definition.Audience == config.AudienceOwner {
		return true
	}
	evaluated, err := a.access.Evals.Evaluated(ctx, persona)
	if err != nil {
		a.logger.WarnContext(ctx, "persona eval lookup failed", "component", "discord")
	}
	if err == nil && evaluated {
		return true
	}
	if a.access.Denied != nil {
		a.access.Denied(ctx, DeniedReasonEvalMissing)
	}
	if token, tokenErr := a.tokenRef.Resolve(); tokenErr == nil {
		a.postNotice(ctx, token, msg.ChannelID, definition.Message(config.MessageUnavailable))
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
	if err := sendText(ctx, a.httpClient, a.restBase, token, channelID, text); err != nil {
		a.logger.WarnContext(ctx, "access notice delivery failed", "component", "discord")
	}
}

func sendText(ctx context.Context, client *http.Client, base, token, channelID, text string) error {
	if channelID == "" || text == "" {
		return nil
	}
	payload, err := json.Marshal(map[string]any{
		"content":          neutralizeMentions(text),
		"allowed_mentions": map[string]any{"parse": []string{}},
	})
	if err != nil {
		return Errorf(ErrorCodeProtocolInvalid, "notice is not serializable")
	}
	call, cancel := context.WithTimeout(ctx, restTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(call, http.MethodPost, base+"/channels/"+channelID+"/messages", bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bot "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, approvalHTTPBytes))
	_ = resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return Errorf(ErrorCodeConnectionFailed, "notice delivery returned status %d", resp.StatusCode)
	}
	return nil
}
