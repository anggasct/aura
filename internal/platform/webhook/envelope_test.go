package webhook

import (
	"fmt"
	"strings"
	"testing"

	"github.com/anggasct/aura/internal/platform/fault"
)

func TestParseEnvelope(t *testing.T) {
	t.Parallel()

	t.Run("valid envelope", func(t *testing.T) {
		t.Parallel()
		raw := []byte(`{"event_id":"evt-1","subject":"deploy","payload":{"env":"prod"},"metadata":{"source":"ci"}}`)
		env, err := ParseEnvelope(raw)
		if err != nil {
			t.Fatalf("ParseEnvelope: %v", err)
		}
		if env.EventID != "evt-1" || env.Subject != "deploy" || env.Metadata["source"] != "ci" {
			t.Fatalf("envelope = %+v", env)
		}
	})

	invalid := map[string]string{
		"unknown field":    `{"event_id":"evt-1","subject":"deploy","payload":{},"extra":true}`,
		"trailing content": `{"event_id":"evt-1","subject":"deploy","payload":{}}{"x":1}`,
		"empty event_id":   `{"event_id":"","subject":"deploy","payload":{}}`,
		"control event_id": `{"event_id":"evt\n1","subject":"deploy","payload":{}}`,
		"long event_id":    fmt.Sprintf(`{"event_id":%q,"subject":"deploy","payload":{}}`, strings.Repeat("a", maxEventIDChars+1)),
		"empty subject":    `{"event_id":"evt-1","subject":"","payload":{}}`,
		"array payload":    `{"event_id":"evt-1","subject":"deploy","payload":[]}`,
		"null payload":     `{"event_id":"evt-1","subject":"deploy","payload":null}`,
	}
	for name, body := range invalid {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := ParseEnvelope([]byte(body))
			if !fault.HasCode(err, ErrorCodeInvalidRequest) {
				t.Fatalf("ParseEnvelope(%s) error = %v, want %s", name, err, ErrorCodeInvalidRequest)
			}
		})
	}
}
