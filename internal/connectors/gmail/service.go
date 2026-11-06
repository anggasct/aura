package gmail

import (
	"bytes"
	"context"
	"encoding/json"
	"io"

	"github.com/anggasct/aura/internal/connectors/wire"
)

type Service struct{}

func New() *Service {
	return &Service{}
}

func (s *Service) Invoke(ctx context.Context, call *wire.Call, do wire.Do) (json.RawMessage, error) {
	api := &client{do: do}
	switch call.Tool {
	case toolSearch:
		return search(ctx, api, call.Arguments)
	case toolRead:
		return read(ctx, api, call.Arguments)
	case toolDraft:
		return compose(ctx, api, call, false)
	case toolSend:
		return compose(ctx, api, call, true)
	}
	return nil, fail(CodeUnknownTool)
}

func decodeArguments(raw json.RawMessage, target any, allowed ...string) error {
	if len(bytes.TrimSpace(raw)) == 0 {
		raw = json.RawMessage(`{}`)
	}
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(raw, &keys); err != nil || keys == nil {
		return fail(CodeInvalidArgument)
	}
	for key := range keys {
		if !containsString(allowed, key) {
			return fail(CodeInvalidArgument)
		}
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return fail(CodeInvalidArgument)
	}
	if _, err := decoder.Token(); err != io.EOF {
		return fail(CodeInvalidArgument)
	}
	return nil
}

func containsString(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

func marshalOutput(value any) (json.RawMessage, error) {
	var out bytes.Buffer
	encoder := json.NewEncoder(&out)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return nil, fail(CodeUpstream)
	}
	return bytes.TrimSpace(out.Bytes()), nil
}
