package discord

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/anggasct/aura/internal/channels/kit/approval"
	"github.com/anggasct/aura/internal/config"
	"github.com/anggasct/aura/internal/platform/principal"
)

const approvalHTTPBytes = 1 << 16

type ApprovalPrompt = approval.Prompt

type approvalBinding struct {
	principal string
	class     principal.Class
	persona   string
	actor     string
	guildID   string
	channelID string
	standing  bool
}

func (b *approvalBinding) shared() *approval.Binding {
	return &approval.Binding{
		Principal: b.principal,
		Class:     b.class,
		Persona:   b.persona,
		Actor:     b.actor,
		Scope:     [2]string{b.guildID, b.channelID},
		Standing:  b.standing,
	}
}

type cardRef struct {
	channel string
	message string
}

func (a *Adapter) rememberTarget(conversationID string, binding *approvalBinding) {
	a.targetsMu.Lock()
	defer a.targetsMu.Unlock()
	if len(a.targets) >= maxTrackedDeliveries {
		for drop := range a.targets {
			delete(a.targets, drop)
			break
		}
	}
	a.targets[conversationID] = *binding
}

func (a *Adapter) HoldsApprovalTarget(sessionID string) bool {
	_, ok := a.lookupTarget(sessionID)
	return ok
}

func (a *Adapter) lookupTarget(conversationID string) (approvalBinding, bool) {
	a.targetsMu.Lock()
	defer a.targetsMu.Unlock()
	binding, ok := a.targets[conversationID]
	return binding, ok
}

func validateApprovalPrompt(ctx context.Context, prompt *ApprovalPrompt) error {
	if ctx == nil {
		return Errorf(ErrorCodeInvalidArgument, "context must not be nil")
	}
	if prompt == nil {
		return Errorf(ErrorCodeInvalidArgument, "approval prompt must not be nil")
	}
	if strings.TrimSpace(prompt.ToolName) == "" || strings.TrimSpace(prompt.SessionID) == "" || strings.TrimSpace(prompt.PrincipalID) == "" {
		return Errorf(ErrorCodeInvalidArgument, "approval prompt is missing tool, session, or principal")
	}
	return nil
}

func (a *Adapter) Decide(ctx context.Context, prompt *ApprovalPrompt) (bool, error) {
	if err := validateApprovalPrompt(ctx, prompt); err != nil {
		return false, err
	}
	binding, ok := a.lookupTarget(prompt.SessionID)
	if !ok || binding.principal != prompt.PrincipalID {
		return false, nil
	}
	if binding.principal != principal.OwnerID {
		return a.decideForOwner(ctx, prompt, &binding)
	}
	card, ok := a.openChannelCard(ctx, prompt, &binding)
	if !ok {
		return false, ctx.Err()
	}
	outcome, _, err := a.awaitApproval(ctx, card.token, card.tokenID, card.pending, card.ref)
	return outcome.Accepted, err
}

func (a *Adapter) DecideForOwner(ctx context.Context, prompt *ApprovalPrompt, requester *approval.Requester) (bool, error) {
	if err := validateApprovalPrompt(ctx, prompt); err != nil {
		return false, err
	}
	if requester == nil || requester.Principal == "" || requester.Principal != prompt.PrincipalID {
		return false, Errorf(ErrorCodeInvalidArgument, "approval requester is missing or does not match the prompt")
	}
	return a.decideForOwner(ctx, prompt, &approvalBinding{principal: requester.Principal, class: requester.Class, persona: requester.Persona})
}

func (a *Adapter) ServesPersona(persona string) bool {
	return a.router != nil && a.router.ServesPersona(config.BindingChannelDiscord, a.account.Name, persona)
}

type openedCard struct {
	token   string
	tokenID string
	pending *approval.Pending
	ref     cardRef
}

func (a *Adapter) openChannelCard(ctx context.Context, prompt *ApprovalPrompt, binding *approvalBinding) (openedCard, bool) {
	expiresAt := prompt.ExpiresAt
	if expiresAt.IsZero() {
		expiresAt = time.Now().Add(approval.FallbackTTL)
	}
	if !time.Now().Before(expiresAt) {
		return openedCard{}, false
	}
	token, err := a.tokenRef.Resolve()
	if err != nil {
		a.logger.WarnContext(ctx, "approval token unavailable", "component", "discord")
		return openedCard{}, false
	}
	tokenID, pending, err := a.approvals.Issue(binding.shared(), expiresAt)
	if err != nil {
		a.logger.WarnContext(ctx, "approval token mint failed", "component", "discord")
		return openedCard{}, false
	}
	cardID, err := a.postApprovalCard(ctx, token, binding.channelID, renderApprovalCard(prompt, ""), tokenID, pending)
	if err != nil {
		a.approvals.Drop(tokenID, pending)
		a.logger.WarnContext(ctx, "approval card delivery failed", "component", "discord")
		return openedCard{}, false
	}
	return openedCard{token: token, tokenID: tokenID, pending: pending, ref: cardRef{channel: binding.channelID, message: cardID}}, true
}

func (a *Adapter) decideForOwner(ctx context.Context, prompt *ApprovalPrompt, requester *approvalBinding) (bool, error) {
	definition, _ := a.personaDefinition(requester.persona)
	if definition.ID == "" {
		definition.ID = requester.persona
	}
	owners := a.owners.OwnerExternalIDs(principal.ChannelDiscord)
	if len(owners) == 0 {
		a.observeRouted(ctx, approval.RoutedUnavailable)
		return false, Errorf(ErrorCodeApprovalUnavailable, "owner has no discord identity")
	}
	token, err := a.tokenRef.Resolve()
	if err != nil {
		a.observeRouted(ctx, approval.RoutedUnavailable)
		return false, Errorf(ErrorCodeApprovalUnavailable, "approval token is unavailable")
	}
	ownerID := owners[0]
	dmChannel, err := a.ownerDMChannel(ctx, token, ownerID)
	if err != nil {
		a.observeRouted(ctx, approval.RoutedUnavailable)
		return false, Errorf(ErrorCodeApprovalUnavailable, "owner dm channel is unavailable")
	}
	now := time.Now()
	expiresAt := now.Add(approval.OwnerTimeout(definition.ApprovalTimeout, a.access.ApprovalTimeout))
	if !prompt.ExpiresAt.IsZero() && prompt.ExpiresAt.Before(expiresAt) {
		expiresAt = prompt.ExpiresAt
	}
	if !now.Before(expiresAt) {
		return false, nil
	}
	binding := approvalBinding{
		principal: requester.principal,
		class:     requester.class,
		persona:   requester.persona,
		actor:     ownerID,
		channelID: dmChannel,
		standing:  a.offersStandingGrant(prompt, requester.class),
	}
	tokenID, pending, err := a.approvals.Issue(binding.shared(), expiresAt)
	if err != nil {
		a.observeRouted(ctx, approval.RoutedUnavailable)
		return false, Errorf(ErrorCodeApprovalUnavailable, "approval token mint failed")
	}
	cardID, err := a.postApprovalCard(ctx, token, dmChannel, renderApprovalCard(prompt, personaLabel(&definition)), tokenID, pending)
	if err != nil {
		a.approvals.Drop(tokenID, pending)
		a.observeRouted(ctx, approval.RoutedUnavailable)
		return false, Errorf(ErrorCodeApprovalUnavailable, "owner approval card delivery failed")
	}
	a.postNotice(ctx, token, requester.channelID, definition.Message(config.MessageWaitingForOwner))
	outcome, timedOut, err := a.awaitApproval(ctx, token, tokenID, pending, cardRef{channel: dmChannel, message: cardID})
	notify := context.WithoutCancel(ctx)
	switch {
	case timedOut || errors.Is(err, context.DeadlineExceeded):
		a.observeRouted(ctx, approval.RoutedTimeout)
		a.postNotice(notify, token, requester.channelID, definition.Message(config.MessageApprovalTimeout))
		if err != nil {
			return false, err
		}
		return false, Errorf(ErrorCodeApprovalTimeout, "owner did not answer the approval in time")
	case err != nil:
		return false, err
	case outcome.Standing:
		a.observeRouted(ctx, approval.RoutedStanding)
		if recordErr := a.access.Grants.RecordStandingGrant(notify, &StandingGrantRequest{
			PrincipalID: requester.principal,
			AgentID:     requester.persona,
			ToolName:    prompt.ToolName,
			OwnerID:     principal.Namespaced(principal.ChannelDiscord, ownerID),
			TTL:         a.access.StandingGrantTTL,
		}); recordErr != nil {
			a.logger.WarnContext(ctx, "standing grant record failed", "component", "discord")
		}
		return true, nil
	case outcome.Accepted:
		a.observeRouted(ctx, approval.RoutedApproved)
		return true, nil
	default:
		a.observeRouted(ctx, approval.RoutedRejected)
		a.postNotice(notify, token, requester.channelID, definition.Message(config.MessageApprovalDenied))
		return false, nil
	}
}

func (a *Adapter) awaitApproval(ctx context.Context, token, tokenID string, pending *approval.Pending, card cardRef) (approval.Outcome, bool, error) {
	defer func() {
		a.disableApprovalCard(context.WithoutCancel(ctx), token, card.channel, card.message)
	}()
	return a.approvals.Await(ctx, tokenID, pending)
}

func renderApprovalCard(prompt *ApprovalPrompt, persona string) string {
	return approval.RenderCard(prompt, persona, neutralizeMentions)
}

func (a *Adapter) approvalButtons(tokenID string, pending *approval.Pending) []componentPayload {
	binding := pending.Binding()
	callbacks := a.approvals.Callbacks(tokenID, &binding, pending.ExpiresAt())
	buttons := []buttonPayload{{Type: componentButton, Style: buttonStyleDanger, Label: "Approve", CustomID: callbacks.Approve}}
	if callbacks.Standing != "" {
		buttons = append(buttons, buttonPayload{Type: componentButton, Style: buttonStyleDanger, Label: approval.StandingGrantLabel(a.access.StandingGrantTTL), CustomID: callbacks.Standing})
	}
	buttons = append(buttons, buttonPayload{Type: componentButton, Style: buttonStylePrimary, Label: "Reject", CustomID: callbacks.Reject})
	return []componentPayload{{Type: componentActionRow, Components: buttons}}
}

func (a *Adapter) postApprovalCard(ctx context.Context, token, channelID, content, tokenID string, pending *approval.Pending) (string, error) {
	payload, err := json.Marshal(map[string]any{
		"content":          content,
		"components":       a.approvalButtons(tokenID, pending),
		"allowed_mentions": map[string]any{"parse": []string{}},
	})
	if err != nil {
		return "", Errorf(ErrorCodeProtocolInvalid, "approval card is not serializable")
	}
	endpoint := a.restBase + "/channels/" + channelID + "/messages"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
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
		return "", Errorf(ErrorCodeDeliveryFailed, "approval card post did not succeed")
	}
	var message struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(body, &message); err != nil || message.ID == "" {
		return "", Errorf(ErrorCodeDeliveryAmbiguous, "approval card post has no message id")
	}
	return message.ID, nil
}

func (a *Adapter) disableApprovalCard(ctx context.Context, token, channelID, messageID string) {
	if channelID == "" || messageID == "" {
		return
	}
	payload, err := json.Marshal(map[string]any{"components": []componentPayload{}})
	if err != nil {
		return
	}
	call, cancel := context.WithTimeout(ctx, restTimeout)
	defer cancel()
	endpoint := a.restBase + "/channels/" + channelID + "/messages/" + messageID
	req, err := http.NewRequestWithContext(call, http.MethodPatch, endpoint, bytes.NewReader(payload))
	if err != nil {
		return
	}
	req.Header.Set("Authorization", "Bot "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := a.httpClient.Do(req)
	if err != nil {
		return
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, approvalHTTPBytes))
	_ = resp.Body.Close()
}

func (a *Adapter) ackInteraction(ctx context.Context, interaction *interactionPayload) {
	payload, err := json.Marshal(map[string]any{"type": callbackDeferredUpdate})
	if err != nil {
		return
	}
	endpoint := a.restBase + "/interactions/" + interaction.ID + "/" + interaction.Token + "/callback"
	call, cancel := context.WithTimeout(ctx, restTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(call, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := a.httpClient.Do(req)
	if err != nil {
		return
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, approvalHTTPBytes))
	_ = resp.Body.Close()
}

func (a *Adapter) resolveComponentClick(ctx context.Context, interaction *interactionPayload) {
	if interaction == nil || interaction.ID == "" || interaction.Token == "" {
		return
	}
	if interaction.Type != interactionMessageComponent {
		return
	}
	defer a.ackInteraction(ctx, interaction)
	if !a.approvals.NoteSeen(interaction.ID) {
		return
	}
	a.approvals.Resolve(interaction.Data.CustomID, &approval.Presser{
		Actor: interaction.actorID(),
		Scope: [2]string{interaction.GuildID, interaction.ChannelID},
	})
}
