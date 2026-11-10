package connector

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/url"
	"strings"
	"unicode/utf8"

	"github.com/anggasct/aura/internal/modules/plugin/wire"
)

type composeArguments struct {
	To        []string `json:"to"`
	Cc        []string `json:"cc"`
	Subject   string   `json:"subject"`
	Body      string   `json:"body"`
	ReplyToID *string  `json:"reply_to_id"`
}

type outgoingMessage struct {
	Raw      string `json:"raw"`
	ThreadID string `json:"threadId,omitempty"`
}

type draftRequest struct {
	Message outgoingMessage `json:"message"`
}

type messageRef struct {
	ID       string `json:"id"`
	ThreadID string `json:"threadId"`
}

type draftResponse struct {
	ID      string     `json:"id"`
	Message messageRef `json:"message"`
}

type sendOutput struct {
	Status    string `json:"status"`
	MessageID string `json:"message_id,omitempty"`
	ThreadID  string `json:"thread_id,omitempty"`
}

type draftOutput struct {
	Status    string `json:"status"`
	DraftID   string `json:"draft_id,omitempty"`
	MessageID string `json:"message_id,omitempty"`
	ThreadID  string `json:"thread_id,omitempty"`
}

type original struct {
	threadID   string
	messageID  string
	references []string
}

func compose(ctx context.Context, api *client, call *wire.Call, send bool) (json.RawMessage, error) {
	var args composeArguments
	if err := decodeArguments(call.Arguments, &args, "to", "cc", "subject", "body", "reply_to_id"); err != nil {
		return nil, err
	}
	if !validCompose(&args) || call.IdempotencyKey == "" {
		return nil, fail(CodeInvalidArgument)
	}
	outbound := &outgoing{to: args.To, cc: args.Cc, subject: args.Subject, body: args.Body, messageID: deriveMessageID(call.IdempotencyKey)}
	threadID := ""
	if args.ReplyToID != nil {
		parent, err := loadOriginal(ctx, api, *args.ReplyToID, args.Subject)
		if err != nil {
			return nil, err
		}
		threadID = parent.threadID
		outbound.inReplyTo = parent.messageID
		outbound.references = parent.references
	}
	encoded, err := outbound.encode()
	if err != nil {
		return nil, err
	}
	message := outgoingMessage{Raw: base64.URLEncoding.EncodeToString(encoded), ThreadID: threadID}
	if send {
		return sendMessage(ctx, api, &message)
	}
	return saveDraft(ctx, api, &message)
}

func sendMessage(ctx context.Context, api *client, message *outgoingMessage) (json.RawMessage, error) {
	body, err := api.post(ctx, "messages/send", message)
	if err != nil {
		return nil, err
	}
	var sent messageRef
	output := sendOutput{Status: "sent"}
	if json.Unmarshal(body, &sent) == nil {
		output.MessageID = validOrEmpty(sent.ID)
		output.ThreadID = validOrEmpty(sent.ThreadID)
	}
	return marshalOutput(&output)
}

func saveDraft(ctx context.Context, api *client, message *outgoingMessage) (json.RawMessage, error) {
	body, err := api.post(ctx, "drafts", &draftRequest{Message: *message})
	if err != nil {
		return nil, err
	}
	var saved draftResponse
	output := draftOutput{Status: "drafted"}
	if json.Unmarshal(body, &saved) == nil {
		output.DraftID = validOrEmpty(saved.ID)
		output.MessageID = validOrEmpty(saved.Message.ID)
		output.ThreadID = validOrEmpty(saved.Message.ThreadID)
	}
	return marshalOutput(&output)
}

func validOrEmpty(id string) string {
	if validID(id) {
		return id
	}
	return ""
}

func validCompose(args *composeArguments) bool {
	if len(args.To) == 0 || len(args.To)+len(args.Cc) > maxRecipients {
		return false
	}
	for _, address := range args.To {
		if !validAddress(address) {
			return false
		}
	}
	for _, address := range args.Cc {
		if !validAddress(address) {
			return false
		}
	}
	if args.ReplyToID != nil && !validID(*args.ReplyToID) {
		return false
	}
	return validSubject(args.Subject) && validBody(args.Body)
}

func validBody(body string) bool {
	if len(body) > maxComposeBody || !utf8.ValidString(body) {
		return false
	}
	for i := range len(body) {
		c := body[i]
		if (c < 0x20 && c != '\t' && c != '\r' && c != '\n') || c == 0x7f {
			return false
		}
	}
	return true
}

func loadOriginal(ctx context.Context, api *client, id, subject string) (*original, error) {
	query := url.Values{"format": {"metadata"}, "metadataHeaders": {"Message-ID", "References", "Subject"}}
	body, err := api.get(ctx, messagePath(id), query)
	if err != nil {
		return nil, err
	}
	fetched, err := decodeMessage(body)
	if err != nil {
		return nil, err
	}
	if fetched.ID != id || !validID(fetched.ThreadID) || fetched.Payload == nil {
		return nil, fail(CodeResponseInvalid)
	}
	headers := fetched.Payload.Headers
	if !sameSubject(subject, headerValue(headers, "Subject")) {
		return nil, fail(CodeSubjectMismatch)
	}
	messageIDs := headerValues(headers, "Message-ID")
	for i := range messageIDs {
		messageIDs[i] = strings.Trim(messageIDs[i], " ")
	}
	references := headerValues(headers, "References")
	if len(messageIDs) != 1 || len(references) > 1 || !validMessageID(messageIDs[0]) {
		return nil, fail(CodeReplyUnavailable)
	}
	chain := []string{}
	if len(references) == 1 {
		if hasControl(references[0]) {
			return nil, fail(CodeReplyUnavailable)
		}
		for _, token := range strings.Fields(references[0]) {
			if validMessageID(token) {
				chain = append(chain, token)
			}
		}
	}
	if len(chain) > maxReferences {
		chain = chain[len(chain)-maxReferences:]
	}
	if len(chain) == 0 || chain[len(chain)-1] != messageIDs[0] {
		chain = append(chain, messageIDs[0])
	}
	return &original{threadID: fetched.ThreadID, messageID: messageIDs[0], references: chain}, nil
}

func sameSubject(approved, parent string) bool {
	return normalizeSubject(approved) == normalizeSubject(decodeHeaderText(parent))
}

func normalizeSubject(subject string) string {
	subject = strings.ToLower(strings.Join(strings.Fields(subject), " "))
	for strings.HasPrefix(subject, "re:") {
		subject = strings.TrimSpace(strings.TrimPrefix(subject, "re:"))
	}
	return subject
}
