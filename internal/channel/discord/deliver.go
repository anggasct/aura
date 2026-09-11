package discord

import (
	"context"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/anggasct/aura/internal/effect"
	runtimechannelhost "github.com/anggasct/aura/internal/runtime/channelhost"
)

const coalescePollInterval = 50 * time.Millisecond

const maxTrackedDeliveries = 4096

type editGate struct {
	mu       sync.Mutex
	entries  map[string]*gateEntry
	interval time.Duration
}

type gateEntry struct {
	lastEdit time.Time
	sequence uint64
	latest   uint64
	receipt  runtimechannelhost.ProviderReceipt
}

func newEditGate(interval time.Duration) *editGate {
	return &editGate{entries: map[string]*gateEntry{}, interval: interval}
}

func (g *editGate) claim(key string) uint64 {
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.entries) >= maxTrackedDeliveries {
		for drop := range g.entries {
			delete(g.entries, drop)
			break
		}
	}
	entry := g.entries[key]
	if entry == nil {
		entry = &gateEntry{}
		g.entries[key] = entry
	}
	entry.sequence++
	entry.latest = entry.sequence
	return entry.sequence
}

func (g *editGate) awaitTurn(ctx context.Context, key string, claimed uint64) (runtimechannelhost.ProviderReceipt, bool, error) {
	for {
		g.mu.Lock()
		entry := g.entries[key]
		if entry == nil || entry.latest != claimed {
			receipt := runtimechannelhost.ProviderReceipt{}
			if entry != nil {
				receipt = entry.receipt
			}
			g.mu.Unlock()
			return receipt, true, nil
		}
		wait := g.interval - time.Since(entry.lastEdit)
		if wait <= 0 {
			entry.lastEdit = time.Now()
			g.mu.Unlock()
			return runtimechannelhost.ProviderReceipt{}, false, nil
		}
		g.mu.Unlock()
		if wait > coalescePollInterval {
			wait = coalescePollInterval
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return runtimechannelhost.ProviderReceipt{}, false, ctx.Err()
		case <-timer.C:
		}
	}
}

func (g *editGate) record(key string, receipt runtimechannelhost.ProviderReceipt) {
	g.mu.Lock()
	defer g.mu.Unlock()
	entry := g.entries[key]
	if entry == nil {
		entry = &gateEntry{}
		g.entries[key] = entry
	}
	entry.lastEdit = time.Now()
	entry.receipt = receipt
}

func (a *Adapter) Deliver(ctx context.Context, req *runtimechannelhost.DeliveryRequest) (runtimechannelhost.ProviderReceipt, error) {
	if req == nil {
		return runtimechannelhost.ProviderReceipt{}, Errorf(ErrorCodeInvalidArgument, "delivery request must not be nil")
	}
	if a.effects == nil {
		return runtimechannelhost.ProviderReceipt{}, Errorf(ErrorCodeDeliveryUnavailable, "effect runner is not configured")
	}
	if req.Channel == "" || req.ConversationID == "" {
		return runtimechannelhost.ProviderReceipt{}, Errorf(ErrorCodeInvalidArgument, "delivery request is missing channel or conversation")
	}
	var texts []string
	for _, part := range req.Parts {
		if strings.TrimSpace(part.Text) != "" {
			texts = append(texts, part.Text)
		}
	}
	if len(texts) == 0 {
		return runtimechannelhost.ProviderReceipt{}, Errorf(ErrorCodeInvalidArgument, "delivery request carries no text")
	}
	text := neutralizeMentions(strings.Join(texts, "\n"))
	chunks := splitMessage(text)
	channelID, messageID, err := parseReplyContext(req.ReplyContext, req.Channel)
	if err != nil {
		return runtimechannelhost.ProviderReceipt{}, err
	}
	_ = messageID
	key := req.Channel + "|" + req.EffectID
	tracked := req.EffectID != ""
	posted, ok := a.postedMessages(key)
	if tracked && ok {
		return a.deliverEdit(ctx, req, key, channelID, posted, chunks)
	}
	return a.deliverPost(ctx, req, key, tracked, channelID, req.ReplyContext, chunks)
}

func keyBase(req *runtimechannelhost.DeliveryRequest) string {
	if req.EffectID != "" {
		return req.EffectID
	}
	return "anon:" + req.IdempotencyKey
}

func (a *Adapter) deliverPost(ctx context.Context, req *runtimechannelhost.DeliveryRequest, key string, tracked bool, channelID string, replyContext []byte, chunks []string) (runtimechannelhost.ProviderReceipt, error) {
	texts, uploads, err := splitUploads(chunks, a.cfg.MaxAttachmentBytes)
	if err != nil {
		return runtimechannelhost.ProviderReceipt{}, err
	}
	posted, err := a.postChunks(ctx, req, channelID, replyContext, texts, uploads)
	if err != nil {
		return runtimechannelhost.ProviderReceipt{}, err
	}
	receipt := lastReceipt(posted)
	if tracked {
		a.rememberPosted(key, posted)
		a.gate.record(key, receipt)
	}
	return receipt, nil
}

func (a *Adapter) deliverEdit(ctx context.Context, req *runtimechannelhost.DeliveryRequest, key, channelID string, posted, chunks []string) (runtimechannelhost.ProviderReceipt, error) {
	claimed := a.gate.claim(key)
	receipt, superseded, err := a.gate.awaitTurn(ctx, key, claimed)
	if err != nil {
		return runtimechannelhost.ProviderReceipt{}, err
	}
	if superseded {
		return receipt, nil
	}
	texts, uploads, err := splitUploads(chunks, a.cfg.MaxAttachmentBytes)
	if err != nil {
		return runtimechannelhost.ProviderReceipt{}, err
	}
	edited, err := a.reconcileMessages(ctx, req, channelID, posted, texts, uploads)
	if err != nil {
		return runtimechannelhost.ProviderReceipt{}, err
	}
	a.gate.record(key, lastReceipt(edited))
	a.rememberPosted(key, edited)
	return lastReceipt(edited), nil
}

func (a *Adapter) reconcileMessages(ctx context.Context, req *runtimechannelhost.DeliveryRequest, channelID string, posted, texts []string, uploads []uploadChunk) ([]string, error) {
	var ids []string
	for index, text := range texts {
		if index < len(posted) {
			if err := a.executeEdit(ctx, req, channelID, posted[index], index, text); err != nil {
				return nil, err
			}
			ids = append(ids, posted[index])
			continue
		}
		id, err := a.executePost(ctx, req, channelID, nil, index, text, nil)
		if err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	if len(uploads) > 0 {
		id, err := a.executePost(ctx, req, channelID, nil, len(texts), uploads[0].text, &uploads[0])
		if err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, nil
}

func (a *Adapter) postChunks(ctx context.Context, req *runtimechannelhost.DeliveryRequest, channelID string, replyContext []byte, texts []string, uploads []uploadChunk) ([]string, error) {
	var ids []string
	_, inboundMessage, err := parseReplyContext(replyContext, channelID)
	if err != nil {
		return nil, err
	}
	for index, text := range texts {
		var replyTo *string
		if index == 0 && inboundMessage != "" {
			replyTo = &inboundMessage
		}
		id, err := a.executePost(ctx, req, channelID, replyTo, index, text, nil)
		if err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	for _, upload := range uploads {
		id, err := a.executePost(ctx, req, channelID, nil, len(ids), upload.text, &upload)
		if err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, nil
}

func (a *Adapter) executePost(ctx context.Context, req *runtimechannelhost.DeliveryRequest, channelID string, replyTo *string, index int, text string, upload *uploadChunk) (string, error) {
	operation := sendOperation{ChannelID: channelID, Text: text}
	if replyTo != nil {
		operation.ReplyTo = *replyTo
	}
	label := "c" + strconv.Itoa(index)
	if upload != nil {
		operation.FileName = upload.name
		operation.FileBody = []byte(upload.text)
		label = "f"
	}
	request, err := marshalOperation(operation)
	if err != nil {
		return "", err
	}
	intent, err := a.effects.Execute(ctx, &effect.PrepareRequest{
		SessionID:      req.ConversationID,
		IdempotencyKey: idempotencyKey(keyBase(req), label, text),
		Provider:       "discord",
		Operation:      opSendMessage,
		Classification: effect.ClassificationEffectful,
		Request:        request,
		EventKind:      effect.EventKindChannelRequested,
	}, a)
	if err != nil {
		return "", deliveryError(ctx, intent, err)
	}
	return receiptMessageID(intent)
}

func (a *Adapter) executeEdit(ctx context.Context, req *runtimechannelhost.DeliveryRequest, channelID, messageID string, index int, text string) error {
	request, err := marshalOperation(editOperation{ChannelID: channelID, MessageID: messageID, Text: text})
	if err != nil {
		return err
	}
	intent, err := a.effects.Execute(ctx, &effect.PrepareRequest{
		SessionID:      req.ConversationID,
		IdempotencyKey: idempotencyKey(keyBase(req), "e"+strconv.Itoa(index), text),
		Provider:       "discord",
		Operation:      opEditMessage,
		Classification: effect.ClassificationEffectful,
		Request:        request,
		EventKind:      effect.EventKindChannelRequested,
	}, a)
	if err != nil {
		return deliveryError(ctx, intent, err)
	}
	_, err = receiptMessageID(intent)
	return err
}

func deliveryError(ctx context.Context, intent *effect.Intent, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if intent != nil {
		if _, stateErr := receiptMessageID(intent); stateErr != nil {
			return stateErr
		}
	}
	return err
}

func receiptMessageID(intent *effect.Intent) (string, error) {
	if intent == nil {
		return "", Errorf(ErrorCodeDeliveryAmbiguous, "delivery outcome is unknown")
	}
	switch intent.State {
	case effect.StateSucceeded:
		var receipt messageReceipt
		if err := unmarshalReceipt(intent.ProviderReceipt, &receipt); err != nil || receipt.MessageID == "" {
			return "", Errorf(ErrorCodeDeliveryAmbiguous, "delivery receipt is missing the message id")
		}
		return receipt.MessageID, nil
	case effect.StateUnknown:
		return "", Errorf(ErrorCodeDeliveryAmbiguous, "delivery outcome is unknown")
	default:
		return "", Errorf(ErrorCodeDeliveryFailed, "delivery settled without success")
	}
}

func lastReceipt(ids []string) runtimechannelhost.ProviderReceipt {
	if len(ids) == 0 {
		return runtimechannelhost.ProviderReceipt{}
	}
	return runtimechannelhost.ProviderReceipt{ProviderID: "discord", ExternalID: ids[len(ids)-1], At: time.Now().UTC()}
}
