package runtimeengine

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/anggasct/aura/internal/runtime"
	"github.com/anggasct/aura/internal/runtime/checkpoint"
	"github.com/anggasct/aura/internal/store"
)

func checkpointFixture(eventSequence uint64) *runtime.Checkpoint {
	return &runtime.Checkpoint{
		RunID:              "run-1",
		SessionID:          "session-1",
		TurnID:             "turn-1",
		OwnerID:            "user-1",
		PrincipalID:        "user-1",
		Phase:              "awaiting_approval",
		EventSequence:      eventSequence,
		InputCursor:        "cursor-1",
		PendingApprovalIDs: []string{"approval-1"},
		PendingToolCallIDs: []string{"call-1"},
		CapabilityDigest:   strings.Repeat("b", 64),
		PolicyVersion:      "policy-1",
		ResumeGeneration:   1,
		StateDigest:        strings.Repeat("a", 64),
	}
}

func validResumeValidation(checkpoint *runtime.Checkpoint, currentSequence uint64) *runtimecheckpoint.ResumeValidation {
	return &runtimecheckpoint.ResumeValidation{
		SessionID:            checkpoint.SessionID,
		TurnID:               checkpoint.TurnID,
		OwnerID:              "user-1",
		PrincipalID:          "user-1",
		CapabilityDigest:     checkpoint.CapabilityDigest,
		PolicyVersion:        checkpoint.PolicyVersion,
		CurrentEventSequence: currentSequence,
		CurrentStateDigest:   checkpoint.StateDigest,
		ResumeGeneration:     checkpoint.ResumeGeneration,
		ApprovalValid:        true,
		EffectStateValid:     true,
	}
}

func checkpointRuntime(t *testing.T) (*Engine, *sql.DB, store.EventStore, uint64) {
	t.Helper()
	engine, db, events := newTestRuntime(t, Config{}, runtime.NewFakeExecutor(nil))
	mustCreateSession(t, db, "session-1")
	if _, err := collect(t, engine, sampleRequest("session-1", "turn-1")); err != nil {
		t.Fatalf("seed turn: %v", err)
	}
	last, err := events.LastSequence(context.Background(), "session-1")
	if err != nil {
		t.Fatalf("last sequence: %v", err)
	}
	return engine, db, events, last
}

func TestSaveAndLoadCheckpoint(t *testing.T) {
	engine, db, eventStore, last := checkpointRuntime(t)
	checkpoint := checkpointFixture(last)
	if err := engine.SaveCheckpoint(context.Background(), checkpoint); err != nil {
		t.Fatalf("SaveCheckpoint: %v", err)
	}
	loaded, err := engine.LoadCheckpoint(context.Background(), checkpoint.TurnID)
	if err != nil {
		t.Fatalf("LoadCheckpoint: %v", err)
	}
	if !reflect.DeepEqual(loaded, *checkpoint) {
		t.Fatalf("loaded checkpoint = %+v, want %+v", loaded, *checkpoint)
	}
	currentSequence, err := eventStore.LastSequence(context.Background(), checkpoint.SessionID)
	if err != nil {
		t.Fatalf("last checkpoint sequence: %v", err)
	}
	if currentSequence <= checkpoint.EventSequence {
		t.Fatalf("checkpoint event sequence = %d, boundary = %d", currentSequence, checkpoint.EventSequence)
	}
	if _, err := engine.ValidateCheckpoint(context.Background(), checkpoint.TurnID, validResumeValidation(checkpoint, currentSequence)); err != nil {
		t.Fatalf("ValidateCheckpoint: %v", err)
	}
	next := *checkpoint
	next.EventSequence = currentSequence
	next.ResumeGeneration = 2
	if err := engine.SaveCheckpoint(context.Background(), &next); err != nil {
		t.Fatalf("SaveCheckpoint next generation: %v", err)
	}
	latest, err := engine.LoadCheckpoint(context.Background(), checkpoint.TurnID)
	if err != nil {
		t.Fatalf("LoadCheckpoint latest: %v", err)
	}
	if latest.ResumeGeneration != 2 || latest.EventSequence != currentSequence {
		t.Fatalf("latest checkpoint = %+v, want generation 2 at boundary %d", latest, currentSequence)
	}
	restarted, err := NewEngine(Config{}, store.NewEventStore(db), store.NewDedupeStore(db), runtime.NewFakeExecutor(nil), nil)
	if err != nil {
		t.Fatalf("NewEngine after restart: %v", err)
	}
	if _, err := restarted.LoadCheckpoint(context.Background(), checkpoint.TurnID); err != nil {
		t.Fatalf("LoadCheckpoint after restart: %v", err)
	}
}

func TestSaveCheckpointRejectsFutureBoundary(t *testing.T) {
	engine, _, _, last := checkpointRuntime(t)
	checkpoint := checkpointFixture(last + 1)
	if err := engine.SaveCheckpoint(context.Background(), checkpoint); err == nil {
		t.Fatal("SaveCheckpoint succeeded for a future event boundary")
	} else if code, ok := runtime.CodeOf(err); !ok || code != runtime.ErrorCodeCheckpointStale {
		t.Fatalf("CodeOf(%v) = %q, %v; want checkpoint_stale", err, code, ok)
	}
}

func TestSaveCheckpointRejectsMutatedRetry(t *testing.T) {
	engine, _, _, last := checkpointRuntime(t)
	checkpoint := checkpointFixture(last)
	if err := engine.SaveCheckpoint(context.Background(), checkpoint); err != nil {
		t.Fatalf("SaveCheckpoint: %v", err)
	}
	if err := engine.SaveCheckpoint(context.Background(), checkpoint); err != nil {
		t.Fatalf("SaveCheckpoint idempotent retry: %v", err)
	}
	mutated := *checkpoint
	mutated.StateDigest = strings.Repeat("c", 64)
	if err := engine.SaveCheckpoint(context.Background(), &mutated); err == nil {
		t.Fatal("SaveCheckpoint accepted a mutated retry")
	} else if code, ok := runtime.CodeOf(err); !ok || code != runtime.ErrorCodeCheckpointStale {
		t.Fatalf("CodeOf(%v) = %q, %v; want checkpoint_stale", err, code, ok)
	}
}

func TestConcurrentCheckpointMutationHasOneAuthoritativePayload(t *testing.T) {
	engine, db, _, last := checkpointRuntime(t)
	other, err := NewEngine(Config{}, store.NewEventStore(db), store.NewDedupeStore(db), runtime.NewFakeExecutor(nil), nil)
	if err != nil {
		t.Fatalf("NewEngine second writer: %v", err)
	}
	first := checkpointFixture(last)
	second := *first
	second.StateDigest = strings.Repeat("c", 64)
	start := make(chan struct{})
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	for _, runner := range []*Engine{engine, other} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			checkpoint := first
			if runner == other {
				checkpoint = &second
			}
			errs <- runner.SaveCheckpoint(context.Background(), checkpoint)
		}()
	}
	close(start)
	wg.Wait()
	close(errs)

	var successes, stale int
	for err := range errs {
		if err == nil {
			successes++
			continue
		}
		if code, ok := runtime.CodeOf(err); ok && code == runtime.ErrorCodeCheckpointStale {
			stale++
			continue
		}
		t.Fatalf("concurrent SaveCheckpoint error = %v", err)
	}
	if successes != 1 || stale != 1 {
		t.Fatalf("concurrent saves successes=%d stale=%d, want one each", successes, stale)
	}
	loaded, err := engine.LoadCheckpoint(context.Background(), first.TurnID)
	if err != nil {
		t.Fatalf("LoadCheckpoint: %v", err)
	}
	if loaded.StateDigest != first.StateDigest && loaded.StateDigest != second.StateDigest {
		t.Fatalf("loaded state digest = %q, want one authoritative payload", loaded.StateDigest)
	}
}

func TestValidateResumeRejectsChangedState(t *testing.T) {
	checkpoint := checkpointFixture(42)
	cases := []struct {
		name   string
		mutate func(*runtimecheckpoint.ResumeValidation)
		want   runtime.ErrorCode
	}{
		{name: "owner", mutate: func(v *runtimecheckpoint.ResumeValidation) { v.PrincipalID = "other" }, want: runtime.ErrorCodePolicyDenied},
		{name: "capability", mutate: func(v *runtimecheckpoint.ResumeValidation) { v.CapabilityDigest = strings.Repeat("c", 64) }, want: runtime.ErrorCodeCheckpointStale},
		{name: "policy", mutate: func(v *runtimecheckpoint.ResumeValidation) { v.PolicyVersion = "policy-2" }, want: runtime.ErrorCodeCheckpointStale},
		{name: "sequence", mutate: func(v *runtimecheckpoint.ResumeValidation) { v.CurrentEventSequence = 41 }, want: runtime.ErrorCodeCheckpointStale},
		{name: "state", mutate: func(v *runtimecheckpoint.ResumeValidation) { v.CurrentStateDigest = strings.Repeat("c", 64) }, want: runtime.ErrorCodeCheckpointStale},
		{name: "generation", mutate: func(v *runtimecheckpoint.ResumeValidation) { v.ResumeGeneration = 2 }, want: runtime.ErrorCodeCheckpointStale},
		{name: "approval", mutate: func(v *runtimecheckpoint.ResumeValidation) { v.ApprovalValid = false }, want: runtime.ErrorCodeCheckpointStale},
		{name: "effect", mutate: func(v *runtimecheckpoint.ResumeValidation) { v.EffectStateValid = false }, want: runtime.ErrorCodeCheckpointStale},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			validation := validResumeValidation(checkpoint, checkpoint.EventSequence)
			test.mutate(validation)
			if err := checkpoint.ValidateResume(validation); err == nil {
				t.Fatal("ValidateResume accepted invalid state")
			} else if code, ok := runtime.CodeOf(err); !ok || code != test.want {
				t.Fatalf("CodeOf(%v) = %q, %v; want %q", err, code, ok, test.want)
			}
		})
	}
}

func TestValidateResumeRejectsMatchingAttackerIdentity(t *testing.T) {
	checkpoint := checkpointFixture(42)
	validation := validResumeValidation(checkpoint, checkpoint.EventSequence)
	validation.OwnerID = "attacker"
	validation.PrincipalID = "attacker"
	if err := checkpoint.ValidateResume(validation); err == nil {
		t.Fatal("ValidateResume accepted an attacker-controlled identity pair")
	} else if code, ok := runtime.CodeOf(err); !ok || code != runtime.ErrorCodePolicyDenied {
		t.Fatalf("CodeOf(%v) = %q, %v; want policy_denied", err, code, ok)
	}
}

func TestCheckpointPayloadRejectsCorruption(t *testing.T) {
	checkpoint := checkpointFixture(4)
	payload, err := checkpoint.MarshalPayload()
	if err != nil {
		t.Fatalf("MarshalPayload: %v", err)
	}
	validEvent := func() store.RuntimeEvent {
		return store.RuntimeEvent{
			ID:            "event-1",
			SessionID:     checkpoint.SessionID,
			Sequence:      checkpoint.EventSequence + 1,
			TurnID:        checkpoint.TurnID,
			InvocationID:  checkpoint.RunID,
			Author:        "runtime",
			Kind:          runtimecheckpoint.EventKindRunCheckpoint,
			SchemaVersion: 1,
			Payload:       payload,
		}
	}
	cases := []struct {
		name   string
		mutate func(*store.RuntimeEvent)
		want   runtime.ErrorCode
	}{
		{name: "wrong kind", mutate: func(event *store.RuntimeEvent) { event.Kind = "unknown" }, want: runtime.ErrorCodeCheckpointUnsupported},
		{name: "wrong schema", mutate: func(event *store.RuntimeEvent) { event.SchemaVersion = 2 }, want: runtime.ErrorCodeCheckpointUnsupported},
		{name: "invalid json", mutate: func(event *store.RuntimeEvent) { event.Payload = json.RawMessage(`{"state":`) }, want: runtime.ErrorCodeCheckpointInvalid},
		{name: "identity mismatch", mutate: func(event *store.RuntimeEvent) { event.TurnID = "other" }, want: runtime.ErrorCodeCheckpointInvalid},
		{name: "boundary mismatch", mutate: func(event *store.RuntimeEvent) { event.Sequence = checkpoint.EventSequence }, want: runtime.ErrorCodeCheckpointInvalid},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			event := validEvent()
			test.mutate(&event)
			if _, err := runtime.ParseCheckpoint(&event); err == nil {
				t.Fatal("ParseCheckpoint accepted corrupted event")
			} else if code, ok := runtime.CodeOf(err); !ok || code != test.want {
				t.Fatalf("CodeOf(%v) = %q, %v; want %q", err, code, ok, test.want)
			}
		})
	}
}

func TestCheckpointGenerationAdvancesExactlyOnce(t *testing.T) {
	checkpoint := checkpointFixture(4)
	next, err := checkpoint.NextResumeGeneration()
	if err != nil {
		t.Fatalf("NextResumeGeneration: %v", err)
	}
	if next != 2 {
		t.Fatalf("next generation = %d, want 2", next)
	}
	if checkpoint.ResumeGeneration != 1 {
		t.Fatalf("checkpoint generation mutated to %d", checkpoint.ResumeGeneration)
	}
}

type resumeEffectValidator struct {
	err   error
	calls int
}

func (v *resumeEffectValidator) ValidateResumeEffects(context.Context, string, string, []string) error {
	v.calls++
	return v.err
}

func TestValidateCheckpointWithEffectsRequiresRecovery(t *testing.T) {
	engine, _, _, last := checkpointRuntime(t)
	checkpoint := checkpointFixture(last)
	if err := engine.SaveCheckpoint(context.Background(), checkpoint); err != nil {
		t.Fatalf("SaveCheckpoint: %v", err)
	}
	validator := &resumeEffectValidator{}
	validation := validResumeValidation(checkpoint, last+1)
	validation.EffectStateValid = false
	if _, err := engine.ValidateCheckpointWithEffects(context.Background(), checkpoint.TurnID, validation, validator); err != nil {
		t.Fatalf("ValidateCheckpointWithEffects: %v", err)
	}
	if validator.calls != 1 {
		t.Fatalf("effect validator calls = %d, want 1", validator.calls)
	}
	validator.err = errors.New("effect recovery unavailable")
	if _, err := engine.ValidateCheckpointWithEffects(context.Background(), checkpoint.TurnID, validation, validator); err == nil {
		t.Fatal("ValidateCheckpointWithEffects accepted failed effect recovery")
	} else if code, ok := runtime.CodeOf(err); !ok || code != runtime.ErrorCodeCheckpointStale {
		t.Fatalf("CodeOf(%v) = %q, %v; want checkpoint_stale", err, code, ok)
	}
}
