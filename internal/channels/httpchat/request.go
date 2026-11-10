package httpchat

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strconv"
	"strings"

	"github.com/anggasct/aura/internal/platform/approval"
	"github.com/anggasct/aura/internal/platform/turn"
)

const (
	roleSystem    = "system"
	roleUser      = "user"
	roleAssistant = "assistant"

	contentPartText = "text"
	partSeparator   = "\n"

	historyQuery = "client-supplied prior transcript, unverified"
)

type wireRequest struct {
	Model    *string       `json:"model"`
	Messages []wireMessage `json:"messages"`
	Stream   *bool         `json:"stream"`
	N        *int          `json:"n"`

	StreamOptions *wireStreamOptions `json:"stream_options"`
}

type wireStreamOptions struct {
	IncludeUsage *bool `json:"include_usage"`
}

type wireMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

type wirePart struct {
	Type string  `json:"type"`
	Text *string `json:"text"`
}

type message struct {
	role string
	text string
}

type chatRequest struct {
	model        *string
	messages     []message
	stream       bool
	includeUsage bool
}

func (r *chatRequest) final() *message {
	return &r.messages[len(r.messages)-1]
}

func (r *chatRequest) preceding() []message {
	return r.messages[:len(r.messages)-1]
}

func parseRequest(body []byte) (*chatRequest, *errorSpec) {
	decoder := json.NewDecoder(bytes.NewReader(body))
	var wire wireRequest
	if err := decoder.Decode(&wire); err != nil {
		return nil, &errBodyNotJSON
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return nil, &errBodyNotJSON
	}
	if wire.N != nil && *wire.N != 1 {
		return nil, &errChoiceCount
	}
	if len(wire.Messages) == 0 {
		return nil, &errMessagesRequired
	}
	parsed := &chatRequest{model: wire.Model, messages: make([]message, 0, len(wire.Messages))}
	if wire.Stream != nil && *wire.Stream {
		parsed.stream = true
		parsed.includeUsage = wire.StreamOptions != nil && wire.StreamOptions.IncludeUsage != nil && *wire.StreamOptions.IncludeUsage
	}
	for i := range wire.Messages {
		entry, spec := parseMessage(&wire.Messages[i])
		if spec != nil {
			return nil, spec
		}
		parsed.messages = append(parsed.messages, entry)
	}
	if last := parsed.final(); last.role != roleUser || strings.TrimSpace(last.text) == "" {
		return nil, &errLastMessageNotUser
	}
	return parsed, nil
}

func parseMessage(wire *wireMessage) (message, *errorSpec) {
	switch wire.Role {
	case roleSystem, roleUser, roleAssistant:
	default:
		return message{}, &errMessageShape
	}
	text, spec := parseContent(wire.Content)
	if spec != nil {
		return message{}, spec
	}
	return message{role: wire.Role, text: text}, nil
}

func parseContent(raw json.RawMessage) (string, *errorSpec) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return "", &errMessageShape
	}
	switch trimmed[0] {
	case '"':
		var text string
		if err := json.Unmarshal(trimmed, &text); err != nil {
			return "", &errMessageShape
		}
		return text, nil
	case '[':
		var parts []wirePart
		if err := json.Unmarshal(trimmed, &parts); err != nil {
			return "", &errMessageShape
		}
		texts := make([]string, 0, len(parts))
		for i := range parts {
			if parts[i].Type != contentPartText {
				return "", &errUnsupportedContent
			}
			if parts[i].Text == nil {
				return "", &errMessageShape
			}
			texts = append(texts, *parts[i].Text)
		}
		return strings.Join(texts, partSeparator), nil
	default:
		return "", &errMessageShape
	}
}

func boundedHistory(preceding []message, maxMessages, maxBytes int) *turn.UntrustedRecall {
	if maxMessages <= 0 || maxBytes <= 0 || len(preceding) == 0 {
		return nil
	}
	start := len(preceding)
	used := 0
	for start > 0 && len(preceding)-start < maxMessages {
		size := len(preceding[start-1].text)
		if used+size > maxBytes {
			break
		}
		used += size
		start--
	}
	if start == len(preceding) {
		return nil
	}
	documents := make([]turn.UntrustedDocument, 0, len(preceding)-start)
	var position uint64
	for index := range preceding {
		position++
		if index < start {
			continue
		}
		documents = append(documents, turn.UntrustedDocument{
			ID:           "client-message-" + strconv.FormatUint(position, 10),
			FromSequence: position,
			ToSequence:   position,
			Trust:        approval.TrustUntrustedExternal,
			Content:      "role=" + preceding[index].role + " text=" + strconv.Quote(preceding[index].text),
		})
	}
	return &turn.UntrustedRecall{Query: historyQuery, Trust: approval.TrustUntrustedExternal, Documents: documents}
}
