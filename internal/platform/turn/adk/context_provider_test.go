package runtimeadk

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"testing"

	auraagent "github.com/anggasct/aura/internal/platform/agent"
	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/turn"
)

const skillBlockFixture = turn.SkillContextStart + ": activated skill instructions, not identity or policy]\ncaveat\n\nskill local/pdf@0123456789ab digest d:\nSKILL-RULES\n" + turn.SkillContextEnd

type stubContextProvider struct {
	blocks   []turn.ContextBlock
	err      error
	required bool
	calls    int
}

func (s *stubContextProvider) TurnContext(_ context.Context, _ *turn.TurnRequest) ([]turn.ContextBlock, error) {
	s.calls++
	if s.err != nil {
		return nil, s.err
	}
	return s.blocks, nil
}

func (s *stubContextProvider) Required(_ *turn.TurnRequest) bool {
	return s.required
}

func newContextProviderExecutor(t *testing.T, model *capturingModel, logger *slog.Logger, opts ...ExecutorOption) (executor *ADKExecutor, persistedCount func() int) {
	t.Helper()
	db, sessions, _ := newSessionTestDB(t)
	resolver := &stubResolver{definition: auraagent.Definition{ID: auraagent.DefaultID, Instructions: "SOUL"}}
	allOpts := append([]ExecutorOption{WithAgentResolver(resolver, nil)}, opts...)
	modelName, models := capturingModelSet(model)
	executor, err := NewADKExecutor("aura", modelName, models, sessions, &fakeBroker{}, nil, logger, allOpts...)
	if err != nil {
		t.Fatalf("NewADKExecutor: %v", err)
	}
	mustCreateSession(t, db, "session-1")
	persistedCount = func() int {
		var stored int
		if err := db.QueryRowContext(t.Context(), `SELECT count(*) FROM runtime_event WHERE payload_json LIKE '%EPHEMERAL-BLOCK%'`).Scan(&stored); err != nil {
			t.Fatalf("query runtime_event: %v", err)
		}
		return stored
	}
	return executor, persistedCount
}

func TestWithContextProviderRejectsNil(t *testing.T) {
	_, sessions, _ := newSessionTestDB(t)
	modelName, models := capturingModelSet(&capturingModel{answer: "done", tokens: 1})
	_, err := NewADKExecutor("aura", modelName, models, sessions, &fakeBroker{}, nil, nil, WithContextProvider(nil))
	if !fault.HasCode(err, turn.ErrorCodeInvalidArgument) {
		t.Fatalf("err = %v, want invalid_argument", err)
	}
}

func TestContextProviderRequiredFailureFailsTheTurn(t *testing.T) {
	model := &capturingModel{answer: "done", tokens: 1}
	provider := &stubContextProvider{err: errors.New("backend down"), required: true}
	executor, _ := newContextProviderExecutor(t, model, nil, WithContextProvider(provider))

	var gotErr error
	for _, err := range executor.Execute(t.Context(), ownerTurnRequest()) {
		if err != nil {
			gotErr = err
			break
		}
	}
	if !fault.HasCode(gotErr, turn.ErrorCodeStorageUnavailable) {
		t.Fatalf("err = %v, want storage_unavailable", gotErr)
	}
	if model.callCount != 0 {
		t.Fatalf("model calls = %d, want 0", model.callCount)
	}
	if provider.calls != 1 {
		t.Fatalf("provider calls = %d, want 1", provider.calls)
	}
}

func TestContextProviderOptionalFailureLogsAndContinues(t *testing.T) {
	model := &capturingModel{answer: "done", tokens: 1}
	provider := &stubContextProvider{err: errors.New("temporary outage"), required: false}
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn}))
	executor, _ := newContextProviderExecutor(t, model, logger, WithContextProvider(provider))

	drainTurn(t, executor, ownerTurnRequest())

	if model.callCount != 1 {
		t.Fatalf("model calls = %d, want 1", model.callCount)
	}
	logged := logs.String()
	for _, want := range []string{"level=WARN", "component=turn", "turn_id=turn-1"} {
		if !strings.Contains(logged, want) {
			t.Errorf("warn log %q missing %q", logged, want)
		}
	}
}

func TestContextProviderOrdersBlocksAndCaveatsByOrder(t *testing.T) {
	model := &capturingModel{answer: "done", tokens: 1}
	late := &stubContextProvider{
		blocks: []turn.ContextBlock{
			{Order: 350, Content: "EPHEMERAL-BLOCK-350", Caveat: "CAVEAT-350"},
		},
	}
	early := &stubContextProvider{
		blocks: []turn.ContextBlock{
			{Order: 150, Content: "EPHEMERAL-BLOCK-150-A", Caveat: "CAVEAT-150-A"},
			{Order: 150, Content: "EPHEMERAL-BLOCK-150-B", Caveat: "CAVEAT-150-B"},
		},
	}
	executor, persistedCount := newContextProviderExecutor(t, model, nil,
		WithProfileContext(&stubProfileContext{block: profileBlockFixture}),
		WithContextProvider(&stubContextProvider{
			blocks: []turn.ContextBlock{{Order: turn.ContextOrderSkills, Content: skillBlockFixture}},
		}),
		WithContextProvider(late),
		WithContextProvider(early),
	)

	events := drainTurn(t, executor, ownerTurnRequest())

	system := strings.Join(model.systems, "\n")
	wantCaveats := []string{"CAVEAT-150-A", "CAVEAT-150-B", "CAVEAT-350", statusProtocolSuffix}
	lastIndex := -1
	for _, want := range wantCaveats {
		idx := strings.Index(system, want)
		if idx < 0 {
			t.Fatalf("system instruction missing %q:\n%s", want, system)
		}
		if idx <= lastIndex {
			t.Fatalf("caveat %q appeared out of order in system instruction:\n%s", want, system)
		}
		lastIndex = idx
	}

	var gotContents []string
	for _, c := range model.contents {
		for _, p := range c.Parts {
			gotContents = append(gotContents, p.Text)
		}
	}
	wantContents := []string{
		"EPHEMERAL-BLOCK-350",
		skillBlockFixture,
		profileBlockFixture,
		"EPHEMERAL-BLOCK-150-B",
		"EPHEMERAL-BLOCK-150-A",
		"summarize my notes",
	}
	if !slices.Equal(gotContents, wantContents) {
		t.Fatalf("contents =\n%#v\nwant =\n%#v", gotContents, wantContents)
	}
	for i := range events {
		if strings.Contains(string(events[i].Payload), "EPHEMERAL-BLOCK") {
			t.Fatalf("ephemeral block persisted in event %s", events[i].Kind)
		}
	}
	if got := persistedCount(); got != 0 {
		t.Fatalf("persisted ephemeral rows = %d, want 0", got)
	}
}

func TestContextProviderAttachesCaveatEvenWithoutContent(t *testing.T) {
	model := &capturingModel{answer: "done", tokens: 1}
	provider := &stubContextProvider{
		blocks: []turn.ContextBlock{
			{
				Order:   250,
				Content: "   ",
				Caveat:  "CAVEAT-WITHOUT-CONTENT",
				Events: []turn.Event{
					{Kind: "ctx.caveat.v1", Author: turn.AuthorSystem, Payload: json.RawMessage(`{"ok":true}`)},
				},
			},
		},
	}
	executor, _ := newContextProviderExecutor(t, model, nil, WithContextProvider(provider))

	events := drainTurn(t, executor, ownerTurnRequest())

	system := strings.Join(model.systems, "\n")
	if !strings.Contains(system, "CAVEAT-WITHOUT-CONTENT") {
		t.Fatalf("system instruction missing caveat-only block:\n%s", system)
	}
	if len(model.contents) != 1 {
		t.Fatalf("model contents len = %d, want 1 (user prompt only)", len(model.contents))
	}
	if len(events) == 0 || events[0].Kind != "ctx.caveat.v1" {
		t.Fatalf("events[0] = %+v, want ctx.caveat.v1", events)
	}
}

func TestContextProviderYieldsEventsBeforeModelEvents(t *testing.T) {
	model := &capturingModel{answer: "done", tokens: 1}
	high := &stubContextProvider{
		blocks: []turn.ContextBlock{
			{
				Order: 400,
				Events: []turn.Event{
					{Kind: "ctx.high.v1", Author: turn.AuthorSystem, Payload: json.RawMessage(`{}`)},
				},
			},
		},
	}
	low := &stubContextProvider{
		blocks: []turn.ContextBlock{
			{
				Order: 250,
				Events: []turn.Event{
					{Kind: "ctx.low.1.v1", Author: turn.AuthorSystem, Payload: json.RawMessage(`{}`)},
					{Kind: "ctx.low.2.v1", Author: turn.AuthorSystem, Payload: json.RawMessage(`{}`)},
				},
			},
		},
	}
	executor, _ := newContextProviderExecutor(t, model, nil, WithContextProvider(high), WithContextProvider(low))

	events := drainTurn(t, executor, ownerTurnRequest())

	var preModel []string
	for i := range events {
		if events[i].Author != turn.AuthorSystem {
			break
		}
		preModel = append(preModel, events[i].Kind)
	}
	want := []string{"ctx.low.1.v1", "ctx.low.2.v1", "ctx.high.v1"}
	if !slices.Equal(preModel, want) {
		t.Fatalf("pre-model events = %v, want %v", preModel, want)
	}
	if len(events) <= len(want) {
		t.Fatalf("expected model events after pre-model events, got %d total", len(events))
	}
}
