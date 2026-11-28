package runtimeadk

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"google.golang.org/adk/v2/agent"
	adkmodel "google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/functiontool"
	"google.golang.org/genai"

	"github.com/anggasct/aura/internal/platform/fault"
	auratools "github.com/anggasct/aura/internal/platform/tools"
	"github.com/anggasct/aura/internal/platform/turn"
	turnstore "github.com/anggasct/aura/internal/platform/turn/store"
)

type stubCompactor struct {
	res     *CompactionResult
	err     error
	lastReq *CompactionRequest
}

func (s *stubCompactor) Compact(_ context.Context, req *CompactionRequest) (*CompactionResult, error) {
	s.lastReq = req
	if s.err != nil {
		return nil, s.err
	}
	return s.res, nil
}

func TestCompactWithJournalRejectsCorruptPayloads(t *testing.T) {
	priorRow := turn.Event{ID: "ev-1", Sequence: 1, TurnID: "t1", Kind: turnstore.EventKindADK}
	priorEvent := &session.Event{
		ID:     "ev-1",
		Author: "aura",
		LLMResponse: adkmodel.LLMResponse{
			Content: &genai.Content{
				Role: genai.RoleUser,
				Parts: []*genai.Part{
					{Text: "plain"},
					{FunctionResponse: &genai.FunctionResponse{ID: "call_1", Name: "fs_read", Response: map[string]any{"output": "big"}}},
				},
			},
		},
	}
	nilContentEvent := &session.Event{ID: "ev-2", Author: "aura"}
	priorBySeq := map[uint64]*session.Event{1: priorEvent, 2: nilContentEvent}

	cases := map[string]struct {
		raw   string
		prior []turn.Event
	}{
		"null":                        {raw: `null`, prior: []turn.Event{priorRow}},
		"empty object":                {raw: `{}`, prior: []turn.Event{priorRow}},
		"empty items with prior":      {raw: `{"items":[]}`, prior: []turn.Event{priorRow}},
		"invalid json":                {raw: `{broken`, prior: []turn.Event{priorRow}},
		"unknown kind":                {raw: `{"items":[{"kind":"bogus"}]}`, prior: []turn.Event{priorRow}},
		"missing sequence":            {raw: `{"items":[{"kind":"event"}]}`, prior: []turn.Event{priorRow}},
		"unknown sequence":            {raw: `{"items":[{"kind":"event","sequence":99}]}`, prior: []turn.Event{priorRow}},
		"empty summary id":            {raw: `{"items":[{"kind":"summary","summary_text":"hello"}]}`, prior: []turn.Event{priorRow}},
		"empty summary text":          {raw: `{"items":[{"kind":"summary","summary_id":"ctxsum-1"}]}`, prior: []turn.Event{priorRow}},
		"negative part index":         {raw: `{"items":[{"kind":"event","sequence":1,"projections":[{"part_index":-1}]}]}`, prior: []turn.Event{priorRow}},
		"out of range part index":     {raw: `{"items":[{"kind":"event","sequence":1,"projections":[{"part_index":5}]}]}`, prior: []turn.Event{priorRow}},
		"non function response part":  {raw: `{"items":[{"kind":"event","sequence":1,"projections":[{"part_index":0}]}]}`, prior: []turn.Event{priorRow}},
		"projected event nil content": {raw: `{"items":[{"kind":"event","sequence":2,"projections":[{"part_index":0}]}]}`, prior: []turn.Event{priorRow}},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			inv := &journalInvocation{journal: map[string][]byte{"compact-1": []byte(tc.raw)}}
			ctx := scopedContext(t, inv)
			cfg := &sessionCompaction{
				compactor: &stubCompactor{err: errors.New("should replay from journal")},
				turnID:    "turn-live",
			}
			res, err := compactWithJournal(ctx, cfg, "sess-1", 1, tc.prior, nil, nil)
			if err == nil {
				_, err = materializeCompactedEvents(res, priorBySeq)
			}
			if err == nil {
				t.Fatalf("expected error for corrupt payload %s, got nil", tc.raw)
			}
			if !fault.HasCode(err, turn.ErrorCodeRuntimeInternal) {
				t.Errorf("err = %v, want code %s", err, turn.ErrorCodeRuntimeInternal)
			}
		})
	}
}

func TestCompactionToolTextIncludesPriorLoadedOperations(t *testing.T) {
	db, sessions, eventStore := newSessionTestDB(t)
	mustCreateSession(t, db, "sess-op-tooltext")
	base := time.Date(2026, 11, 28, 12, 0, 0, 0, time.UTC)

	appendADK := func(turnID string, ev *session.Event) {
		re, err := turnstore.RuntimeEventFromADK("sess-op-tooltext", turnID, ev)
		if err != nil {
			t.Fatalf("RuntimeEventFromADK: %v", err)
		}
		if _, err := eventStore.AppendSequenced(t.Context(), "sess-op-tooltext", &re); err != nil {
			t.Fatalf("AppendSequenced: %v", err)
		}
	}
	appendADK("prior-1", &session.Event{
		ID: "op-call-1", Timestamp: base, InvocationID: "inv-1", Author: "aura",
		LLMResponse: adkmodel.LLMResponse{Content: &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{
			FunctionCall: &genai.FunctionCall{ID: "call_prior_search", Name: auratools.OperationSearchToolName, Args: map[string]any{"group": "notes"}},
		}}}},
	})
	appendADK("prior-1", &session.Event{
		ID: "op-resp-1", Timestamp: base.Add(time.Second), InvocationID: "inv-1", Author: "aura",
		LLMResponse: adkmodel.LLMResponse{Content: &genai.Content{Role: genai.RoleUser, Parts: []*genai.Part{{
			FunctionResponse: &genai.FunctionResponse{
				ID:       "call_prior_search",
				Name:     auratools.OperationSearchToolName,
				Response: map[string]any{"loaded": []any{"aura_notes_create"}},
			},
		}}}},
	})

	loadedTool, err := functiontool.New[map[string]any, map[string]any](
		functiontool.Config{Name: "aura_notes_create", Description: "Create a structured note entry"},
		func(agent.Context, map[string]any) (map[string]any, error) { return map[string]any{}, nil },
	)
	if err != nil {
		t.Fatalf("functiontool.New: %v", err)
	}

	compactor := &stubCompactor{
		res: &CompactionResult{
			Items: []CompactionItem{
				{Kind: CompactionItemEvent, Sequence: 1},
				{Kind: CompactionItemEvent, Sequence: 2},
			},
		},
	}
	svc, err := NewADKSessionService(sessions)
	if err != nil {
		t.Fatalf("NewADKSessionService: %v", err)
	}
	svc.compaction = &sessionCompaction{
		compactor: compactor,
		turnID:    "turn-2",
		operationPlan: &operationPlan{
			visible: func(name string) bool { return name == "aura_notes_create" },
		},
		operationBinding: &operationBinding{
			tools: map[string]tool.Tool{"aura_notes_create": loadedTool},
		},
	}

	got, err := svc.Get(t.Context(), &session.GetRequest{SessionID: "sess-op-tooltext"})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	userEv := &session.Event{
		ID:     "turn-2-u",
		Author: userAuthor,
		LLMResponse: adkmodel.LLMResponse{
			Content: genai.NewContentFromText("create a note", genai.RoleUser),
		},
	}
	if err := svc.AppendEvent(t.Context(), got.Session, userEv); err != nil {
		t.Fatalf("AppendEvent: %v", err)
	}
	if _, err := svc.CurrentEvents("sess-op-tooltext"); err != nil {
		t.Fatalf("CurrentEvents: %v", err)
	}
	if compactor.lastReq == nil {
		t.Fatal("compactor was not called")
	}
	if !strings.Contains(compactor.lastReq.ToolText, "aura_notes_create") {
		t.Errorf("ToolText = %q, want prior loaded operation tool included", compactor.lastReq.ToolText)
	}
}

func TestADKEventsLoadErrorPropagatesOnLenAndAt(t *testing.T) {
	wantErr := errors.New("session store read failed")
	for _, method := range []string{"Len", "At", "All"} {
		t.Run(method, func(t *testing.T) {
			var reported error
			events := &adkEvents{
				load: func() ([]*session.Event, error) {
					return nil, wantErr
				},
				onErr: func(err error) {
					reported = err
				},
			}
			switch method {
			case "Len":
				if n := events.Len(); n != 0 {
					t.Errorf("Len() = %d, want 0", n)
				}
			case "At":
				if ev := events.At(0); ev != nil {
					t.Errorf("At(0) = %v, want nil", ev)
				}
			case "All":
				for range events.All() {
					t.Fatal("All() yielded event despite load error")
				}
			}
			if !errors.Is(reported, wantErr) {
				t.Errorf("reported error = %v, want %v", reported, wantErr)
			}
		})
	}
}

func TestCloneEventWithProjectionsValidatesInputs(t *testing.T) {
	ev := &session.Event{
		ID:     "ev-valid",
		Author: "aura",
		LLMResponse: adkmodel.LLMResponse{
			Content: &genai.Content{
				Role: genai.RoleUser,
				Parts: []*genai.Part{
					{FunctionResponse: &genai.FunctionResponse{ID: "call_1", Name: "fs_read", Response: map[string]any{"output": "large"}}},
				},
			},
		},
	}
	cloned, err := cloneEventWithProjections(ev, []CompactionToolProjection{{
		PartIndex:   0,
		Digest:      strings.Repeat("a", 64),
		Bytes:       500,
		Excerpt:     "ex",
		RefSequence: 3,
	}})
	if err != nil {
		t.Fatalf("cloneEventWithProjections valid: %v", err)
	}
	if cloned.Content.Parts[0].FunctionResponse.Response["projected"] != true {
		t.Errorf("cloned response = %+v", cloned.Content.Parts[0].FunctionResponse.Response)
	}
	if ev.Content.Parts[0].FunctionResponse.Response["output"] != "large" {
		t.Errorf("original event mutated: %+v", ev.Content.Parts[0].FunctionResponse.Response)
	}
}
