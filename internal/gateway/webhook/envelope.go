package webhook

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"unicode"
	"unicode/utf8"
)

const (
	maxEventIDChars       = 200
	maxSubjectChars       = 500
	maxMetadataEntries    = 32
	maxMetadataKeyChars   = 128
	maxMetadataValueChars = 512
)

// Envelope is the strict event body. Payload and metadata stay untrusted
// external input: they are carried, never executed or interpolated.
type Envelope struct {
	EventID  string            `json:"event_id"`
	Subject  string            `json:"subject"`
	Payload  json.RawMessage   `json:"payload"`
	Metadata map[string]string `json:"metadata"`
}

// ParseEnvelope strictly decodes a bounded, already-authenticated body.
// Unknown top-level fields, a non-object payload, and every documented
// length bound are enforced here so nothing downstream re-validates.
func ParseEnvelope(body []byte) (Envelope, error) {
	var envelope Envelope
	// One byte past the body proves the decoder consumed exactly the input
	// and never allocated beyond the request bound already enforced upstream.
	decoder := json.NewDecoder(io.LimitReader(bytes.NewReader(body), int64(len(body))+1))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&envelope); err != nil {
		return Envelope{}, Errorf(ErrorCodeInvalidRequest, "body is not a valid event envelope")
	}
	if decoder.More() {
		return Envelope{}, Errorf(ErrorCodeInvalidRequest, "body contains trailing content")
	}
	if err := validateEnvelope(&envelope); err != nil {
		return Envelope{}, err
	}
	return envelope, nil
}

func validateEnvelope(envelope *Envelope) error {
	var problems []error
	if count := utf8.RuneCountInString(envelope.EventID); count < 1 || count > maxEventIDChars || containsControl(envelope.EventID) {
		problems = append(problems, fmt.Errorf("event_id must be 1-%d visible characters", maxEventIDChars))
	}
	if count := utf8.RuneCountInString(envelope.Subject); count < 1 || count > maxSubjectChars {
		problems = append(problems, fmt.Errorf("subject must be 1-%d characters", maxSubjectChars))
	}
	if !isObject(envelope.Payload) {
		problems = append(problems, errors.New("payload must be a JSON object"))
	}
	if len(envelope.Metadata) > maxMetadataEntries {
		problems = append(problems, fmt.Errorf("metadata must have at most %d entries", maxMetadataEntries))
	}
	for key, value := range envelope.Metadata {
		if utf8.RuneCountInString(key) > maxMetadataKeyChars {
			problems = append(problems, fmt.Errorf("metadata keys must be at most %d characters", maxMetadataKeyChars))
			break
		}
		if utf8.RuneCountInString(value) > maxMetadataValueChars {
			problems = append(problems, fmt.Errorf("metadata values must be at most %d characters", maxMetadataValueChars))
			break
		}
	}
	if err := errors.Join(problems...); err != nil {
		return Errorf(ErrorCodeInvalidRequest, "%s", err.Error())
	}
	return nil
}

func isObject(raw json.RawMessage) bool {
	for _, b := range raw {
		if !isJSONSpace(b) {
			return b == '{'
		}
	}
	return false
}

func isJSONSpace(b byte) bool {
	return b == ' ' || b == '\t' || b == '\n' || b == '\r'
}

func containsControl(s string) bool {
	for _, r := range s {
		if unicode.IsControl(r) {
			return true
		}
	}
	return false
}
