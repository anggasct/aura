package effect

import (
	"context"
	"encoding/json"
	"testing"
)

func TestValidateResumeEffectsClaimsStartedAndRejectsAmbiguous(t *testing.T) {
	j, _ := newTestJournal(t)
	intent := mustPrepare(t, j, validPrepare(1))
	mustStart(t, j, intent.ID)

	if err := j.ValidateResumeEffects(context.Background(), "sess-1", "turn-1", []string{intent.ToolCallID}); err == nil {
		t.Fatal("ValidateResumeEffects accepted an ambiguous effect")
	} else {
		assertCode(t, err, ErrorCodeUnknown)
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

func TestValidateResumeEffectsRejectsTerminalPendingEffects(t *testing.T) {
	j, _ := newTestJournal(t)
	succeeded := mustPrepare(t, j, validPrepare(2))
	mustStart(t, j, succeeded.ID)
	if _, err := j.Succeed(context.Background(), succeeded.ID, nil); err != nil {
		t.Fatalf("Succeed: %v", err)
	}
	failed := mustPrepare(t, j, validPrepare(3))
	mustStart(t, j, failed.ID)
	if _, err := j.Fail(context.Background(), failed.ID, "provider_failed"); err != nil {
		t.Fatalf("Fail: %v", err)
	}
	unknown := mustPrepare(t, j, validPrepare(4))
	mustStart(t, j, unknown.ID)
	if _, err := j.MarkUnknown(context.Background(), unknown.ID); err != nil {
		t.Fatalf("MarkUnknown: %v", err)
	}

	for _, test := range []struct {
		name string
		id   string
		want ErrorCode
	}{
		{name: "succeeded", id: succeeded.ToolCallID, want: ErrorCodeTransitionInvalid},
		{name: "failed", id: failed.ToolCallID, want: ErrorCodeTransitionInvalid},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := j.ValidateResumeEffects(context.Background(), "sess-1", "turn-1", []string{test.id}); err == nil {
				t.Fatal("ValidateResumeEffects accepted a terminal pending effect")
			} else {
				assertCode(t, err, test.want)
			}
		})
	}
	if err := j.ValidateResumeEffects(context.Background(), "sess-1", "turn-1", []string{unknown.ToolCallID}); err == nil {
		t.Fatal("ValidateResumeEffects accepted an unreconciled effect")
	} else {
		assertCode(t, err, ErrorCodeUnknown)
	}
	if _, err := j.Resolve(context.Background(), unknown.ID, Resolution{Succeeded: true, Receipt: json.RawMessage(`{"provider_id":"p-1"}`)}); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if err := j.ValidateResumeEffects(context.Background(), "sess-1", "turn-1", []string{unknown.ToolCallID}); err == nil {
		t.Fatal("ValidateResumeEffects accepted a resolved effect as pending")
	} else {
		assertCode(t, err, ErrorCodeTransitionInvalid)
	}
}
