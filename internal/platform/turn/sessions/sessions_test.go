package runtimesessions

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/anggasct/aura/internal/platform/turn"
)

func testDescriptor(turnID string) *Descriptor {
	return &Descriptor{
		TurnID:         turnID,
		SessionID:      "session-1",
		PrincipalID:    "owner",
		Origin:         "terminal",
		Parts:          []Part{{Text: "hello"}},
		IdempotencyKey: "key-" + turnID,
	}
}

func codeOf(t *testing.T, err error) turn.ErrorCode {
	t.Helper()
	if err == nil {
		t.Fatal("expected an error")
	}
	code, ok := turn.CodeOf(err)
	if !ok {
		t.Fatalf("expected a coded error, got %v", err)
	}
	return code
}

func TestDescriptorCarriesTheSkillPin(t *testing.T) {
	desc := testDescriptor("turn-1")
	desc.Skill = &turn.SkillPin{SkillID: "local/pdf@0123456789ab", Digest: "d", InvocationID: "i", PolicyVersion: "1"}
	raw, err := json.Marshal(desc)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded Descriptor
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if decoded.Skill == nil || *decoded.Skill != *desc.Skill || decoded.Validate() != nil {
		t.Fatalf("decoded = %+v", decoded.Skill)
	}
	plain, err := json.Marshal(testDescriptor("turn-2"))
	if err != nil {
		t.Fatalf("marshal plain: %v", err)
	}
	if strings.Contains(string(plain), `"skill"`) {
		t.Fatalf("unpinned descriptor carries a skill field: %s", plain)
	}
	for name, pin := range map[string]turn.SkillPin{
		"no skill":   {Digest: "d", InvocationID: "i", PolicyVersion: "1"},
		"no digest":  {SkillID: "s", InvocationID: "i", PolicyVersion: "1"},
		"no invoke":  {SkillID: "s", Digest: "d", PolicyVersion: "1"},
		"no version": {SkillID: "s", Digest: "d", InvocationID: "i"},
	} {
		incomplete := *testDescriptor("turn-3")
		incomplete.Skill = &pin
		if code := codeOf(t, incomplete.Validate()); code != turn.ErrorCodeInvalidArgument {
			t.Errorf("%s: code = %s", name, code)
		}
	}
}
