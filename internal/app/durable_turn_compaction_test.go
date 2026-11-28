//go:build durable

package app

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	adkmodel "google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/session"
	"google.golang.org/genai"

	"github.com/anggasct/aura/internal/modules/compaction"
	"github.com/anggasct/aura/internal/platform/operation/operationtest"
	"github.com/anggasct/aura/internal/platform/tools"
	runtimeadk "github.com/anggasct/aura/internal/platform/turn/adk"
	turnstore "github.com/anggasct/aura/internal/platform/turn/store"
)

const (
	compactionCallsLedger    = "compaction-calls"
	compactionSummarizerKind = "compaction-summarizer"
	durableLifeOneSummary    = "LIFE-ONE-DURABLE-SUMMARY"
)

type ledgerSummarizer struct {
	ledger *turnLedger
}

func (s *ledgerSummarizer) Summarize(_ context.Context, _ *compaction.SummarizeRequest) (*compaction.SummarizeResult, error) {
	if _, err := s.ledger.record(compactionSummarizerKind, fmt.Sprintf("life-%d", s.ledger.life)); err != nil {
		return nil, err
	}
	if s.ledger.life > 1 {
		return nil, errors.New("summarizer unavailable after restart")
	}
	return &compaction.SummarizeResult{
		Text:     `{"goals":["` + durableLifeOneSummary + `"],"decisions":[],"constraints":[],"open_work":[],"facts":[]}`,
		Producer: compaction.Producer{Provider: "openai_chat_compat", Model: "durable-summary-model"},
	}, nil
}

type ledgerCompactor struct {
	ledger *turnLedger
	inner  runtimeadk.HistoryCompactor
}

func (c *ledgerCompactor) Compact(ctx context.Context, req *runtimeadk.CompactionRequest) (*runtimeadk.CompactionResult, error) {
	if _, err := c.ledger.record(compactionCallsLedger, fmt.Sprintf("life-%d", c.ledger.life)); err != nil {
		return nil, err
	}
	return c.inner.Compact(ctx, req)
}

func seedCompactionDurableSession(t *testing.T, db *sql.DB) {
	t.Helper()
	eventStore := turnstore.NewEventStore(db)
	base := time.Date(2026, 11, 28, 12, 0, 0, 0, time.UTC)
	appendADK := func(turnID string, ev *session.Event) {
		re, err := turnstore.RuntimeEventFromADK(suiteSession, turnID, ev)
		if err != nil {
			t.Fatalf("RuntimeEventFromADK: %v", err)
		}
		if _, err := eventStore.AppendSequenced(t.Context(), suiteSession, &re); err != nil {
			t.Fatalf("AppendSequenced: %v", err)
		}
	}
	appendADK("hist-1", &session.Event{
		ID: "dh-1-u", Timestamp: base, InvocationID: "inv-dh-1", Author: "user",
		LLMResponse: adkmodel.LLMResponse{Content: genai.NewContentFromText("old search "+strings.Repeat("alpha ", 250), genai.RoleUser)},
	})
	appendADK("hist-1", &session.Event{
		ID: "dh-1-call", Timestamp: base.Add(time.Second), InvocationID: "inv-dh-1", Author: "main",
		LLMResponse: adkmodel.LLMResponse{Content: &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{
			FunctionCall: &genai.FunctionCall{ID: "call_durable_search_1", Name: tools.OperationSearchToolName, Args: map[string]any{"group": operationtest.Group}},
		}}}},
	})
	appendADK("hist-1", &session.Event{
		ID: "dh-1-resp", Timestamp: base.Add(2 * time.Second), InvocationID: "inv-dh-1", Author: "main",
		LLMResponse: adkmodel.LLMResponse{Content: &genai.Content{Role: genai.RoleUser, Parts: []*genai.Part{{
			FunctionResponse: &genai.FunctionResponse{
				ID:       "call_durable_search_1",
				Name:     tools.OperationSearchToolName,
				Response: map[string]any{"loaded": []any{operationtest.ToolOf(operationtest.Change)}},
			},
		}}}},
	})
	appendADK("hist-1", &session.Event{
		ID: "dh-1-a", Timestamp: base.Add(3 * time.Second), InvocationID: "inv-dh-1", Author: "main",
		LLMResponse: adkmodel.LLMResponse{Content: genai.NewContentFromText("found "+strings.Repeat("beta ", 250), genai.RoleModel)},
	})
	seedSessionTextTurns(t, db, suiteSession, []struct{ turnID, user, assistant string }{
		{turnID: "hist-2", user: "HIST-2 " + strings.Repeat("gamma ", 250), assistant: "HIST-2-REP " + strings.Repeat("delta ", 250)},
		{turnID: "hist-3", user: "HIST-3-RECENT", assistant: "HIST-3-REPLY"},
	})
}

func compactionAcrossLives(t *testing.T, db *sql.DB, ledger *turnLedger) lifeWiring {
	t.Helper()
	cfg := compaction.Default()
	cfg.RecentCompleteTurns = 1
	cfg.SafetyMarginTokens = 200
	cfg.Summary.MaxOutputTokens = 200
	store, err := compaction.NewIndexedEventSummaryStore(turnstore.NewEventKindReader(db), turnstore.NewEventStore(db))
	if err != nil {
		t.Fatalf("NewIndexedEventSummaryStore: %v", err)
	}
	mod, err := compaction.New(t.Context(), &compaction.ModuleDeps{
		Config:     cfg,
		Store:      store,
		Summarizer: &ledgerSummarizer{ledger: ledger},
		Logger:     slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatalf("compaction.New: %v", err)
	}
	return lifeWiring{
		compactor: &ledgerCompactor{ledger: ledger, inner: compactionAdapter{mod: mod}},
		capabilities: staticCapabilityLookup{
			scriptedModelName: {ID: scriptedModelName, ContextTokens: 1600, Tokenizer: "estimated"},
		},
	}
}

func TestDurableTurnCompactionReplaysDeterministically(t *testing.T) {
	suite := startKilledSuite(t, "compaction-replay")
	suite.child.waitKillPoint(t, "tool-a is entered after step 1 compaction", suite.ledger.exists(t, "tool-a.entered"))
	suite.ledger.requireCount(t, compactionSummarizerKind, 1)
	suite.ledger.requireCount(t, compactionCallsLedger, 1)

	firstCalls := suite.ledger.records(t, modelCalls)
	if len(firstCalls) != 1 || !strings.Contains(firstCalls[0].Text, durableLifeOneSummary) {
		t.Fatalf("first life model-1 = %+v, want %q", firstCalls, durableLifeOneSummary)
	}
	firstDeclared := suite.ledger.records(t, modelDeclarations)
	if len(firstDeclared) != 1 || strings.Contains(firstDeclared[0].Text, operationtest.ToolOf(operationtest.Change)) {
		t.Fatalf("first life declarations = %+v, want compacted-out operation unloaded", firstDeclared)
	}

	suite.child.kill(t)
	life := suite.openSecondLife(t)
	suite.release(t, "tool-a")
	suite.requireResumed(t, life)

	summarizerCalls := suite.ledger.records(t, compactionSummarizerKind)
	if len(summarizerCalls) != 1 || summarizerCalls[0].Life != 1 {
		t.Errorf("summarizer calls = %+v, want exactly 1 on life 1", summarizerCalls)
	}
	compactCalls := suite.ledger.records(t, compactionCallsLedger)
	if len(compactCalls) != 2 || compactCalls[0].Life != 1 || compactCalls[1].Life != 2 {
		t.Errorf("compaction calls = %+v, want 1 on life 1 (step 1) and 1 on life 2 (step 2)", compactCalls)
	}
	declared := suite.ledger.records(t, modelDeclarations)
	if len(declared) != 2 {
		t.Fatalf("model declarations = %d, want 2", len(declared))
	}
	if strings.Contains(declared[1].Text, operationtest.ToolOf(operationtest.Change)) {
		t.Errorf("replayed turn loaded compacted-out operation: %q", declared[1].Text)
	}
}
