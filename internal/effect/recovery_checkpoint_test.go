package effect

import (
	"context"
	"testing"
)

func TestValidateResumeEffectsClaimsStartedAndRejectsMissing(t *testing.T) {
	j, _ := newTestJournal(t)
	intent := mustPrepare(t, j, validPrepare(1))
	mustStart(t, j, intent.ID)

	if err := j.ValidateResumeEffects(context.Background(), "sess-1", "turn-1", []string{intent.ToolCallID}); err != nil {
		t.Fatalf("ValidateResumeEffects: %v", err)
	}
	unknown, err := j.Get(context.Background(), intent.ID)
	if err != nil {
		t.Fatalf("Get after resume validation: %v", err)
	}
	if unknown.State != StateUnknown {
		t.Fatalf("state after resume validation = %q, want unknown", unknown.State)
	}
	if err := j.ValidateResumeEffects(context.Background(), "sess-1", "turn-1", []string{"missing"}); err == nil {
		t.Fatal("ValidateResumeEffects accepted a missing tool call")
	} else {
		assertCode(t, err, ErrorCodeNotFound)
	}
}
