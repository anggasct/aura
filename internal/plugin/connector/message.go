package connector

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"regexp"
	"unicode"
)

const (
	Version = "aura.connector/v1"

	typeDescribe   = "describe"
	typeInvoke     = "invoke"
	typeHTTPResult = "http_result"
	typeTools      = "tools"
	typeHTTP       = "http"
	typeResult     = "result"
	typeError      = "error"

	maxBodyBytes       = 1 << 20
	maxHTTPSteps       = 16
	maxHeaders         = 32
	maxHeaderValue     = 4096
	maxURLBytes        = 4096
	maxDescribedTools  = 64
	maxDescriptionSize = 1024
	maxSchemaBytes     = 64 << 10
)

var (
	reportedCodePattern = regexp.MustCompile(`^[a-z_]{1,48}$`)
	headerNamePattern   = regexp.MustCompile(`^[A-Za-z0-9-]{1,64}$`)
	toolNamePattern     = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`)
	errTrailingData     = errors.New("trailing data after the message")
)

type describeMessage struct {
	V    string `json:"v"`
	Type string `json:"type"`
}

type invokeMessage struct {
	V              string          `json:"v"`
	Type           string          `json:"type"`
	Call           string          `json:"call"`
	Tool           string          `json:"tool"`
	Arguments      json.RawMessage `json:"arguments"`
	IdempotencyKey string          `json:"idempotency_key"`
}

type httpResultMessage struct {
	V       string            `json:"v"`
	Type    string            `json:"type"`
	Call    string            `json:"call"`
	Status  int               `json:"status,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
	BodyB64 string            `json:"body_b64,omitempty"`
	Error   string            `json:"error,omitempty"`
}

type envelope struct {
	V    string `json:"v"`
	Type string `json:"type"`
}

type describedTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
}

type toolsMessage struct {
	V     string          `json:"v"`
	Type  string          `json:"type"`
	Tools []describedTool `json:"tools"`
}

type httpMessage struct {
	V       string            `json:"v"`
	Type    string            `json:"type"`
	Call    string            `json:"call"`
	Method  string            `json:"method"`
	URL     string            `json:"url"`
	Headers map[string]string `json:"headers"`
	BodyB64 string            `json:"body_b64"`
}

type resultMessage struct {
	V      string          `json:"v"`
	Type   string          `json:"type"`
	Call   string          `json:"call"`
	Output json.RawMessage `json:"output"`
}

type errorMessage struct {
	V    string `json:"v"`
	Type string `json:"type"`
	Call string `json:"call"`
	Code string `json:"code"`
}

func decodeStrict(raw []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return errTrailingData
	}
	return nil
}

func peek(raw []byte) (string, *Error) {
	var head envelope
	if err := json.Unmarshal(raw, &head); err != nil {
		return "", Errorf(ErrRPCInvalid, "the connector sent a message that is not a json object")
	}
	if head.V != Version {
		return "", Errorf(ErrRPCInvalid, "the connector sent a message with the wrong version")
	}
	return head.Type, nil
}

func invalid() *Error {
	return Errorf(ErrRPCInvalid, "the connector sent a message that does not match the contract")
}

func decodeTools(raw []byte) ([]describedTool, *Error) {
	kind, fail := peek(raw)
	if fail != nil {
		return nil, fail
	}
	var message toolsMessage
	if kind != typeTools || decodeStrict(raw, &message) != nil {
		return nil, invalid()
	}
	if len(message.Tools) > maxDescribedTools {
		return nil, Errorf(ErrRPCInvalid, "the connector described too many tools")
	}
	seen := make(map[string]bool, len(message.Tools))
	for i := range message.Tools {
		tool := &message.Tools[i]
		if !toolNamePattern.MatchString(tool.Name) || seen[tool.Name] || len(tool.Description) > maxDescriptionSize || len(tool.InputSchema) > maxSchemaBytes {
			return nil, invalid()
		}
		if len(tool.InputSchema) > 0 && !validSchemaObject(tool.InputSchema) {
			return nil, invalid()
		}
		seen[tool.Name] = true
	}
	return message.Tools, nil
}

func validSchemaObject(schema json.RawMessage) bool {
	var object map[string]json.RawMessage
	return json.Unmarshal(schema, &object) == nil && object != nil
}

type reply struct {
	http   *httpMessage
	result json.RawMessage
	code   string
}

func decodeReply(raw []byte, call string, maxOutput int64) (*reply, *Error) {
	kind, fail := peek(raw)
	if fail != nil {
		return nil, fail
	}
	switch kind {
	case typeHTTP:
		var message httpMessage
		if decodeStrict(raw, &message) != nil || message.Call != call {
			return nil, invalid()
		}
		return &reply{http: &message}, nil
	case typeResult:
		var message resultMessage
		if decodeStrict(raw, &message) != nil || message.Call != call || len(bytes.TrimSpace(message.Output)) == 0 || !json.Valid(message.Output) {
			return nil, invalid()
		}
		if int64(len(message.Output)) > maxOutput {
			return nil, Errorf(ErrRPCLimit, "the connector output exceeds the message bound")
		}
		return &reply{result: message.Output}, nil
	case typeError:
		var message errorMessage
		if decodeStrict(raw, &message) != nil || message.Call != call || !reportedCodePattern.MatchString(message.Code) {
			return nil, invalid()
		}
		return &reply{code: message.Code}, nil
	default:
		return nil, invalid()
	}
}

func decodeBody(encoded string) ([]byte, *Error) {
	if base64.StdEncoding.DecodedLen(len(encoded)) > maxBodyBytes+2 {
		return nil, Errorf(ErrRPCLimit, "the request body exceeds the size bound")
	}
	body, err := base64.StdEncoding.Strict().DecodeString(encoded)
	if err != nil {
		return nil, invalid()
	}
	if len(body) > maxBodyBytes {
		return nil, Errorf(ErrRPCLimit, "the request body exceeds the size bound")
	}
	return body, nil
}

func validHeaderValue(value string) bool {
	if len(value) > maxHeaderValue {
		return false
	}
	for _, r := range value {
		if r == '\t' {
			continue
		}
		if unicode.IsControl(r) || r > unicode.MaxASCII {
			return false
		}
	}
	return true
}
