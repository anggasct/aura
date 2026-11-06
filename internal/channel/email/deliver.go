package email

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"

	"github.com/anggasct/aura/internal/effect"
	"github.com/anggasct/aura/internal/principal"
	runtimechannelhost "github.com/anggasct/aura/internal/runtime/channelhost"
)

const (
	ProviderName       = "email"
	OpSendReply        = "send_reply"
	sendMaxAttempts    = 3
	maxDeliverInflight = 4
	safeCodePrefix     = "email_"
	faultCancelled     = "email_cancelled"
	keyDigestChars     = 16
)

type sendOperation struct {
	To         string   `json:"to"`
	Subject    string   `json:"subject"`
	MessageID  string   `json:"message_id"`
	InReplyTo  string   `json:"in_reply_to,omitempty"`
	References []string `json:"references,omitempty"`
	Body       string   `json:"body"`
}

type sendReceipt struct {
	MessageID string `json:"message_id"`
}

func (a *Adapter) Deliver(ctx context.Context, req *runtimechannelhost.DeliveryRequest) (runtimechannelhost.ProviderReceipt, error) {
	if req == nil {
		return runtimechannelhost.ProviderReceipt{}, Errorf(ErrorCodeInvalidArgument, "delivery request must not be nil")
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
	body := replyBody(texts)
	if body == "" {
		return runtimechannelhost.ProviderReceipt{}, Errorf(ErrorCodeInvalidArgument, "delivery request carries no text")
	}
	var target replyReference
	if err := json.Unmarshal(req.ReplyContext, &target); err != nil || !a.recipientAllowed(target.Sender) {
		return runtimechannelhost.ProviderReceipt{}, Errorf(ErrorCodeInvalidArgument, "delivery reply context carries no usable recipient")
	}
	select {
	case a.deliverSem <- struct{}{}:
	case <-ctx.Done():
		return runtimechannelhost.ProviderReceipt{}, ctx.Err()
	}
	defer func() { <-a.deliverSem }()
	base := keyBase(req)
	operation := a.operationFor(&target, base, body)
	messageID, err := a.executeSend(ctx, req, base, &operation)
	if err != nil {
		return runtimechannelhost.ProviderReceipt{}, err
	}
	return runtimechannelhost.ProviderReceipt{ProviderID: ProviderName, ExternalID: messageID, At: a.now().UTC()}, nil
}

func (a *Adapter) recipientAllowed(recipient string) bool {
	return principal.IsPlainEmailAddress(recipient) && recipient != a.account.Address
}

func (a *Adapter) operationFor(target *replyReference, base, body string) sendOperation {
	operation := sendOperation{
		To:         target.Sender,
		Subject:    replySubject(target.Subject),
		MessageID:  replyMessageID(base, domainOf(a.account.Address)),
		References: boundedReferences(target.References),
		Body:       body,
	}
	if headerSafeID(target.MessageID) {
		operation.InReplyTo = target.MessageID
		operation.References = boundedReferences(append(operation.References, target.MessageID))
	}
	return operation
}

func (a *Adapter) executeSend(ctx context.Context, req *runtimechannelhost.DeliveryRequest, base string, operation *sendOperation) (string, error) {
	request, err := json.Marshal(operation)
	if err != nil {
		return "", Errorf(ErrorCodeMessageInvalid, "send request is not serializable")
	}
	intent, err := a.effects.Execute(ctx, &effect.PrepareRequest{
		SessionID:      req.ConversationID,
		IdempotencyKey: idempotencyKey(base, req.ConversationID, operation),
		Provider:       ProviderName,
		Operation:      OpSendReply,
		Classification: effect.ClassificationEffectful,
		Request:        request,
		EventKind:      effect.EventKindChannelRequested,
	}, a)
	if err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		if intent != nil {
			if _, stateErr := receiptMessageID(intent); stateErr != nil {
				return "", stateErr
			}
		}
		return "", err
	}
	return receiptMessageID(intent)
}

func keyBase(req *runtimechannelhost.DeliveryRequest) string {
	if req.EffectID != "" {
		return req.EffectID
	}
	return "anon:" + req.IdempotencyKey
}

func idempotencyKey(base, session string, operation *sendOperation) string {
	body := sha256.Sum256([]byte(operation.Body))
	sum := sha256.Sum256([]byte(base + "|" + session + "|" + operation.InReplyTo + "|" + hex.EncodeToString(body[:])))
	return ProviderName + ":" + base + ":" + hex.EncodeToString(sum[:])[:keyDigestChars]
}

func receiptMessageID(intent *effect.Intent) (string, error) {
	if intent == nil {
		return "", Errorf(ErrorCodeDeliveryAmbiguous, "delivery outcome is unknown")
	}
	switch intent.State {
	case effect.StateSucceeded:
		var receipt sendReceipt
		if err := json.Unmarshal(intent.ProviderReceipt, &receipt); err != nil || receipt.MessageID == "" {
			return "", Errorf(ErrorCodeDeliveryAmbiguous, "delivery receipt is missing the message id")
		}
		return receipt.MessageID, nil
	case effect.StateUnknown, effect.StateStarted:
		return "", Errorf(ErrorCodeDeliveryAmbiguous, "delivery outcome is unknown")
	default:
		return "", Errorf(ErrorCodeDeliveryFailed, "delivery settled without success")
	}
}

func (a *Adapter) SupportsIdempotency() bool {
	return false
}

func (a *Adapter) Invoke(ctx context.Context, invocation *effect.Invocation) (effect.Outcome, error) {
	if invocation == nil {
		return effect.Outcome{}, Errorf(ErrorCodeInvalidArgument, "invocation must not be nil")
	}
	if invocation.Operation != OpSendReply {
		return effect.Outcome{}, Errorf(ErrorCodeInvalidArgument, "unsupported effect operation")
	}
	var operation sendOperation
	if err := json.Unmarshal(invocation.Request, &operation); err != nil {
		return effect.Outcome{}, Errorf(ErrorCodeInvalidArgument, "send request is not decodable")
	}
	raw, err := a.renderOperation(&operation)
	if err != nil {
		outcome, _ := failureOutcome(err)
		return outcome, nil
	}
	return a.submit(ctx, &Submission{From: a.account.Address, To: operation.To, Message: raw}, operation.MessageID)
}

func (a *Adapter) renderOperation(operation *sendOperation) ([]byte, error) {
	if !a.recipientAllowed(operation.To) {
		return nil, Errorf(ErrorCodeMessageInvalid, "recipient is not usable")
	}
	message := replyMessage{
		From:       a.account.Address,
		To:         operation.To,
		Subject:    operation.Subject,
		MessageID:  operation.MessageID,
		InReplyTo:  operation.InReplyTo,
		References: operation.References,
		Body:       operation.Body,
		Date:       a.now(),
	}
	return message.render()
}

func (a *Adapter) submit(ctx context.Context, submission *Submission, messageID string) (effect.Outcome, error) {
	var last ErrorCode
	for attempt := range sendMaxAttempts {
		if attempt > 0 && !sleepContext(ctx, a.retryDelay(attempt-1)) {
			return effect.Outcome{SafeErrorCode: faultCancelled}, nil
		}
		err := a.mailer.Submit(ctx, submission)
		if err == nil {
			a.smtpFault.Store(faultNone)
			return successOutcome(messageID), nil
		}
		outcome, code := failureOutcome(err)
		if outcome.Ambiguous {
			return outcome, nil
		}
		last = code
		if cancelled(ctx) {
			return effect.Outcome{SafeErrorCode: faultCancelled}, nil
		}
		if !retryable(code) {
			break
		}
	}
	a.noteSubmitFailure(last)
	return effect.Outcome{SafeErrorCode: safeCodePrefix + string(last)}, nil
}

func cancelled(ctx context.Context) bool {
	select {
	case <-ctx.Done():
		return true
	default:
		return false
	}
}

func successOutcome(messageID string) effect.Outcome {
	receipt, err := json.Marshal(sendReceipt{MessageID: messageID})
	if err != nil {
		return effect.Outcome{Ambiguous: true}
	}
	return effect.Outcome{Succeeded: true, Receipt: receipt}
}

func failureOutcome(err error) (effect.Outcome, ErrorCode) {
	code, coded := CodeOf(err)
	if !coded || code == ErrorCodeDeliveryAmbiguous {
		return effect.Outcome{Ambiguous: true}, ErrorCodeDeliveryAmbiguous
	}
	return effect.Outcome{SafeErrorCode: safeCodePrefix + string(code)}, code
}

func retryable(code ErrorCode) bool {
	return code == ErrorCodeConnectionFailed || code == ErrorCodeSubmissionDeferred
}
