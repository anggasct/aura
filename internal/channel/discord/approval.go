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
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	approvalTokenPrefix = "aura"
	approvalTokenBytes  = 8
	approvalMacBytes    = 8
	approvalActionOk    = "a"
	approvalActionDeny  = "r"
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
	guildID   string
	channelID string
}

type approvalOutcome struct {
	accepted bool
}

type pendingApproval struct {
	binding     approvalBinding
	expiresAt   time.Time
	decided     bool
	result      chan approvalOutcome
	cardChannel string
	cardMessage string
}

func (a *Adapter) rememberTarget(conversationID string, binding approvalBinding) {
	a.targetsMu.Lock()
	defer a.targetsMu.Unlock()
	if len(a.targets) >= maxTrackedDeliveries {
		for drop := range a.targets {
			delete(a.targets, drop)
			break
		}
	}
	a.targets[conversationID] = binding
}

func (a *Adapter) lookupTarget(conversationID string) (approvalBinding, bool) {
	a.targetsMu.Lock()
	defer a.targetsMu.Unlock()
	binding, ok := a.targets[conversationID]
	return binding, ok
}

func (a *Adapter) approvalMAC(tokenID, action string, binding approvalBinding, expiryUnix int64) string {
	mac := hmac.New(sha256.New, a.approvalKey[:])
	_, _ = mac.Write([]byte(tokenID + "|" + action + "|" + binding.principal + "|" + binding.guildID + "|" + binding.channelID + "|" + strconv.FormatInt(expiryUnix, 10)))
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
	if action != approvalActionOk && action != approvalActionDeny {
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

func (a *Adapter) issueApproval(binding approvalBinding, expiresAt time.Time) (tokenID string, pending *pendingApproval, err error) {
	var raw [approvalTokenBytes]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", nil, err
	}
	tokenID = hex.EncodeToString(raw[:])
	pending = &pendingApproval{
		binding:   binding,
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
	expiresAt := prompt.ExpiresAt
	if expiresAt.IsZero() {
		expiresAt = time.Now().Add(approvalFallbackTTL)
	}
	if !time.Now().Before(expiresAt) {
		return false, nil
	}
	token, err := a.tokenRef.Resolve()
	if err != nil {
		a.logger.WarnContext(ctx, "approval token unavailable", "component", "discord")
		return false, nil
	}
	tokenID, pending, err := a.issueApproval(binding, expiresAt)
	if err != nil {
		a.logger.WarnContext(ctx, "approval token mint failed", "component", "discord")
		return false, nil
	}
	card := renderApprovalCard(prompt)
	cardID, err := a.postApprovalCard(ctx, token, binding.channelID, card, tokenID, binding, expiresAt)
	if err != nil {
		a.dropApproval(tokenID, pending)
		a.logger.WarnContext(ctx, "approval card delivery failed", "component", "discord")
		return false, nil
	}
	a.approvalsMu.Lock()
	pending.cardChannel = binding.channelID
	pending.cardMessage = cardID
	a.approvalsMu.Unlock()
	timer := time.NewTimer(time.Until(expiresAt))
	defer timer.Stop()
	select {
	case outcome := <-pending.result:
		a.dropApproval(tokenID, pending)
		a.approvalsMu.Lock()
		cardChannel, cardMessage := pending.cardChannel, pending.cardMessage
		a.approvalsMu.Unlock()
		a.disableApprovalCard(context.WithoutCancel(ctx), token, cardChannel, cardMessage)
		return outcome.accepted, nil
	case <-timer.C:
		a.dropApproval(tokenID, pending)
		a.approvalsMu.Lock()
		cardChannel, cardMessage := pending.cardChannel, pending.cardMessage
		a.approvalsMu.Unlock()
		a.disableApprovalCard(context.WithoutCancel(ctx), token, cardChannel, cardMessage)
		return false, nil
	case <-ctx.Done():
		a.dropApproval(tokenID, pending)
		a.approvalsMu.Lock()
		cardChannel, cardMessage := pending.cardChannel, pending.cardMessage
		a.approvalsMu.Unlock()
		a.disableApprovalCard(context.WithoutCancel(ctx), token, cardChannel, cardMessage)
		return false, ctx.Err()
	}
}

func renderApprovalCard(prompt *ApprovalPrompt) string {
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
	card.WriteString("\nsession: " + neutralizeMentions(prompt.SessionID))
	card.WriteString("\nprincipal: " + neutralizeMentions(prompt.PrincipalID))
	card.WriteString("\narguments: " + arguments)
	card.WriteString("\napprove exactly this request?")
	return card.String()
}

func (a *Adapter) approvalButtons(tokenID string, binding approvalBinding, expiresAt time.Time) []componentPayload {
	expiry := expiresAt.Unix()
	approve := approvalCustomID(tokenID, approvalActionOk, a.approvalMAC(tokenID, approvalActionOk, binding, expiry))
	reject := approvalCustomID(tokenID, approvalActionDeny, a.approvalMAC(tokenID, approvalActionDeny, binding, expiry))
	return []componentPayload{{
		Type: componentActionRow,
		Components: []buttonPayload{
			{Type: componentButton, Style: buttonStyleDanger, Label: "Approve", CustomID: approve},
			{Type: componentButton, Style: buttonStylePrimary, Label: "Reject", CustomID: reject},
		},
	}}
}

func (a *Adapter) postApprovalCard(ctx context.Context, token, channelID, content, tokenID string, binding approvalBinding, expiresAt time.Time) (string, error) {
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
	expected := a.approvalMAC(tokenID, action, pending.binding, pending.expiresAt.Unix())
	binding := pending.binding
	a.approvalsMu.Unlock()
	if subtle.ConstantTimeCompare([]byte(mac), []byte(expected)) != 1 {
		return
	}
	if interaction.actorID() != binding.principal || interaction.ChannelID != binding.channelID || (binding.guildID != "" && interaction.GuildID != binding.guildID) {
		return
	}
	a.approvalsMu.Lock()
	if pending.decided {
		a.approvalsMu.Unlock()
		return
	}
	pending.decided = true
	a.approvalsMu.Unlock()
	pending.result <- approvalOutcome{accepted: action == approvalActionOk}
}
