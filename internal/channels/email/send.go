package email

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"time"

	"github.com/anggasct/aura/internal/config"
	"github.com/anggasct/aura/internal/platform/principal"
)

const (
	freshKeyPrefix = "tool:"
	randomIDBytes  = 16
)

type Outgoing struct {
	SessionID      string
	IdempotencyKey string
	To             string
	Subject        string
	Body           string
}

func (a *Adapter) Send(ctx context.Context, outgoing *Outgoing) (string, error) {
	if outgoing == nil {
		return "", Errorf(ErrorCodeInvalidArgument, "outgoing message must not be nil")
	}
	if outgoing.SessionID == "" || outgoing.IdempotencyKey == "" {
		return "", Errorf(ErrorCodeInvalidArgument, "outgoing message is missing its session or idempotency key")
	}
	body := cleanBody(outgoing.Body)
	if !a.recipientAllowed(outgoing.To) || body == "" || len(body) > MaxReplyBodyBytes {
		return "", Errorf(ErrorCodeMessageInvalid, "outgoing recipient or body is not usable")
	}
	select {
	case a.deliverSem <- struct{}{}:
	case <-ctx.Done():
		return "", ctx.Err()
	}
	defer func() { <-a.deliverSem }()
	base := freshKeyPrefix + outgoing.IdempotencyKey
	operation := sendOperation{
		To:        outgoing.To,
		Subject:   freshSubject(outgoing.Subject),
		MessageID: outboundMessageID(base, domainOf(a.account.Address)),
		Body:      body,
	}
	return a.executeSend(ctx, &sendAttempt{
		session:   outgoing.SessionID,
		operation: OpSendMessage,
		base:      base,
		bind:      "|" + operation.To + "|" + operation.Subject,
	}, &operation)
}

type TextReply struct {
	To     string
	Thread ThreadReference
	Text   string
}

func DeliverText(ctx context.Context, account *config.EmailAccount, reply *TextReply) error {
	if account == nil || reply == nil {
		return Errorf(ErrorCodeInvalidArgument, "email account and reply must not be nil")
	}
	mailer, err := NewSMTPMailer(account)
	if err != nil {
		return err
	}
	return deliverText(ctx, mailer, account.Address, reply, time.Now)
}

func deliverText(ctx context.Context, mailer Mailer, from string, reply *TextReply, now func() time.Time) error {
	body := cleanBody(reply.Text)
	if !principal.IsPlainEmailAddress(reply.To) || reply.To == from || body == "" || len(body) > MaxReplyBodyBytes {
		return Errorf(ErrorCodeMessageInvalid, "reply recipient or body is not usable")
	}
	var raw [randomIDBytes]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return &Error{Code: ErrorCodeDeliveryFailed, Detail: "message id is not generatable", Cause: err}
	}
	message := outboundMessage{
		From:      from,
		To:        reply.To,
		Subject:   reply.Thread.Subject,
		MessageID: outboundMessageID(hex.EncodeToString(raw[:]), domainOf(from)),
		Body:      body,
		Date:      now(),
	}
	if headerSafeID(reply.Thread.MessageID) {
		message.InReplyTo = reply.Thread.MessageID
		message.References = []string{reply.Thread.MessageID}
	}
	rendered, err := message.render()
	if err != nil {
		return err
	}
	outcome, failed := attemptSubmit(ctx, mailer, &Submission{From: from, To: reply.To, Message: rendered}, message.MessageID, func(attempt int) time.Duration {
		return backoffBase << min(attempt, 8)
	})
	switch {
	case outcome.Succeeded:
		return nil
	case outcome.Ambiguous:
		return Errorf(ErrorCodeDeliveryAmbiguous, "delivery outcome is unknown")
	case failed != "":
		return Errorf(failed, "reply was not delivered")
	default:
		return Errorf(ErrorCodeDeliveryFailed, "reply was not delivered")
	}
}

func ParseThreadReference(raw string) ThreadReference {
	var thread ThreadReference
	if err := json.Unmarshal([]byte(raw), &thread); err != nil {
		return ThreadReference{}
	}
	return thread
}
