package discord

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/anggasct/aura/internal/config"
	"github.com/anggasct/aura/internal/principal"
)

const (
	approvalTokenPrefix    = "aura"
	approvalTokenBytes     = 8
	approvalMacBytes       = 8
	approvalActionOk       = "a"
	approvalActionDeny     = "r"
	approvalActionStanding = "s"
)

const (
	maxTrackedApprovals  = 1024
	maxSeenInteractions  = 4096
	approvalFallbackTTL  = 5 * time.Minute
	approvalCardArgRunes = 1200
	approvalHTTPBytes    = 1 << 16
)

type ApprovalPrompt struct {
	ToolName       string
	ToolVersion    string
	SessionID      string
	TurnID         string
	PrincipalID    string
	Arguments      string
	Network        bool
	Timeout        time.Duration
	MaxOutputBytes int64
	PolicyVersion  string
	ReasonCode     string
	ExpiresAt      time.Time
}

type approvalBinding struct {
	principal string
	class     principal.Class
	persona   string
	actor     string
	guildID   string
	channelID string
	standing  bool
}

type approvalOutcome struct {
	accepted bool
	standing bool
}

type pendingApproval struct {
	binding     approvalBinding
	expiresAt   time.Time
	decided     bool
	result      chan approvalOutcome
	cardChannel string
	cardMessage string
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

func (a *Adapter) approvalMAC(tokenID, action string, binding *approvalBinding, expiryUnix int64) string {
	mac := hmac.New(sha256.New, a.approvalKey[:])
	_, _ = mac.Write([]byte(tokenID + "|" + action + "|" + binding.actor + "|" + binding.guildID + "|" + binding.channelID + "|" + strconv.FormatInt(expiryUnix, 10)))
	return hex.EncodeToString(mac.Sum(nil)[:approvalMacBytes])
}

func approvalCustomID(tokenID, action, mac string) string {
	return approvalTokenPrefix + ":" + tokenID + ":" + action + ":" + mac
}

func parseApprovalCustomID(raw string) (tokenID, action, mac string, ok bool) {
	parts := strings.Split(raw, ":")
	if len(parts) != 4 || parts[0] != approvalTokenPrefix {
		return "", "", "", false
	}
	tokenID, action, mac = parts[1], parts[2], parts[3]
	if len(tokenID) != approvalTokenBytes*2 || len(mac) != approvalMacBytes*2 {
		return "", "", "", false
	}
	if action != approvalActionOk && action != approvalActionDeny && action != approvalActionStanding {
		return "", "", "", false
	}
	if !isHex(tokenID) || !isHex(mac) {
		return "", "", "", false
	}
	return tokenID, action, mac, true
}

func isHex(value string) bool {
	for i := range len(value) {
		c := value[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

func (a *Adapter) issueApproval(binding *approvalBinding, expiresAt time.Time) (tokenID string, pending *pendingApproval, err error) {
	var raw [approvalTokenBytes]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", nil, err
	}
	tokenID = hex.EncodeToString(raw[:])
	pending = &pendingApproval{
		binding:   *binding,
		expiresAt: expiresAt,
		result:    make(chan approvalOutcome, 1),
	}
	a.approvalsMu.Lock()
	defer a.approvalsMu.Unlock()
	if len(a.approvals) >= maxTrackedApprovals {
		for drop := range a.approvals {
			delete(a.approvals, drop)
			break
		}
	}
	a.approvals[tokenID] = pending
	return tokenID, pending, nil
}

func (a *Adapter) dropApproval(tokenID string, pending *pendingApproval) {
	a.approvalsMu.Lock()
	defer a.approvalsMu.Unlock()
	if current, ok := a.approvals[tokenID]; ok && current == pending {
		delete(a.approvals, tokenID)
	}
}

func (a *Adapter) Decide(ctx context.Context, prompt *ApprovalPrompt) (bool, error) {
	if ctx == nil {
		return false, Errorf(ErrorCodeInvalidArgument, "context must not be nil")
	}
	if prompt == nil {
		return false, Errorf(ErrorCodeInvalidArgument, "approval prompt must not be nil")
	}
	if strings.TrimSpace(prompt.ToolName) == "" || strings.TrimSpace(prompt.SessionID) == "" || strings.TrimSpace(prompt.PrincipalID) == "" {
		return false, Errorf(ErrorCodeInvalidArgument, "approval prompt is missing tool, session, or principal")
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
		return false, nil
	}
	outcome, _, err := a.awaitApproval(ctx, card.token, card.tokenID, card.pending, card.expiresAt)
	return outcome.accepted, err
}

type openedCard struct {
	token     string
	tokenID   string
	pending   *pendingApproval
	expiresAt time.Time
}

func (a *Adapter) openChannelCard(ctx context.Context, prompt *ApprovalPrompt, binding *approvalBinding) (openedCard, bool) {
	expiresAt := prompt.ExpiresAt
	if expiresAt.IsZero() {
		expiresAt = time.Now().Add(approvalFallbackTTL)
	}
	if !time.Now().Before(expiresAt) {
		return openedCard{}, false
	}
	token, err := a.tokenRef.Resolve()
	if err != nil {
		a.logger.WarnContext(ctx, "approval token unavailable", "component", "discord")
		return openedCard{}, false
	}
	tokenID, pending, err := a.issueApproval(binding, expiresAt)
	if err != nil {
		a.logger.WarnContext(ctx, "approval token mint failed", "component", "discord")
		return openedCard{}, false
	}
	cardID, err := a.postApprovalCard(ctx, token, binding.channelID, renderApprovalCard(prompt, ""), tokenID, binding, expiresAt)
	if err != nil {
		a.dropApproval(tokenID, pending)
		a.logger.WarnContext(ctx, "approval card delivery failed", "component", "discord")
		return openedCard{}, false
	}
	a.trackApprovalCard(pending, binding.channelID, cardID)
	return openedCard{token: token, tokenID: tokenID, pending: pending, expiresAt: expiresAt}, true
}

func (a *Adapter) decideForOwner(ctx context.Context, prompt *ApprovalPrompt, requester *approvalBinding) (bool, error) {
	definition, _ := a.personaDefinition(requester.persona)
	if definition.ID == "" {
		definition.ID = requester.persona
	}
	owners := a.owners.OwnerExternalIDs(principal.ChannelDiscord)
	if len(owners) == 0 {
		a.observeRouted(ctx, ApprovalRoutedUnavailable)
		return false, Errorf(ErrorCodeApprovalUnavailable, "owner has no discord identity")
	}
	token, err := a.tokenRef.Resolve()
	if err != nil {
		a.observeRouted(ctx, ApprovalRoutedUnavailable)
		return false, Errorf(ErrorCodeApprovalUnavailable, "approval token is unavailable")
	}
	ownerID := owners[0]
	dmChannel, err := a.ownerDMChannel(ctx, token, ownerID)
	if err != nil {
		a.observeRouted(ctx, ApprovalRoutedUnavailable)
		return false, Errorf(ErrorCodeApprovalUnavailable, "owner dm channel is unavailable")
	}
	now := time.Now()
	expiresAt := now.Add(a.ownerApprovalTimeout(&definition))
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
		standing:  a.offersStandingGrant(requester.class),
	}
	tokenID, pending, err := a.issueApproval(&binding, expiresAt)
	if err != nil {
		a.observeRouted(ctx, ApprovalRoutedUnavailable)
		return false, Errorf(ErrorCodeApprovalUnavailable, "approval token mint failed")
	}
	cardID, err := a.postApprovalCard(ctx, token, dmChannel, renderApprovalCard(prompt, personaLabel(&definition)), tokenID, &binding, expiresAt)
	if err != nil {
		a.dropApproval(tokenID, pending)
		a.observeRouted(ctx, ApprovalRoutedUnavailable)
		return false, Errorf(ErrorCodeApprovalUnavailable, "owner approval card delivery failed")
	}
	a.trackApprovalCard(pending, dmChannel, cardID)
	a.postNotice(ctx, token, requester.channelID, definition.Message(config.MessageWaitingForOwner))
	outcome, timedOut, err := a.awaitApproval(ctx, token, tokenID, pending, expiresAt)
	notify := context.WithoutCancel(ctx)
	switch {
	case timedOut || errors.Is(err, context.DeadlineExceeded):
		a.observeRouted(ctx, ApprovalRoutedTimeout)
		a.postNotice(notify, token, requester.channelID, definition.Message(config.MessageApprovalTimeout))
		if err != nil {
			return false, err
		}
		return false, Errorf(ErrorCodeApprovalTimeout, "owner did not answer the approval in time")
	case err != nil:
		return false, err
	case outcome.standing:
		a.observeRouted(ctx, ApprovalRoutedStanding)
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
	case outcome.accepted:
		a.observeRouted(ctx, ApprovalRoutedApproved)
		return true, nil
	default:
		a.observeRouted(ctx, ApprovalRoutedRejected)
		a.postNotice(notify, token, requester.channelID, definition.Message(config.MessageApprovalDenied))
		return false, nil
	}
}

func (a *Adapter) trackApprovalCard(pending *pendingApproval, channelID, messageID string) {
	a.approvalsMu.Lock()
	pending.cardChannel = channelID
	pending.cardMessage = messageID
	a.approvalsMu.Unlock()
}

func (a *Adapter) awaitApproval(ctx context.Context, token, tokenID string, pending *pendingApproval, expiresAt time.Time) (outcome approvalOutcome, timedOut bool, err error) {
	timer := time.NewTimer(time.Until(expiresAt))
	defer timer.Stop()
	defer func() {
		a.dropApproval(tokenID, pending)
		a.approvalsMu.Lock()
		cardChannel, cardMessage := pending.cardChannel, pending.cardMessage
		a.approvalsMu.Unlock()
		a.disableApprovalCard(context.WithoutCancel(ctx), token, cardChannel, cardMessage)
	}()
	select {
	case outcome = <-pending.result:
		return outcome, false, nil
	case <-timer.C:
		return approvalOutcome{}, true, nil
	case <-ctx.Done():
		return approvalOutcome{}, false, ctx.Err()
	}
}

func renderApprovalCard(prompt *ApprovalPrompt, persona string) string {
	arguments := neutralizeMentions(strings.TrimSpace(prompt.Arguments))
	if arguments == "" {
		arguments = "{}"
	}
	if runes := []rune(arguments); len(runes) > approvalCardArgRunes {
		arguments = string(runes[:approvalCardArgRunes]) + "...[truncated]"
	}
	var card strings.Builder
	card.WriteString("approval required: " + neutralizeMentions(prompt.ToolName))
	if prompt.ToolVersion != "" {
		card.WriteString("@" + neutralizeMentions(prompt.ToolVersion))
	}
	if persona != "" {
		card.WriteString("\npersona: " + neutralizeMentions(persona))
	}
	card.WriteString("\nsession: " + neutralizeMentions(prompt.SessionID))
	card.WriteString("\nprincipal: " + neutralizeMentions(prompt.PrincipalID))
	card.WriteString("\narguments: " + arguments)
	card.WriteString("\napprove exactly this request?")
	return card.String()
}

func (a *Adapter) approvalButtons(tokenID string, binding *approvalBinding, expiresAt time.Time) []componentPayload {
	expiry := expiresAt.Unix()
	approve := approvalCustomID(tokenID, approvalActionOk, a.approvalMAC(tokenID, approvalActionOk, binding, expiry))
	reject := approvalCustomID(tokenID, approvalActionDeny, a.approvalMAC(tokenID, approvalActionDeny, binding, expiry))
	buttons := []buttonPayload{{Type: componentButton, Style: buttonStyleDanger, Label: "Approve", CustomID: approve}}
	if binding.standing {
		standing := approvalCustomID(tokenID, approvalActionStanding, a.approvalMAC(tokenID, approvalActionStanding, binding, expiry))
		buttons = append(buttons, buttonPayload{Type: componentButton, Style: buttonStyleDanger, Label: standingGrantLabel(a.access.StandingGrantTTL), CustomID: standing})
	}
	buttons = append(buttons, buttonPayload{Type: componentButton, Style: buttonStylePrimary, Label: "Reject", CustomID: reject})
	return []componentPayload{{Type: componentActionRow, Components: buttons}}
}

func (a *Adapter) postApprovalCard(ctx context.Context, token, channelID, content, tokenID string, binding *approvalBinding, expiresAt time.Time) (string, error) {
	payload, err := json.Marshal(map[string]any{
		"content":          content,
		"components":       a.approvalButtons(tokenID, binding, expiresAt),
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

func (a *Adapter) noteSeenInteraction(id string) bool {
	a.approvalsMu.Lock()
	defer a.approvalsMu.Unlock()
	if _, ok := a.seen[id]; ok {
		return false
	}
	if len(a.seen) >= maxSeenInteractions {
		for drop := range a.seen {
			delete(a.seen, drop)
			break
		}
	}
	a.seen[id] = struct{}{}
	return true
}

func (a *Adapter) resolveComponentClick(ctx context.Context, interaction *interactionPayload) {
	if interaction == nil || interaction.ID == "" || interaction.Token == "" {
		return
	}
	if interaction.Type != interactionMessageComponent {
		return
	}
	defer a.ackInteraction(ctx, interaction)
	if !a.noteSeenInteraction(interaction.ID) {
		return
	}
	tokenID, action, mac, ok := parseApprovalCustomID(interaction.Data.CustomID)
	if !ok {
		return
	}
	a.approvalsMu.Lock()
	pending, found := a.approvals[tokenID]
	if found && (pending.decided || !time.Now().Before(pending.expiresAt)) {
		if !pending.decided {
			delete(a.approvals, tokenID)
		}
		a.approvalsMu.Unlock()
		return
	}
	if !found {
		a.approvalsMu.Unlock()
		return
	}
	expected := a.approvalMAC(tokenID, action, &pending.binding, pending.expiresAt.Unix())
	binding := pending.binding
	a.approvalsMu.Unlock()
	if subtle.ConstantTimeCompare([]byte(mac), []byte(expected)) != 1 {
		return
	}
	if interaction.actorID() != binding.actor || interaction.ChannelID != binding.channelID || (binding.guildID != "" && interaction.GuildID != binding.guildID) {
		return
	}
	if action == approvalActionStanding && !binding.standing {
		return
	}
	a.approvalsMu.Lock()
	if pending.decided {
		a.approvalsMu.Unlock()
		return
	}
	pending.decided = true
	a.approvalsMu.Unlock()
	pending.result <- approvalOutcome{accepted: action == approvalActionOk || action == approvalActionStanding, standing: action == approvalActionStanding}
}
