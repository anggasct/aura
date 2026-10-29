package runtimeengine

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/anggasct/aura/internal/principal"
	"github.com/anggasct/aura/internal/runtime/sessions"
)

func TestEnvelopeClassIsParsedFailClosed(t *testing.T) {
	for raw, want := range map[string]principal.Class{
		"owner":   principal.ClassOwner,
		"trusted": principal.ClassTrusted,
		"":        principal.ClassTrusted,
		"root":    principal.ClassTrusted,
	} {
		env := sampleEnvelope("conv-1", "msg-1")
		env.PrincipalClass = raw
		req, err := turnRequestFromEnvelope(env)
		if err != nil {
			t.Fatalf("turnRequestFromEnvelope(%q): %v", raw, err)
		}
		if req.PrincipalClass != want {
			t.Errorf("class from envelope %q = %q, want %q", raw, req.PrincipalClass, want)
		}
	}
}

func TestDurableReplayReadsClassFromDescriptor(t *testing.T) {
	req := sampleRequest("session-a", "turn-0")
	req.PrincipalID = "discord:111"
	req.PrincipalClass = principal.ClassTrusted
	desc := turnDescriptorFromRequest(req)
	payload, err := json.Marshal(desc)
	if err != nil {
		t.Fatalf("marshal descriptor: %v", err)
	}
	if !strings.Contains(string(payload), `"principal_class":"trusted"`) {
		t.Fatalf("descriptor payload does not persist the class: %s", payload)
	}
	var stored runtimesessions.Descriptor
	if err := json.Unmarshal(payload, &stored); err != nil {
		t.Fatalf("unmarshal descriptor: %v", err)
	}
	if replayed := turnRequestFromDescriptor(&stored); replayed.PrincipalClass != principal.ClassTrusted || replayed.PrincipalID != "discord:111" {
		t.Fatalf("replayed principal = %q/%q, want discord:111/trusted", replayed.PrincipalID, replayed.PrincipalClass)
	}
}

func TestLegacyDescriptorWithoutClassReplaysAsTrusted(t *testing.T) {
	legacy := []byte(`{"turn_id":"turn-0","session_id":"session-a","principal_id":"local-owner","origin":"discord:owner","parts":[{"text":"hi"}],"idempotency_key":"k","max_tokens":0,"max_cost":0,"trace_parent":"","agent_id":""}`)
	var desc runtimesessions.Descriptor
	if err := json.Unmarshal(legacy, &desc); err != nil {
		t.Fatalf("unmarshal legacy descriptor: %v", err)
	}
	if replayed := turnRequestFromDescriptor(&desc); replayed.PrincipalClass != principal.ClassTrusted {
		t.Fatalf("legacy descriptor class = %q, want trusted", replayed.PrincipalClass)
	}
}

func TestDescriptorWithoutClassKeepsLegacyEncoding(t *testing.T) {
	desc := turnDescriptorFromRequest(sampleRequest("session-a", "turn-0"))
	payload, err := json.Marshal(desc)
	if err != nil {
		t.Fatalf("marshal descriptor: %v", err)
	}
	if strings.Contains(string(payload), "principal_class") {
		t.Fatalf("classless request changed the descriptor encoding: %s", payload)
	}
}
