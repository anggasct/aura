package discord

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/anggasct/aura/internal/channels/kit/approval"
	auraagent "github.com/anggasct/aura/internal/platform/agent"
	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/principal"
)

const (
	approvalHTTPBytes   = 1 << 16
	suppressEmbedsFlags = 4
)

var backtickRun = regexp.MustCompile("`{3,}")

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
		return fault.New(ErrorCodeInvalidArgument, "context must not be nil")
	}
	if prompt == nil {
		return fault.New(ErrorCodeInvalidArgument, "approval prompt must not be nil")
	}
	if strings.TrimSpace(prompt.ToolName) == "" || strings.TrimSpace(prompt.SessionID) == "" || strings.TrimSpace(prompt.PrincipalID) == "" {
		return fault.New(ErrorCodeInvalidArgument, "approval prompt is missing tool, session, or principal")
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
	return a.decideInChat(ctx, prompt, &binding)
}

func (a *Adapter) decideInChat(ctx context.Context, prompt *ApprovalPrompt, binding *approvalBinding) (bool, error) {
	expiresAt := prompt.ExpiresAt
	if expiresAt.IsZero() {
		expiresAt = time.Now().Add(approval.FallbackTTL)
	}
	if !time.Now().Before(expiresAt) {
		return false, nil
	}
	token, err := a.tokenRef.Resolve()
	if err != nil {
		a.logger.WarnContext(ctx, "approval token unavailable", "component", "discord")
		return false, ctx.Err()
	}
	definition := a.cardPersona(binding.persona)
	content, buttons, err := a.buildToolCard(prompt, &definition, false)
	if err != nil {
		return false, err
	}
	tokenID, pending, err := a.approvals.Issue(binding.shared(), expiresAt)
	if err != nil {
		a.logger.WarnContext(ctx, "approval token mint failed", "component", "discord")
		return false, ctx.Err()
	}
	cardID, err := a.postApprovalCard(ctx, &cardPostRequest{
		token:     token,
		channelID: binding.channelID,
		content:   content,
		tokenID:   tokenID,
		pending:   pending,
		buttons:   buttons,
	})
	if err != nil {
		a.approvals.Drop(tokenID, pending)
		a.logger.WarnContext(ctx, "approval card delivery failed", "component", "discord")
		return false, ctx.Err()
	}
	card := openedCard{
		token:       token,
		tokenID:     tokenID,
		pending:     pending,
		ref:         cardRef{channel: binding.channelID, message: cardID},
		content:     content,
		persona:     definition,
		editOutcome: true,
	}
	outcome, _, err := a.awaitApproval(ctx, &card)
	return outcome.Accepted, err
}

func (a *Adapter) DecideForOwner(ctx context.Context, prompt *ApprovalPrompt, requester *approval.Requester) (bool, error) {
	if err := validateApprovalPrompt(ctx, prompt); err != nil {
		return false, err
	}
	if requester == nil || requester.Principal == "" || requester.Principal != prompt.PrincipalID {
		return false, fault.New(ErrorCodeInvalidArgument, "approval requester is missing or does not match the prompt")
	}
	return a.decideForOwner(ctx, prompt, &approvalBinding{principal: requester.Principal, class: requester.Class, persona: requester.Persona})
}

func (a *Adapter) ServesPersona(persona string) bool {
	return a.router != nil && a.router.ServesPersona(auraagent.BindingChannelDiscord, a.account.Name, persona)
}

type openedCard struct {
	token       string
	tokenID     string
	pending     *approval.Pending
	ref         cardRef
	content     string
	persona     auraagent.Definition
	editOutcome bool
}

type cardPostRequest struct {
	token     string
	channelID string
	content   string
	tokenID   string
	pending   *approval.Pending
	buttons   approval.CardButtons
}

func (a *Adapter) clockNow() time.Time {
	if a.now != nil {
		return a.now()
	}
	return time.Now()
}

func (a *Adapter) cardPersona(personaID string) auraagent.Definition {
	definition, ok := a.personaDefinition(personaID)
	if !ok && personaID == "" && a.account != nil {
		definition, _ = a.personaDefinition(a.account.Persona)
	}
	if definition.ID == "" {
		definition.ID = personaID
	}
	return definition
}

func (a *Adapter) buildToolCard(prompt *ApprovalPrompt, persona *auraagent.Definition, standing bool) (content string, buttons approval.CardButtons, err error) {
	content = renderApprovalCard(prompt, persona)
	longest := approval.LongestOutcomeLine(&approval.OutcomeLineInput{
		Persona:     persona,
		StandingTTL: a.access.StandingGrantTTL,
		Location:    a.ownerLoc,
		Clean:       neutralizeMentions,
	}, standing, utf8.RuneCountInString)
	if utf8.RuneCountInString(content)+1+longest > discordMessageLimit {
		return "", approval.CardButtons{}, approvalUnavailable("approval card exceeds discord message limit")
	}
	var standingTTL time.Duration
	if standing {
		standingTTL = a.access.StandingGrantTTL
	}
	return content, approval.RenderButtons(persona, standingTTL), nil
}

func (a *Adapter) decideForOwner(ctx context.Context, prompt *ApprovalPrompt, requester *approvalBinding) (bool, error) {
	definition := a.cardPersona(requester.persona)
	owners := a.owners.OwnerExternalIDs(principal.ChannelDiscord)
	if len(owners) == 0 {
		a.observeRouted(ctx, approval.RoutedUnavailable)
		return false, approvalUnavailable("owner has no discord identity")
	}
	token, err := a.tokenRef.Resolve()
	if err != nil {
		a.observeRouted(ctx, approval.RoutedUnavailable)
		return false, approvalUnavailable("approval token is unavailable")
	}
	standing := a.offersStandingGrant(prompt, requester.class)
	content, buttons, err := a.buildToolCard(prompt, &definition, standing)
	if err != nil {
		a.observeRouted(ctx, approval.RoutedUnavailable)
		return false, err
	}
	ownerID := owners[0]
	dmChannel, err := a.ownerDMChannel(ctx, token, ownerID)
	if err != nil {
		a.observeRouted(ctx, approval.RoutedUnavailable)
		return false, approvalUnavailable("owner dm channel is unavailable")
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
		standing:  standing,
	}
	tokenID, pending, err := a.approvals.Issue(binding.shared(), expiresAt)
	if err != nil {
		a.observeRouted(ctx, approval.RoutedUnavailable)
		return false, approvalUnavailable("approval token mint failed")
	}
	cardID, err := a.postApprovalCard(ctx, &cardPostRequest{
		token:     token,
		channelID: dmChannel,
		content:   content,
		tokenID:   tokenID,
		pending:   pending,
		buttons:   buttons,
	})
	if err != nil {
		a.approvals.Drop(tokenID, pending)
		a.observeRouted(ctx, approval.RoutedUnavailable)
		return false, approvalUnavailable("owner approval card delivery failed")
	}
	a.postNotice(ctx, token, requester.channelID, definition.Message(auraagent.MessageWaitingForOwner))
	outcome, timedOut, err := a.awaitApproval(ctx, &openedCard{
		token:       token,
		tokenID:     tokenID,
		pending:     pending,
		ref:         cardRef{channel: dmChannel, message: cardID},
		content:     content,
		persona:     definition,
		editOutcome: true,
	})
	notify := context.WithoutCancel(ctx)
	switch {
	case timedOut || errors.Is(err, context.DeadlineExceeded):
		a.observeRouted(ctx, approval.RoutedTimeout)
		a.postNotice(notify, token, requester.channelID, definition.Message(auraagent.MessageApprovalTimeout))
		if err != nil {
			return false, err
		}
		return false, approvalTimeout("owner did not answer the approval in time")
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
		a.postNotice(notify, token, requester.channelID, definition.Message(auraagent.MessageApprovalDenied))
		return false, nil
	}
}

func (a *Adapter) awaitApproval(ctx context.Context, card *openedCard) (approval.Outcome, bool, error) {
	outcome, timedOut, err := a.approvals.Await(ctx, card.tokenID, card.pending)
	notify := context.WithoutCancel(ctx)
	if card.editOutcome {
		a.finishToolCard(notify, card, approval.CardOutcomeOf(outcome, timedOut, err))
	} else {
		a.disableApprovalCard(notify, card.token, card.ref.channel, card.ref.message)
	}
	return outcome, timedOut, err
}

func (a *Adapter) finishToolCard(ctx context.Context, card *openedCard, outcome approval.CardOutcome) {
	if card.ref.channel == "" || card.ref.message == "" {
		return
	}
	line := approval.RenderOutcomeLine(&approval.OutcomeLineInput{
		Persona:     &card.persona,
		Outcome:     outcome,
		StandingTTL: a.access.StandingGrantTTL,
		At:          a.clockNow(),
		Location:    a.ownerLoc,
		Clean:       neutralizeMentions,
	})
	if err := a.editApprovalCard(ctx, card, card.content+"\n"+line); err != nil {
		a.logger.WarnContext(ctx, "approval card outcome edit failed", "component", "discord")
		a.disableApprovalCard(ctx, card.token, card.ref.channel, card.ref.message)
	}
}

func (a *Adapter) editApprovalCard(ctx context.Context, card *openedCard, content string) error {
	payload, err := json.Marshal(map[string]any{
		"content":          content,
		"components":       []componentPayload{},
		"flags":            suppressEmbedsFlags,
		"allowed_mentions": map[string]any{"parse": []string{}},
	})
	if err != nil {
		return fault.New(ErrorCodeProtocolInvalid, "approval card edit is not serializable")
	}
	call, cancel := context.WithTimeout(ctx, restTimeout)
	defer cancel()
	endpoint := a.restBase + "/channels/" + card.ref.channel + "/messages/" + card.ref.message
	req, err := http.NewRequestWithContext(call, http.MethodPatch, endpoint, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bot "+card.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := a.httpClient.Do(req)
	if err != nil {
		return err
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, approvalHTTPBytes))
	_ = resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fault.New(ErrorCodeDeliveryFailed, "approval card edit did not succeed")
	}
	return nil
}

func renderApprovalCard(prompt *ApprovalPrompt, persona *auraagent.Definition) string {
	if prompt == nil {
		return ""
	}
	rendered := approval.RenderCard(prompt, approval.CardOptions{
		Persona:        persona,
		InlineCodeTool: true,
		Clean:          neutralizeMentions,
	})
	return rendered.Header + "\n" + fenceBlock(rendered.Details)
}

func fenceBlock(text string) string {
	body := backtickRun.ReplaceAllStringFunc(text, func(run string) string {
		var broken strings.Builder
		for i := range len(run) {
			broken.WriteByte('`')
			if i%2 == 1 {
				broken.WriteRune('\u200b')
			}
		}
		return broken.String()
	})
	return "```\n" + body + "\n```"
}

func (a *Adapter) approvalButtons(req *cardPostRequest) []componentPayload {
	binding := req.pending.Binding()
	callbacks := a.approvals.Callbacks(req.tokenID, &binding, req.pending.ExpiresAt())
	labels := req.buttons
	if labels.Approve == "" || labels.Reject == "" {
		labels = approval.RenderButtons(nil, 0)
	}
	buttons := []buttonPayload{{Type: componentButton, Style: buttonStyleDanger, Label: labels.Approve, CustomID: callbacks.Approve}}
	if callbacks.Standing != "" {
		buttons = append(buttons, buttonPayload{Type: componentButton, Style: buttonStyleDanger, Label: labels.Standing, CustomID: callbacks.Standing})
	}
	buttons = append(buttons, buttonPayload{Type: componentButton, Style: buttonStylePrimary, Label: labels.Reject, CustomID: callbacks.Reject})
	return []componentPayload{{Type: componentActionRow, Components: buttons}}
}

func (a *Adapter) postApprovalCard(ctx context.Context, card *cardPostRequest) (string, error) {
	payload, err := json.Marshal(map[string]any{
		"content":          card.content,
		"flags":            suppressEmbedsFlags,
		"components":       a.approvalButtons(card),
		"allowed_mentions": map[string]any{"parse": []string{}},
	})
	if err != nil {
		return "", fault.New(ErrorCodeProtocolInvalid, "approval card is not serializable")
	}
	endpoint := a.restBase + "/channels/" + card.channelID + "/messages"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bot "+card.token)
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
		return "", fault.New(ErrorCodeDeliveryFailed, "approval card post did not succeed")
	}
	var message struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(body, &message); err != nil || message.ID == "" {
		return "", deliveryAmbiguous("approval card post has no message id")
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
	presser := &approval.Presser{
		Actor: interaction.actorID(),
		Scope: [2]string{interaction.GuildID, interaction.ChannelID},
	}
	if isStopCustomID(interaction.Data.CustomID) {
		if a.stops == nil || !a.stops.NoteSeen(interaction.ID) {
			a.logger.DebugContext(ctx, "stop press rejected", "component", "discord")
			return
		}
		resolvedAction, binding, resolved := a.stops.ResolveAction(interaction.Data.CustomID, presser)
		if !resolved || resolvedAction != approval.ActionStop {
			a.logger.DebugContext(ctx, "stop press rejected", "component", "discord")
			return
		}
		a.cancelStoppedTurn(ctx, binding.SessionID, binding.TurnID)
		return
	}
	if !a.approvals.NoteSeen(interaction.ID) {
		return
	}
	a.approvals.Resolve(interaction.Data.CustomID, presser)
}

func isStopCustomID(raw string) bool {
	parts := strings.Split(raw, ":")
	return len(parts) == 4 && parts[0] == approval.CallbackPrefix && parts[2] == approval.ActionStop
}

func (a *Adapter) cancelStoppedTurn(ctx context.Context, sessionID, turnID string) {
	if a.commands == nil || sessionID == "" || turnID == "" {
		return
	}
	if _, err := a.commands.CancelTurn(ctx, sessionID, turnID); err != nil {
		a.logger.WarnContext(ctx, "stop turn cancel failed", "component", "discord", "error", err)
	}
}
