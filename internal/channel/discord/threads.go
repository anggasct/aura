package discord

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/anggasct/aura/internal/config"
	"github.com/anggasct/aura/internal/effect"
	"github.com/anggasct/aura/internal/principal"
	runtimeingress "github.com/anggasct/aura/internal/runtime/ingress"
)

const (
	OpCreateThread = "create_thread"
)

const (
	defaultThreadAutoArchiveMinutes = 1440
	threadNameMaxRunes              = 100
)

const (
	channelTypeGuildText     = 0
	channelTypeGuildNews     = 5
	channelTypeNewsThread    = 10
	channelTypePublicThread  = 11
	channelTypePrivateThread = 12
	channelTypeGuildForum    = 15
	channelTypeGuildMedia    = 16
)

type createThreadOperation struct {
	ParentID           string `json:"parent_id"`
	MessageID          string `json:"message_id"`
	Name               string `json:"name"`
	AutoArchiveMinutes int    `json:"auto_archive_duration"`
}

type threadReceipt struct {
	ThreadID string `json:"thread_id"`
	ParentID string `json:"parent_id"`
}

type threadChannelResponse struct {
	ID       string `json:"id"`
	ParentID string `json:"parent_id"`
	OwnerID  string `json:"owner_id"`
	Type     int    `json:"type"`
}

func threadName(content string) string {
	cleaned := strings.Join(strings.Fields(content), " ")
	runes := []rune(cleaned)
	if len(runes) > threadNameMaxRunes {
		runes = runes[:threadNameMaxRunes]
	}
	named := strings.TrimSpace(string(runes))
	if named == "" {
		return "Discussion"
	}
	return named
}

func skipThreadCreate(channelType *int) bool {
	if channelType == nil {
		return false
	}
	return *channelType == channelTypeGuildForum || *channelType == channelTypeGuildMedia
}

func isThreadChannelType(channelType int) bool {
	return channelType == channelTypeNewsThread || channelType == channelTypePublicThread || channelType == channelTypePrivateThread
}

func (a *Adapter) lookupTriggerThread(triggerID string) (string, bool) {
	a.threadsMu.Lock()
	defer a.threadsMu.Unlock()
	id, ok := a.triggerThreads[triggerID]
	return id, ok
}

func (a *Adapter) lookupThread(threadID string) (threadRoute, bool) {
	a.threadsMu.Lock()
	defer a.threadsMu.Unlock()
	route, ok := a.threadParents[threadID]
	return route, ok
}

func (a *Adapter) rememberThread(triggerID, threadID string, route threadRoute) {
	a.threadsMu.Lock()
	defer a.threadsMu.Unlock()
	if triggerID != "" {
		if _, ok := a.triggerThreads[triggerID]; !ok {
			a.triggerThreads[triggerID] = threadID
		}
	}
	if existing, ok := a.threadParents[threadID]; !ok || existing.persona == "" {
		a.threadParents[threadID] = route
	}
}

func (a *Adapter) threadAutoArchiveMinutes() int {
	if a.cfg != nil && a.cfg.ThreadAutoArchiveMinutes > 0 {
		return a.cfg.ThreadAutoArchiveMinutes
	}
	return defaultThreadAutoArchiveMinutes
}

func (a *Adapter) createThreadREST(ctx context.Context, token, parentID, messageID, name string) (string, error) {
	operation := createThreadOperation{
		ParentID:           parentID,
		MessageID:          messageID,
		Name:               name,
		AutoArchiveMinutes: a.threadAutoArchiveMinutes(),
	}
	payload, err := json.Marshal(map[string]any{
		"name":                  operation.Name,
		"auto_archive_duration": operation.AutoArchiveMinutes,
	})
	if err != nil {
		return "", Errorf(ErrorCodeProtocolInvalid, "thread payload is not serializable")
	}
	endpoint := a.restBase + "/channels/" + parentID + "/messages/" + messageID + "/threads"
	response, outcome, err := a.requestWithRetry(ctx, http.MethodPost, endpoint, token, payload, "application/json")
	if err != nil {
		return "", err
	}
	if !outcome.Succeeded {
		return "", Errorf(ErrorCodeThreadCreateFailed, "thread create did not succeed")
	}
	var created threadChannelResponse
	if err := json.Unmarshal(response, &created); err != nil || created.ID == "" {
		return "", Errorf(ErrorCodeThreadCreateFailed, "thread create response is not decodable")
	}
	return created.ID, nil
}

func (a *Adapter) fetchThreadParent(ctx context.Context, token, threadID string) (threadChannelResponse, error) {
	endpoint := a.restBase + "/channels/" + threadID
	call, cancel := context.WithTimeout(ctx, restTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(call, http.MethodGet, endpoint, http.NoBody)
	if err != nil {
		return threadChannelResponse{}, err
	}
	req.Header.Set("Authorization", "Bot "+token)
	resp, err := a.httpClient.Do(req)
	if err != nil {
		return threadChannelResponse{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return threadChannelResponse{}, Errorf(ErrorCodeThreadUnavailable, "thread lookup did not succeed")
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxRestBytes))
	if err != nil {
		return threadChannelResponse{}, Errorf(ErrorCodeThreadUnavailable, "thread lookup response is not readable")
	}
	var info threadChannelResponse
	if err := json.Unmarshal(body, &info); err != nil || info.ID == "" {
		return threadChannelResponse{}, Errorf(ErrorCodeThreadUnavailable, "thread lookup response is not decodable")
	}
	return info, nil
}

func deliveryTarget(ref *replyReference) (target, parent, messageID string) {
	if ref.ThreadID != "" {
		return ref.ThreadID, ref.ChannelID, ref.MessageID
	}
	return ref.ChannelID, "", ref.MessageID
}

func buildThreadEnvelope(account, principalID string, class principal.Class, msg *messagePayload, threadID, parentID string) (*runtimeingress.IngressEnvelope, error) {
	reference, err := json.Marshal(replyReference{
		ChannelID: parentID,
		MessageID: msg.ID,
		GuildID:   msg.GuildID,
		ThreadID:  threadID,
	})
	if err != nil {
		return nil, Errorf(ErrorCodeProtocolInvalid, "reply reference is not serializable")
	}
	return &runtimeingress.IngressEnvelope{
		Source:         "discord:" + account,
		ExternalID:     "message:" + msg.ID,
		PrincipalID:    principalID,
		PrincipalClass: string(class),
		ConversationID: "thread:" + threadID,
		ReplyContext:   reference,
		Parts:          []runtimeingress.InputPart{{Text: msg.Content}},
		ReceivedAt:     time.Now().UTC(),
	}, nil
}

func validThreadAuthor(account *config.DiscordAccount, owners *principal.Resolver, selfID string, msg *messagePayload) bool {
	if msg.Author.Bot || msg.Author.ID == selfID {
		return false
	}
	if msg.Content == "" {
		return false
	}
	if msg.Type != 0 && msg.Type != 19 {
		return false
	}
	_, _, admitted := resolveAuthor(account, owners, msg.Author.ID)
	return admitted
}

func (a *Adapter) createThreadForTrigger(ctx context.Context, msg *messagePayload, parentID, persona string) (string, error) {
	if id, ok := a.lookupTriggerThread(msg.ID); ok {
		return id, nil
	}
	request, existing, err := a.buildThreadCreateRequest(msg, parentID)
	if err != nil {
		return "", err
	}
	if existing != "" {
		return existing, nil
	}
	sessionID := a.sessionKey(persona, "channel:"+parentID)
	principalID, _, _ := resolveAuthor(a.account, a.owners, msg.Author.ID)
	if err := a.sessions.EnsureSession(ctx, sessionID, principalID, persona); err != nil {
		return "", ErrorWrapf(ErrorCodeThreadCreateFailed, err, "thread create for message %s: parent session could not be ensured", msg.ID)
	}
	intent, err := a.effects.Execute(ctx, &effect.PrepareRequest{
		SessionID:      sessionID,
		IdempotencyKey: "discord:thread:" + msg.ID,
		Provider:       "discord",
		Operation:      OpCreateThread,
		Classification: effect.ClassificationEffectful,
		Request:        request,
		EventKind:      effect.EventKindChannelRequested,
	}, a)
	if err != nil {
		return "", Errorf(ErrorCodeThreadCreateFailed, "thread create for message %s did not succeed", msg.ID)
	}
	if intent == nil {
		return "", Errorf(ErrorCodeThreadCreateFailed, "thread create for message %s did not succeed", msg.ID)
	}
	switch intent.State {
	case effect.StateSucceeded:
		var receipt threadReceipt
		if err := json.Unmarshal(intent.ProviderReceipt, &receipt); err != nil || receipt.ThreadID == "" {
			return "", Errorf(ErrorCodeThreadCreateFailed, "thread create for message %s did not succeed", msg.ID)
		}
		return a.commitThreadReceipt(msg.ID, receipt.ThreadID, threadRoute{parent: parentID, persona: persona})
	case effect.StateUnknown:
		return "", Errorf(ErrorCodeThreadCreateFailed, "thread create for message %s did not succeed", msg.ID)
	default:
		return "", Errorf(ErrorCodeThreadCreateFailed, "thread create for message %s did not succeed", msg.ID)
	}
}

func (a *Adapter) buildThreadCreateRequest(msg *messagePayload, parentID string) (request []byte, existing string, err error) {
	a.threadsMu.Lock()
	defer a.threadsMu.Unlock()
	if id, ok := a.triggerThreads[msg.ID]; ok {
		return nil, id, nil
	}
	operation := createThreadOperation{
		ParentID:           parentID,
		MessageID:          msg.ID,
		Name:               threadName(msg.Content),
		AutoArchiveMinutes: a.threadAutoArchiveMinutes(),
	}
	request, err = marshalOperation(operation)
	if err != nil {
		return nil, "", err
	}
	return request, "", nil
}

func (a *Adapter) commitThreadReceipt(triggerID, threadID string, route threadRoute) (string, error) {
	a.threadsMu.Lock()
	defer a.threadsMu.Unlock()
	if id, ok := a.triggerThreads[triggerID]; ok {
		return id, nil
	}
	a.triggerThreads[triggerID] = threadID
	a.threadParents[threadID] = route
	return threadID, nil
}

func rewriteEnvelopeForThread(envelope *runtimeingress.IngressEnvelope, msg *messagePayload, threadID, parentID string) error {
	reference, err := json.Marshal(replyReference{
		ChannelID: parentID,
		MessageID: msg.ID,
		GuildID:   msg.GuildID,
		ThreadID:  threadID,
	})
	if err != nil {
		return Errorf(ErrorCodeProtocolInvalid, "reply reference is not serializable")
	}
	envelope.ConversationID = "thread:" + threadID
	envelope.ReplyContext = reference
	return nil
}
