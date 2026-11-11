package toolbroker

import (
	"encoding/json"
	"errors"
	"testing"
)

func TestToolOutcomeRecordRoundTrips(t *testing.T) {
	t.Run("result", func(t *testing.T) {
		encoded, err := encodeOutcomeResult(json.RawMessage(`{"output":{"a":1}}`))
		if err != nil {
			t.Fatalf("encode result: %v", err)
		}
		decoded, err := decodeOutcome(encoded)
		if err != nil {
			t.Fatalf("decode result: %v", err)
		}
		if string(decoded) != `{"output":{"a":1}}` {
			t.Fatalf("decoded result = %s, want the encoded one", decoded)
		}
	})
	t.Run("failure", func(t *testing.T) {
		encoded, err := encodeOutcomeFailure(ResultPolicyDenied, "policy_denied: denied")
		if err != nil {
			t.Fatalf("encode failure: %v", err)
		}
		decoded, err := decodeOutcome(encoded)
		var failure *Error
		if !errors.As(err, &failure) {
			t.Fatalf("decode failure = %s, %v, want a broker error", decoded, err)
		}
		if failure.Class != ResultPolicyDenied || failure.Detail != "policy_denied: denied" || failure.Cause != nil {
			t.Fatalf("decoded failure = %+v, want the encoded class and detail without a cause", failure)
		}
		if decoded != nil {
			t.Fatalf("decoded result of a failure = %s, want none", decoded)
		}
	})
	unreadable := map[string]string{
		"null":          `null`,
		"empty object":  `{}`,
		"array":         `[{"result":{}}]`,
		"both keys":     `{"result":{},"failure":{"class":"policy_denied","detail":"x"}}`,
		"unknown class": `{"failure":{"class":"made_up","detail":"x"}}`,
		"another key":   `{"outcome":{}}`,
	}
	for name, raw := range unreadable {
		t.Run(name, func(t *testing.T) {
			decoded, err := decodeOutcome([]byte(raw))
			if code, ok := CodeOf(err); !ok || code != ResultExecutionFailed {
				t.Fatalf("decode %s = %s, %v, want %s", raw, decoded, err, ResultExecutionFailed)
			}
		})
	}
}
