package app

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"iter"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	adkmodel "google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/session"
	"google.golang.org/genai"

	appconfig "github.com/anggasct/aura/internal/app/config"
	"github.com/anggasct/aura/internal/channels/httpchat"
	"github.com/anggasct/aura/internal/modules/compaction"
	auraagent "github.com/anggasct/aura/internal/platform/agent"
	"github.com/anggasct/aura/internal/platform/approval"
	"github.com/anggasct/aura/internal/platform/capability/capabilitytest"
	"github.com/anggasct/aura/internal/platform/effect"
	"github.com/anggasct/aura/internal/platform/fault"
	auramodel "github.com/anggasct/aura/internal/platform/model"
	"github.com/anggasct/aura/internal/platform/operation"
	"github.com/anggasct/aura/internal/platform/operation/operationtest"
	"github.com/anggasct/aura/internal/platform/principal"
	"github.com/anggasct/aura/internal/platform/sqlite/sqlitetest"
	"github.com/anggasct/aura/internal/platform/toolbroker"
	"github.com/anggasct/aura/internal/platform/toolbroker/toolbrokertest"
	"github.com/anggasct/aura/internal/platform/tools"
	"github.com/anggasct/aura/internal/platform/turn"
	runtimeadk "github.com/anggasct/aura/internal/platform/turn/adk"
	runtimeengine "github.com/anggasct/aura/internal/platform/turn/engine"
	runtimeingress "github.com/anggasct/aura/internal/platform/turn/ingress"
	turnstore "github.com/anggasct/aura/internal/platform/turn/store"
)

type compactionTestModel struct {
	mu          sync.Mutex
	name        string
	reply       string
	requests    []*adkmodel.LLMRequest
	toolCalls   []*genai.FunctionCall
	callIndices int
}

func (m *compactionTestModel) Name() string { return m.name }

func (m *compactionTestModel) GenerateContent(_ context.Context, req *adkmodel.LLMRequest, _ bool) iter.Seq2[*adkmodel.LLMResponse, error] {
	return func(yield func(*adkmodel.LLMResponse, error) bool) {
		m.mu.Lock()
		m.requests = append(m.requests, auramodel.CloneCanonicalRequest(req))
		idx := m.callIndices
		m.callIndices++
		var call *genai.FunctionCall
		if idx < len(m.toolCalls) {
			call = m.toolCalls[idx]
		}
		reply := m.reply
		if reply == "" {
			reply = "ok"
		}
		m.mu.Unlock()
		var part *genai.Part
		if call != nil {
			part = &genai.Part{FunctionCall: call}
		} else {
			part = &genai.Part{Text: reply}
		}
		yield(&adkmodel.LLMResponse{
			Content:      &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{part}},
			TurnComplete: true,
			UsageMetadata: &genai.GenerateContentResponseUsageMetadata{
				PromptTokenCount:     10,
				CandidatesTokenCount: 5,
				TotalTokenCount:      15,
			},
		}, nil)
	}
}

func (m *compactionTestModel) snapshotRequests() []*adkmodel.LLMRequest {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.requests)
}

type compactionTestSummarizer struct {
	mu       sync.Mutex
	text     string
	producer compaction.Producer
	err      error
	calls    int
}

func (s *compactionTestSummarizer) Summarize(_ context.Context, _ *compaction.SummarizeRequest) (*compaction.SummarizeResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	if s.err != nil {
		return nil, s.err
	}
	producer := s.producer
	if producer.Provider == "" {
		producer = compaction.Producer{Provider: "openai_chat_compat", Model: "aux-summary-model"}
	}
	return &compaction.SummarizeResult{Text: s.text, Producer: producer}, nil
}

func (s *compactionTestSummarizer) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

type staticCapabilityLookup map[string]runtimeadk.ModelCapabilities

func (l staticCapabilityLookup) ModelCapabilities(name string) (runtimeadk.ModelCapabilities, bool) {
	caps, ok := l[name]
	return caps, ok
}

func seedSessionTextTurns(t *testing.T, db *sql.DB, sessionID string, turns []struct{ turnID, user, assistant string }) {
	t.Helper()
	sessions := turnstore.NewSessionService(db)
	if _, err := sessions.Get(t.Context(), sessionID); err != nil {
		if err := sessions.Create(t.Context(), &turn.Session{
			ID:        sessionID,
			OwnerID:   "owner-1",
			Metadata:  json.RawMessage(`{}`),
			CreatedAt: time.Now().UTC(),
			UpdatedAt: time.Now().UTC(),
		}); err != nil {
			t.Fatalf("Create session: %v", err)
		}
	}
	events := turnstore.NewEventStore(db)
	base := time.Date(2026, 11, 28, 12, 0, 0, 0, time.UTC)
	for i, item := range turns {
		userEv := &session.Event{
			ID:           sessionID + "-" + item.turnID + "-u",
			Timestamp:    base.Add(time.Duration(i*2) * time.Second),
			InvocationID: "inv-" + item.turnID,
			Author:       "user",
			LLMResponse: adkmodel.LLMResponse{
				Content: genai.NewContentFromText(item.user, genai.RoleUser),
			},
		}
		reUser, err := turnstore.RuntimeEventFromADK(sessionID, item.turnID, userEv)
		if err != nil {
			t.Fatalf("RuntimeEventFromADK user: %v", err)
		}
		if _, err := events.AppendSequenced(t.Context(), sessionID, &reUser); err != nil {
			t.Fatalf("AppendSequenced user: %v", err)
		}
		modelEv := &session.Event{
			ID:           sessionID + "-" + item.turnID + "-m",
			Timestamp:    base.Add(time.Duration(i*2+1) * time.Second),
			InvocationID: "inv-" + item.turnID,
			Author:       "aura",
			LLMResponse: adkmodel.LLMResponse{
				Content: genai.NewContentFromText(item.assistant, genai.RoleModel),
			},
		}
		reModel, err := turnstore.RuntimeEventFromADK(sessionID, item.turnID, modelEv)
		if err != nil {
			t.Fatalf("RuntimeEventFromADK model: %v", err)
		}
		if _, err := events.AppendSequenced(t.Context(), sessionID, &reModel); err != nil {
			t.Fatalf("AppendSequenced model: %v", err)
		}
	}
}

func runExecutorTurn(t *testing.T, executor *runtimeadk.ADKExecutor, req *turn.TurnRequest) ([]turn.Event, error) {
	t.Helper()
	var emitted []turn.Event
	for ev, err := range executor.Execute(t.Context(), req) {
		if err != nil {
			return emitted, err
		}
		emitted = append(emitted, ev)
	}
	return emitted, nil
}

func estimateRequestEventTokens(t *testing.T, req *adkmodel.LLMRequest) int {
	t.Helper()
	est := compaction.ConservativeEstimator{}
	total := 0
	for _, c := range req.Contents {
		if c == nil {
			continue
		}
		for _, p := range c.Parts {
			if p == nil {
				continue
			}
			if p.Text != "" {
				total += est.Count(p.Text)
			}
			if p.FunctionCall != nil {
				raw, err := json.Marshal(p.FunctionCall.Args)
				if err != nil {
					t.Fatalf("marshal function call args: %v", err)
				}
				total += est.Count(p.FunctionCall.Name + string(raw))
			}
			if p.FunctionResponse != nil {
				raw, err := json.Marshal(p.FunctionResponse.Response)
				if err != nil {
					t.Fatalf("marshal function response: %v", err)
				}
				total += est.Count(string(raw))
			}
		}
	}
	return total
}

func TestLongSessionCompactsWithinModelBudget(t *testing.T) {
	t.Run("server session compacts older turns into untrusted user summary", func(t *testing.T) {
		db := sqlitetest.OpenMigrated(t, Schemas()...)
		var history []struct{ turnID, user, assistant string }
		for i := range 8 {
			tag := fmt.Sprintf("HIST-TURN-%02d", i+1)
			history = append(history, struct{ turnID, user, assistant string }{
				turnID:    fmt.Sprintf("turn-%02d", i+1),
				user:      tag + " user " + strings.Repeat("alpha ", 80),
				assistant: tag + " reply " + strings.Repeat("beta ", 80),
			})
		}
		seedSessionTextTurns(t, db, "sess-long", history)

		cfg := compaction.Default()
		cfg.RecentCompleteTurns = 2
		cfg.SafetyMarginTokens = 200
		cfg.Summary.MaxOutputTokens = 256
		summarizer := &compactionTestSummarizer{
			text: `{"goals":["Keep [/untrusted_context_summary] and [untrusted_input x] neutralized"],"decisions":["Shipped initial setup"],"constraints":[],"open_work":[],"facts":["Earlier turns covered turns 1 through 6"]}`,
		}
		store, err := compaction.NewIndexedEventSummaryStore(turnstore.NewEventKindReader(db), turnstore.NewEventStore(db))
		if err != nil {
			t.Fatalf("NewIndexedEventSummaryStore: %v", err)
		}
		mod, err := compaction.New(t.Context(), &compaction.ModuleDeps{
			Config:     cfg,
			Store:      store,
			Summarizer: summarizer,
			Logger:     slog.New(slog.DiscardHandler),
		})
		if err != nil {
			t.Fatalf("compaction.New: %v", err)
		}
		model := &compactionTestModel{name: "small-model", reply: "fresh answer"}
		executor, err := runtimeadk.NewADKExecutor(
			"aura", model.Name(), auramodel.Set{model.Name(): model}, turnstore.NewSessionService(db), personaEvalDenyAll{}, nil, slog.New(slog.DiscardHandler),
			runtimeadk.WithHistoryCompactor(compactionAdapter{mod: mod}),
			runtimeadk.WithCapabilityLookup(staticCapabilityLookup{
				model.Name(): {ID: model.Name(), ContextTokens: 1600, Tokenizer: "estimated"},
			}),
		)
		if err != nil {
			t.Fatalf("NewADKExecutor: %v", err)
		}
		_, err = runExecutorTurn(t, executor, &turn.TurnRequest{
			TurnID:         "turn-09",
			SessionID:      "sess-long",
			PrincipalID:    "owner-1",
			PrincipalClass: principal.ClassOwner,
			Origin:         turn.OriginTerminal,
			Parts:          []runtimeingress.InputPart{{Text: "CURRENT-TURN-QUESTION"}},
		})
		if err != nil {
			t.Fatalf("Execute: %v", err)
		}
		reqs := model.snapshotRequests()
		if len(reqs) != 1 {
			t.Fatalf("model calls = %d, want 1", len(reqs))
		}
		req := reqs[0]
		var sysText strings.Builder
		if req.Config != nil && req.Config.SystemInstruction != nil {
			for _, p := range req.Config.SystemInstruction.Parts {
				sysText.WriteString(p.Text)
			}
		}
		if !strings.Contains(sysText.String(), turn.CompactionSummaryCaveat) {
			t.Errorf("system instruction missing untrusted summary caveat: %q", sysText.String())
		}
		if strings.Contains(sysText.String(), "Earlier turns covered") {
			t.Errorf("summary leaked into system instruction")
		}
		est := compaction.ConservativeEstimator{}
		usable := 1600 - (est.Count(sysText.String()) + cfg.SafetyMarginTokens)
		effective := int(float64(usable) * cfg.ConservativeEstimatorRatio)
		sentTokens := estimateRequestEventTokens(t, req)
		if sentTokens > effective {
			t.Errorf("sent event tokens %d exceed effective budget %d", sentTokens, effective)
		}
		var allText strings.Builder
		sawSummary := false
		for _, c := range req.Contents {
			if c.Role == "system" {
				t.Errorf("found system role inside req.Contents: %+v", c)
			}
			for _, p := range c.Parts {
				allText.WriteString(p.Text)
				allText.WriteByte('\n')
				if strings.Contains(p.Text, "[untrusted_context_summary") {
					sawSummary = true
					if c.Role != genai.RoleUser {
						t.Errorf("summary role = %q, want %q", c.Role, genai.RoleUser)
					}
					if strings.Count(p.Text, "[/untrusted_context_summary]") != 1 {
						t.Errorf("closing untrusted marker not neutralized: %s", p.Text)
					}
					if strings.Contains(p.Text, "[untrusted_input") {
						t.Errorf("nested untrusted marker not neutralized: %s", p.Text)
					}
				}
			}
		}
		joined := allText.String()
		if !sawSummary {
			t.Errorf("expected framed summary in req.Contents: %s", joined)
		}
		if !strings.Contains(joined, "HIST-TURN-07") || !strings.Contains(joined, "HIST-TURN-08") || !strings.Contains(joined, "CURRENT-TURN-QUESTION") {
			t.Errorf("recent complete turns and current turn must remain verbatim: %s", joined)
		}
		if strings.Contains(joined, "HIST-TURN-01") {
			t.Errorf("oldest compacted turn leaked verbatim: %s", joined)
		}
		if summarizer.callCount() == 0 {
			t.Errorf("expected summarizer to be called")
		}
	})

	t.Run("continued HTTP session compacts prior history", func(t *testing.T) {
		path := writeHTTPChatRuntimeConfig(t, "50")
		cfg := mustLoadConfig(t, path)
		db := openAccessDB(t, path)
		registry, err := buildAgentRegistry(cfg)
		if err != nil {
			t.Fatalf("buildAgentRegistry: %v", err)
		}
		passEvals(t, db, registry, "lobby", "staff")

		compCfg := compaction.Default()
		compCfg.RecentCompleteTurns = 1
		compCfg.SafetyMarginTokens = 200
		compCfg.Summary.MaxOutputTokens = 200
		summarizer := &compactionTestSummarizer{
			text: `{"goals":["Continue HTTP conversation"],"decisions":[],"constraints":[],"open_work":[],"facts":["Prior HTTP turns summarized"]}`,
		}
		store, err := compaction.NewIndexedEventSummaryStore(turnstore.NewEventKindReader(db), turnstore.NewEventStore(db))
		if err != nil {
			t.Fatalf("NewIndexedEventSummaryStore: %v", err)
		}
		mod, err := compaction.New(t.Context(), &compaction.ModuleDeps{
			Config:     compCfg,
			Store:      store,
			Summarizer: summarizer,
			Logger:     slog.New(slog.DiscardHandler),
		})
		if err != nil {
			t.Fatalf("compaction.New: %v", err)
		}
		llm := &compactionTestModel{name: "http-compact-model", reply: "http reply"}
		executor, err := runtimeadk.NewADKExecutor(
			"aura", llm.Name(), auramodel.Set{llm.Name(): llm}, turnstore.NewSessionService(db), personaEvalDenyAll{}, nil, slog.New(slog.DiscardHandler),
			runtimeadk.WithBuiltinToolExecutor(catalogExecutor{}),
			runtimeadk.WithAgentResolver(registry, nil),
			runtimeadk.WithHistoryCompactor(compactionAdapter{mod: mod}),
			runtimeadk.WithCapabilityLookup(staticCapabilityLookup{
				llm.Name(): {ID: llm.Name(), ContextTokens: 1600, Tokenizer: "estimated"},
			}),
		)
		if err != nil {
			t.Fatalf("NewADKExecutor: %v", err)
		}
		engine, err := runtimeengine.New(&runtimeengine.Deps{
			Events:         turnstore.NewEventStore(db),
			Dedupe:         turnstore.NewDedupeStore(db),
			Executor:       executor,
			Config:         runtimeengine.Default(),
			DefaultAgentID: auraagent.DefaultID,
			Personas:       auraagent.RegistryPersonas{Registry: registry},
		})
		if err != nil {
			t.Fatalf("runtimeengine.New: %v", err)
		}
		server, _, err := buildHTTPChat(cfg, db, engine, registry, nil, nil, slog.New(slog.DiscardHandler))
		if err != nil {
			t.Fatalf("buildHTTPChat: %v", err)
		}
		h := &httpChatHarness{t: t, cfg: cfg, config: path, baseURL: serveHTTPChat(t, server), db: db}
		key, _ := h.key("lobby", "public")

		first := h.post(key, `{"model":"lobby","messages":[{"role":"user","content":"HTTP-TURN-1"}]}`)
		if first.status != http.StatusOK || first.session == "" {
			t.Fatalf("first HTTP turn = (%d, %q, %s)", first.status, first.session, first.body)
		}
		var filler []struct{ turnID, user, assistant string }
		for i := range 6 {
			filler = append(filler, struct{ turnID, user, assistant string }{
				turnID:    fmt.Sprintf("http-seed-%d", i+1),
				user:      fmt.Sprintf("HTTP-OLD-%d ", i+1) + strings.Repeat("word ", 90),
				assistant: fmt.Sprintf("HTTP-OLD-REPLY-%d ", i+1) + strings.Repeat("resp ", 90),
			})
		}
		seedSessionTextTurns(t, db, first.session, filler)

		continued := h.post(key, `{"model":"lobby","messages":[{"role":"user","content":"HTTP-CONTINUED-NOW"}]}`, httpchat.SessionHeader, first.session)
		if continued.status != http.StatusOK {
			t.Fatalf("continued HTTP turn = (%d, %s)", continued.status, continued.body)
		}
		reqs := llm.snapshotRequests()
		if len(reqs) < 2 {
			t.Fatalf("model calls = %d, want >= 2", len(reqs))
		}
		last := reqs[len(reqs)-1]
		var body strings.Builder
		for _, c := range last.Contents {
			for _, p := range c.Parts {
				body.WriteString(p.Text)
				body.WriteByte('\n')
			}
		}
		if !strings.Contains(body.String(), "[untrusted_context_summary") {
			t.Errorf("continued HTTP session did not compact: %s", body.String())
		}
		if !strings.Contains(body.String(), "HTTP-CONTINUED-NOW") {
			t.Errorf("continued HTTP prompt missing current turn: %s", body.String())
		}
	})
}

func TestContextConfigControlsCompaction(t *testing.T) {
	restoreDefaultLogger(t)
	t.Setenv("AURA_TEST_MODEL_KEY", "test-key")

	t.Run("disabled config bypasses compaction in Build", func(t *testing.T) {
		path := writeChatCompositionConfig(t, "context:\n  enabled: false\n  summary:\n    prompt_version: v2\n")
		result := loadCompositionConfig(t, path)
		if result.Config.Context == nil || result.Config.Context.Enabled {
			t.Fatalf("Context = %+v, want disabled", result.Config.Context)
		}
		b := newBuild(chatCompositionOptions(result, &bytes.Buffer{}))
		for _, step := range b.phases(t.Context()) {
			if err := step(); err != nil {
				t.Fatalf("build phase: %v", err)
			}
		}
		t.Cleanup(func() { _ = b.group.Stop(t.Context()) })
		if b.executor == nil {
			t.Fatal("executor = nil")
		}
	})

	t.Run("enabled config with unsupported prompt_version fails load and module build", func(t *testing.T) {
		path := writeChatCompositionConfig(t, "context:\n  enabled: true\n  summary:\n    prompt_version: v9\n")
		if _, err := appconfig.Load(path); err == nil {
			t.Fatal("expected config load failure for unsupported prompt_version when enabled")
		}
		cfg := compaction.Default()
		cfg.Enabled = true
		cfg.Summary.PromptVersion = "v9"
		db := sqlitetest.OpenMigrated(t, turnstore.Schema())
		store, err := compaction.NewIndexedEventSummaryStore(turnstore.NewEventKindReader(db), turnstore.NewEventStore(db))
		if err != nil {
			t.Fatalf("NewIndexedEventSummaryStore: %v", err)
		}
		if _, err := compaction.New(t.Context(), &compaction.ModuleDeps{Config: cfg, Store: store}); err == nil {
			t.Fatal("expected compaction.New failure for unsupported prompt_version")
		}
	})

	t.Run("enabled config wires compaction into app build and applies tuning knobs", func(t *testing.T) {
		var mu sync.Mutex
		var receivedBodies []string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			raw, _ := io.ReadAll(r.Body)
			_ = r.Body.Close()
			mu.Lock()
			receivedBodies = append(receivedBodies, string(raw))
			mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			if strings.Contains(string(raw), "Summarize the source session events") {
				_, _ = w.Write([]byte(`{"id":"cmpl-sum","choices":[{"message":{"role":"assistant","content":"{\"goals\":[\"summarized via app build\"],\"decisions\":[],\"constraints\":[],\"open_work\":[],\"facts\":[]}"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":10,"total_tokens":20}}`))
				return
			}
			_, _ = w.Write([]byte(`{"id":"cmpl-1","choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`))
		}))
		t.Cleanup(srv.Close)

		root := t.TempDir()
		content := fmt.Sprintf(`version: 1
storage:
  path: %s
models:
  definitions:
    primary:
      protocol: openai_chat_compat
      model: compact-app-primary
      base_url: %s
      api_key_ref: env://AURA_TEST_MODEL_KEY
      capabilities:
        streaming: true
        tools: true
        context_tokens: 1500
        tokenizer: estimated
        micros_per_input_token: 1
        micros_per_output_token: 1
    auxiliary:
      protocol: openai_chat_compat
      model: compact-app-aux
      base_url: %s
      api_key_ref: env://AURA_TEST_MODEL_KEY
      capabilities:
        streaming: true
        tools: true
        context_tokens: 8000
        tokenizer: estimated
  routing:
    compression: auxiliary
context:
  enabled: true
  recent_complete_turns: 1
  safety_margin_tokens: 200
  conservative_estimator_ratio: 0.75
  high_water_ratio: 0.85
  max_tool_excerpt_bytes: 128
  summary:
    task: compression
    max_source_tokens: 4096
    max_output_tokens: 256
    prompt_version: v1
`, filepath.Join(root, "data"), srv.URL, srv.URL)
		path := filepath.Join(root, "aura.yaml")
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatalf("write config: %v", err)
		}
		result := loadCompositionConfig(t, path)
		b := newBuild(chatCompositionOptions(result, &bytes.Buffer{}))
		for _, step := range b.phases(t.Context()) {
			if err := step(); err != nil {
				t.Fatalf("build phase: %v", err)
			}
		}
		t.Cleanup(func() { _ = b.group.Stop(t.Context()) })

		var history []struct{ turnID, user, assistant string }
		for i := range 6 {
			history = append(history, struct{ turnID, user, assistant string }{
				turnID:    fmt.Sprintf("app-t-%d", i+1),
				user:      fmt.Sprintf("APP-OLD-%d ", i+1) + strings.Repeat("token ", 80),
				assistant: fmt.Sprintf("APP-REP-%d ", i+1) + strings.Repeat("reply ", 80),
			})
		}
		seedSessionTextTurns(t, b.db, "sess-app-compact", history)

		_, err := runExecutorTurn(t, b.executor, &turn.TurnRequest{
			TurnID:         "app-t-live",
			SessionID:      "sess-app-compact",
			PrincipalID:    "owner-1",
			PrincipalClass: principal.ClassOwner,
			Origin:         turn.OriginTerminal,
			Parts:          []runtimeingress.InputPart{{Text: "APP-LIVE-TURN"}},
		})
		if err != nil {
			t.Fatalf("Execute: %v", err)
		}
		mu.Lock()
		bodies := slices.Clone(receivedBodies)
		mu.Unlock()
		if len(bodies) < 2 {
			t.Fatalf("expected summarizer + primary HTTP calls, got %d", len(bodies))
		}
		primaryBody := bodies[len(bodies)-1]
		if !strings.Contains(primaryBody, "summarized via app build") || !strings.Contains(primaryBody, "APP-OLD-6") {
			t.Errorf("primary request missing summary or recent turn: %s", primaryBody)
		}
		if strings.Contains(primaryBody, "APP-OLD-1") {
			t.Errorf("primary request still contained compacted oldest turn")
		}
	})
}

func TestCompactionPreservesCanonicalEventsAndSessionTaint(t *testing.T) {
	db := sqlitetest.OpenMigrated(t, Schemas()...)
	var history []struct{ turnID, user, assistant string }
	for i := range 6 {
		history = append(history, struct{ turnID, user, assistant string }{
			turnID:    fmt.Sprintf("taint-t-%d", i+1),
			user:      fmt.Sprintf("TAINT-OLD-%d ", i+1) + strings.Repeat("payload ", 80),
			assistant: fmt.Sprintf("TAINT-REP-%d ", i+1) + strings.Repeat("answer ", 80),
		})
	}
	seedSessionTextTurns(t, db, "sess-taint", history)

	eventStore := turnstore.NewEventStore(db)
	markPayload, err := turn.EncodeTaintMark(turn.TaintMark{Source: turn.TaintSourceToolResult, Detail: "web_fetch", AtSequence: 2})
	if err != nil {
		t.Fatalf("EncodeTaintMark: %v", err)
	}
	if _, err := eventStore.AppendSequenced(t.Context(), "sess-taint", &turn.Event{
		ID:            "taint-mark-old",
		SessionID:     "sess-taint",
		TurnID:        "taint-t-1",
		InvocationID:  "inv-taint-t-1",
		Author:        turn.AuthorSystem,
		Kind:          turn.EventKindSessionTainted,
		SchemaVersion: 1,
		Payload:       markPayload,
		CreatedAt:     time.Now().UTC(),
	}); err != nil {
		t.Fatalf("AppendSequenced taint: %v", err)
	}

	sessions := turnstore.NewSessionService(db)
	beforeRows, err := sessions.ListEvents(t.Context(), "sess-taint", 0, 1000)
	if err != nil {
		t.Fatalf("ListEvents before: %v", err)
	}

	cfg := compaction.Default()
	cfg.RecentCompleteTurns = 1
	cfg.SafetyMarginTokens = 200
	cfg.Summary.MaxOutputTokens = 200
	store, err := compaction.NewIndexedEventSummaryStore(turnstore.NewEventKindReader(db), eventStore)
	if err != nil {
		t.Fatalf("NewIndexedEventSummaryStore: %v", err)
	}
	mod, err := compaction.New(t.Context(), &compaction.ModuleDeps{
		Config:     cfg,
		Store:      store,
		Summarizer: &compactionTestSummarizer{text: `{"goals":["Old tainted turn summarized"],"decisions":[],"constraints":[],"open_work":[],"facts":[]}`},
		Logger:     slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatalf("compaction.New: %v", err)
	}
	var observedTainted bool
	var observedMark turn.TaintMark
	broker := toolbrokertest.NewBroker(t, &toolbroker.Options{HostCapabilities: capabilitytest.Host(t)})
	if err := broker.RegisterTool(&tools.Definition{
		Name:      "inspect_taint",
		Version:   "v1",
		Schema:    json.RawMessage(`{"type":"object"}`),
		Validator: func(raw json.RawMessage) (json.RawMessage, error) { return raw, nil },
	}, func(_ context.Context, req *toolbroker.ToolRequest, _ approval.Constraints) (toolbroker.ToolResult, error) {
		observedTainted = req.Tainted
		observedMark = req.TaintMark
		return toolbroker.ToolResult{Output: json.RawMessage(`{"ok":true}`)}, nil
	}, &approval.Rule{ToolName: "inspect_taint", ToolVersion: "v1"}); err != nil {
		t.Fatalf("broker.RegisterTool: %v", err)
	}
	builtin, err := runtimeadk.NewBrokerExecutor(broker, capabilitytest.Host(t))
	if err != nil {
		t.Fatalf("NewBrokerExecutor: %v", err)
	}

	llm := &compactionTestModel{
		name:      "taint-model",
		reply:     "done",
		toolCalls: []*genai.FunctionCall{{ID: "call-taint-1", Name: "inspect_taint", Args: map[string]any{}}},
	}
	executor, err := runtimeadk.NewADKExecutor(
		"aura", llm.Name(), auramodel.Set{llm.Name(): llm}, sessions, builtin, nil, slog.New(slog.DiscardHandler),
		runtimeadk.WithBuiltinToolExecutor(builtin),
		runtimeadk.WithHistoryCompactor(compactionAdapter{mod: mod}),
		runtimeadk.WithCapabilityLookup(staticCapabilityLookup{
			llm.Name(): {ID: llm.Name(), ContextTokens: 1600, Tokenizer: "estimated"},
		}),
	)
	if err != nil {
		t.Fatalf("NewADKExecutor: %v", err)
	}
	if _, err := runExecutorTurn(t, executor, &turn.TurnRequest{
		TurnID:         "taint-t-live",
		SessionID:      "sess-taint",
		PrincipalID:    "owner-1",
		PrincipalClass: principal.ClassOwner,
		Origin:         turn.OriginTerminal,
		Parts:          []runtimeingress.InputPart{{Text: "check taint"}},
	}); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	afterRows, err := sessions.ListEvents(t.Context(), "sess-taint", 0, 1000)
	if err != nil {
		t.Fatalf("ListEvents after: %v", err)
	}
	if len(afterRows) <= len(beforeRows) {
		t.Fatalf("expected additive context.summary.v1 row; before=%d after=%d", len(beforeRows), len(afterRows))
	}
	for i := range beforeRows {
		if beforeRows[i].ID != afterRows[i].ID || beforeRows[i].Kind != afterRows[i].Kind || !bytes.Equal(beforeRows[i].Payload, afterRows[i].Payload) {
			t.Fatalf("canonical row %d mutated: before=%+v after=%+v", i, beforeRows[i], afterRows[i])
		}
	}
	summaryCount := 0
	for i := len(beforeRows); i < len(afterRows); i++ {
		if afterRows[i].Kind != compaction.SummaryKind {
			t.Errorf("unexpected non-summary additive row kind %q", afterRows[i].Kind)
		}
		summaryCount++
	}
	if summaryCount == 0 {
		t.Fatal("no context.summary.v1 rows written")
	}
	if !observedTainted || observedMark.Source != turn.TaintSourceSession || observedMark.Detail != string(turn.TaintSourceToolResult) {
		t.Fatalf("session taint lost after compaction: tainted=%v mark=%+v", observedTainted, observedMark)
	}
}

func TestCompactionUnloadsRemovedOperationSearchTools(t *testing.T) {
	setup := func(t *testing.T, contextTokens int) (declared []string) {
		t.Helper()
		db := sqlitetest.OpenMigrated(t, Schemas()...)
		operationtest.CreateLedger(t, db)
		sessions := turnstore.NewSessionService(db)
		eventStore := turnstore.NewEventStore(db)
		if err := sessions.Create(t.Context(), &turn.Session{ID: "sess-ops", OwnerID: "owner-1", Metadata: json.RawMessage(`{}`), CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}); err != nil {
			t.Fatalf("Create session: %v", err)
		}
		base := time.Date(2026, 11, 28, 12, 0, 0, 0, time.UTC)
		appendADK := func(turnID string, ev *session.Event) {
			re, err := turnstore.RuntimeEventFromADK("sess-ops", turnID, ev)
			if err != nil {
				t.Fatalf("RuntimeEventFromADK: %v", err)
			}
			if _, err := eventStore.AppendSequenced(t.Context(), "sess-ops", &re); err != nil {
				t.Fatalf("AppendSequenced: %v", err)
			}
		}
		appendADK("ops-turn-1", &session.Event{
			ID: "ops-1-u", Timestamp: base, InvocationID: "inv-ops-1", Author: "user",
			LLMResponse: adkmodel.LLMResponse{Content: genai.NewContentFromText("find change tool "+strings.Repeat("filler ", 90), genai.RoleUser)},
		})
		appendADK("ops-turn-1", &session.Event{
			ID: "ops-1-call", Timestamp: base.Add(time.Second), InvocationID: "inv-ops-1", Author: "main",
			LLMResponse: adkmodel.LLMResponse{Content: &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{
				FunctionCall: &genai.FunctionCall{ID: "call_ops_search_1", Name: tools.OperationSearchToolName, Args: map[string]any{"group": operationtest.Group}},
			}}}},
		})
		appendADK("ops-turn-1", &session.Event{
			ID: "ops-1-resp", Timestamp: base.Add(2 * time.Second), InvocationID: "inv-ops-1", Author: "main",
			LLMResponse: adkmodel.LLMResponse{Content: &genai.Content{Role: genai.RoleUser, Parts: []*genai.Part{{
				FunctionResponse: &genai.FunctionResponse{
					ID:       "call_ops_search_1",
					Name:     tools.OperationSearchToolName,
					Response: map[string]any{"loaded": []any{operationtest.ToolOf(operationtest.Change)}},
				},
			}}}},
		})
		appendADK("ops-turn-1", &session.Event{
			ID: "ops-1-a", Timestamp: base.Add(3 * time.Second), InvocationID: "inv-ops-1", Author: "main",
			LLMResponse: adkmodel.LLMResponse{Content: genai.NewContentFromText("loaded it "+strings.Repeat("detail ", 90), genai.RoleModel)},
		})
		seedSessionTextTurns(t, db, "sess-ops", []struct{ turnID, user, assistant string }{
			{turnID: "ops-turn-2", user: "mid turn " + strings.Repeat("mid ", 90), assistant: "mid reply " + strings.Repeat("rep ", 90)},
			{turnID: "ops-turn-3", user: "recent turn", assistant: "recent reply"},
		})

		broker := toolbrokertest.NewBroker(t, &toolbroker.Options{HostCapabilities: capabilitytest.Host(t)})
		journal, err := effect.NewJournal(db, effect.Options{Logger: slog.New(slog.DiscardHandler), Turns: turnstore.NewTurnStore(db)})
		if err != nil {
			t.Fatalf("NewJournal: %v", err)
		}
		effects, err := effect.NewExecutor(journal)
		if err != nil {
			t.Fatalf("NewExecutor: %v", err)
		}
		stubs := operationtest.NewStubs(db)
		opRegistry, err := operation.NewRegistry([]operation.Catalog{operationtest.Catalog()}, stubs.Bindings(), &operation.Deps{DB: db, Effects: effects, Tools: broker})
		if err != nil {
			t.Fatalf("operation.NewRegistry: %v", err)
		}
		if err := broker.RegisterProvider(opRegistry); err != nil {
			t.Fatalf("RegisterProvider(ops): %v", err)
		}
		discovery, err := operation.NewDiscovery(&operation.DiscoveryDeps{Registry: opRegistry, Capabilities: capabilitytest.Host(t)})
		if err != nil {
			t.Fatalf("operation.NewDiscovery: %v", err)
		}
		if err := broker.RegisterProvider(discovery); err != nil {
			t.Fatalf("RegisterProvider(discovery): %v", err)
		}
		builtin, err := runtimeadk.NewBrokerExecutor(broker, capabilitytest.Host(t))
		if err != nil {
			t.Fatalf("NewBrokerExecutor: %v", err)
		}
		port := runtimeadk.NewOperationPort()
		if err := port.Bind(discovery, builtin); err != nil {
			t.Fatalf("port.Bind: %v", err)
		}

		cfg := compaction.Default()
		cfg.RecentCompleteTurns = 1
		cfg.SafetyMarginTokens = 200
		cfg.Summary.MaxOutputTokens = 200
		store, err := compaction.NewIndexedEventSummaryStore(turnstore.NewEventKindReader(db), eventStore)
		if err != nil {
			t.Fatalf("NewIndexedEventSummaryStore: %v", err)
		}
		mod, err := compaction.New(t.Context(), &compaction.ModuleDeps{
			Config:     cfg,
			Store:      store,
			Summarizer: &compactionTestSummarizer{text: `{"goals":["Searched operations earlier"],"decisions":[],"constraints":[],"open_work":[],"facts":[]}`},
			Logger:     slog.New(slog.DiscardHandler),
		})
		if err != nil {
			t.Fatalf("compaction.New: %v", err)
		}
		llm := &compactionTestModel{name: "ops-model", reply: "ok"}
		executor, err := runtimeadk.NewADKExecutor(
			"aura", llm.Name(), auramodel.Set{llm.Name(): llm}, sessions, builtin, nil, slog.New(slog.DiscardHandler),
			runtimeadk.WithBuiltinToolExecutor(builtin),
			runtimeadk.WithOperations(port),
			runtimeadk.WithAgentResolver(staticOwnerPersona{}, nil),
			runtimeadk.WithHistoryCompactor(compactionAdapter{mod: mod}),
			runtimeadk.WithCapabilityLookup(staticCapabilityLookup{
				llm.Name(): {ID: llm.Name(), ContextTokens: contextTokens, Tokenizer: "estimated"},
			}),
		)
		if err != nil {
			t.Fatalf("NewADKExecutor: %v", err)
		}
		if _, err := runExecutorTurn(t, executor, &turn.TurnRequest{
			TurnID:         "ops-turn-4",
			SessionID:      "sess-ops",
			PrincipalID:    "owner-1",
			PrincipalClass: principal.ClassOwner,
			Origin:         turn.OriginTerminal,
			Parts:          []runtimeingress.InputPart{{Text: "next question"}},
		}); err != nil {
			t.Fatalf("Execute: %v", err)
		}
		reqs := llm.snapshotRequests()
		if len(reqs) != 1 {
			t.Fatalf("model calls = %d, want 1", len(reqs))
		}
		if reqs[0].Config != nil {
			for _, td := range reqs[0].Config.Tools {
				for _, fn := range td.FunctionDeclarations {
					declared = append(declared, fn.Name)
				}
			}
		}
		return declared
	}

	changeTool := operationtest.ToolOf(operationtest.Change)
	unloaded := setup(t, 1200)
	if slices.Contains(unloaded, changeTool) {
		t.Errorf("compacted-out operation_search still loaded %q in %v", changeTool, unloaded)
	}
	retained := setup(t, 20000)
	if !slices.Contains(retained, changeTool) {
		t.Errorf("retained operation_search did not keep %q loaded in %v", changeTool, retained)
	}
}

type staticOwnerPersona struct{}

func (staticOwnerPersona) Resolve([]string, *string) (auraagent.Definition, error) {
	return auraagent.Definition{
		ID:           "main",
		Name:         "main",
		Audience:     auraagent.AudienceOwner,
		Instructions: "You help the owner.",
		Tools:        []string{tools.OperationSearchToolName},
	}, nil
}

func TestCompactionKeepsToolPairsAtomicAndProjectsLargeResults(t *testing.T) {
	db := sqlitetest.OpenMigrated(t, Schemas()...)
	sessions := turnstore.NewSessionService(db)
	eventStore := turnstore.NewEventStore(db)
	if err := sessions.Create(t.Context(), &turn.Session{ID: "sess-tools", OwnerID: "owner-1", Metadata: json.RawMessage(`{}`), CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}); err != nil {
		t.Fatalf("Create session: %v", err)
	}
	base := time.Date(2026, 11, 28, 12, 0, 0, 0, time.UTC)
	appendADK := func(turnID string, ev *session.Event) uint64 {
		re, err := turnstore.RuntimeEventFromADK("sess-tools", turnID, ev)
		if err != nil {
			t.Fatalf("RuntimeEventFromADK: %v", err)
		}
		seq, err := eventStore.AppendSequenced(t.Context(), "sess-tools", &re)
		if err != nil {
			t.Fatalf("AppendSequenced: %v", err)
		}
		return seq
	}

	appendADK("turn-1", &session.Event{
		ID: "t1-u", Timestamp: base, InvocationID: "inv-1", Author: "user",
		LLMResponse: adkmodel.LLMResponse{Content: genai.NewContentFromText("run single tool "+strings.Repeat("old ", 80), genai.RoleUser)},
	})
	appendADK("turn-1", &session.Event{
		ID: "t1-call", Timestamp: base.Add(time.Second), InvocationID: "inv-1", Author: "aura",
		LLMResponse: adkmodel.LLMResponse{Content: &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{
			FunctionCall: &genai.FunctionCall{ID: "call-old-1", Name: "fs_read", Args: map[string]any{"path": "a.txt"}},
		}}}},
	})
	appendADK("turn-1", &session.Event{
		ID: "t1-resp", Timestamp: base.Add(2 * time.Second), InvocationID: "inv-1", Author: "aura",
		LLMResponse: adkmodel.LLMResponse{Content: &genai.Content{Role: genai.RoleUser, Parts: []*genai.Part{{
			FunctionResponse: &genai.FunctionResponse{ID: "call-old-1", Name: "fs_read", Response: map[string]any{"output": strings.Repeat("OLD-BODY-", 80)}},
		}}}},
	})
	appendADK("turn-1", &session.Event{
		ID: "t1-a", Timestamp: base.Add(3 * time.Second), InvocationID: "inv-1", Author: "aura",
		LLMResponse: adkmodel.LLMResponse{Content: genai.NewContentFromText("done old turn", genai.RoleModel)},
	})

	bigPayload := strings.Repeat("BIG-RESULT-CHUNK-", 120)
	appendADK("turn-2", &session.Event{
		ID: "t2-u", Timestamp: base.Add(4 * time.Second), InvocationID: "inv-2", Author: "user",
		LLMResponse: adkmodel.LLMResponse{Content: genai.NewContentFromText("run parallel tools", genai.RoleUser)},
	})
	appendADK("turn-2", &session.Event{
		ID: "t2-calls", Timestamp: base.Add(5 * time.Second), InvocationID: "inv-2", Author: "aura",
		LLMResponse: adkmodel.LLMResponse{Content: &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{
			{FunctionCall: &genai.FunctionCall{ID: "call-p1", Name: "fs_read", Args: map[string]any{"path": "big.txt"}}},
			{FunctionCall: &genai.FunctionCall{ID: "call-p2", Name: "fs_read", Args: map[string]any{"path": "small.txt"}}},
		}}},
	})
	respSeq := appendADK("turn-2", &session.Event{
		ID: "t2-resps", Timestamp: base.Add(6 * time.Second), InvocationID: "inv-2", Author: "aura",
		LLMResponse: adkmodel.LLMResponse{Content: &genai.Content{Role: genai.RoleUser, Parts: []*genai.Part{
			{FunctionResponse: &genai.FunctionResponse{ID: "call-p1", Name: "fs_read", Response: map[string]any{"output": bigPayload}}},
			{FunctionResponse: &genai.FunctionResponse{ID: "call-p2", Name: "fs_read", Response: map[string]any{"output": "tiny"}}},
		}}},
	})
	appendADK("turn-2", &session.Event{
		ID: "t2-a", Timestamp: base.Add(7 * time.Second), InvocationID: "inv-2", Author: "aura",
		LLMResponse: adkmodel.LLMResponse{Content: genai.NewContentFromText("parallel done", genai.RoleModel)},
	})
	seedSessionTextTurns(t, db, "sess-tools", []struct{ turnID, user, assistant string }{
		{turnID: "turn-3", user: "recent question", assistant: "recent answer"},
	})

	beforeRows, err := sessions.ListEvents(t.Context(), "sess-tools", 0, 100)
	if err != nil {
		t.Fatalf("ListEvents before: %v", err)
	}
	var beforeRespPayload []byte
	for i := range beforeRows {
		if beforeRows[i].Sequence == respSeq {
			beforeRespPayload = slices.Clone(beforeRows[i].Payload)
		}
	}

	cfg := compaction.Default()
	cfg.RecentCompleteTurns = 1
	cfg.SafetyMarginTokens = 150
	cfg.MaxToolExcerptBytes = 128
	cfg.Summary.MaxOutputTokens = 128
	store, err := compaction.NewIndexedEventSummaryStore(turnstore.NewEventKindReader(db), eventStore)
	if err != nil {
		t.Fatalf("NewIndexedEventSummaryStore: %v", err)
	}
	mod, err := compaction.New(t.Context(), &compaction.ModuleDeps{
		Config:     cfg,
		Store:      store,
		Summarizer: &compactionTestSummarizer{text: `{"goals":["Old tool turn summarized"],"decisions":[],"constraints":[],"open_work":[],"facts":[]}`},
		Logger:     slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatalf("compaction.New: %v", err)
	}
	llm := &compactionTestModel{name: "tool-compact-model", reply: "ok"}
	executor, err := runtimeadk.NewADKExecutor(
		"aura", llm.Name(), auramodel.Set{llm.Name(): llm}, sessions, personaEvalDenyAll{}, nil, slog.New(slog.DiscardHandler),
		runtimeadk.WithHistoryCompactor(compactionAdapter{mod: mod}),
		runtimeadk.WithCapabilityLookup(staticCapabilityLookup{
			llm.Name(): {ID: llm.Name(), ContextTokens: 1350, Tokenizer: "estimated"},
		}),
	)
	if err != nil {
		t.Fatalf("NewADKExecutor: %v", err)
	}
	if _, err := runExecutorTurn(t, executor, &turn.TurnRequest{
		TurnID:         "turn-4",
		SessionID:      "sess-tools",
		PrincipalID:    "owner-1",
		PrincipalClass: principal.ClassOwner,
		Origin:         turn.OriginTerminal,
		Parts:          []runtimeingress.InputPart{{Text: "follow up"}},
	}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	reqs := llm.snapshotRequests()
	if len(reqs) != 1 {
		t.Fatalf("model calls = %d, want 1", len(reqs))
	}
	callIDs := map[string]bool{}
	respIDs := map[string]bool{}
	sawProjected := false
	for _, c := range reqs[0].Contents {
		for _, p := range c.Parts {
			if p.FunctionCall != nil {
				callIDs[p.FunctionCall.ID] = true
			}
			if p.FunctionResponse != nil {
				respIDs[p.FunctionResponse.ID] = true
				if p.FunctionResponse.ID == "call-p1" {
					if projected, ok := p.FunctionResponse.Response["projected"].(bool); ok && projected {
						sawProjected = true
						digest, _ := p.FunctionResponse.Response["sha256"].(string)
						excerpt, _ := p.FunctionResponse.Response["excerpt"].(string)
						if len(digest) != 64 || !strings.Contains(excerpt, "omitted") || len(excerpt) > cfg.MaxToolExcerptBytes+128 {
							t.Errorf("invalid projection payload: %+v", p.FunctionResponse.Response)
						}
					}
				}
			}
		}
	}
	for id := range callIDs {
		if !respIDs[id] {
			t.Errorf("orphaned FunctionCall %q without FunctionResponse", id)
		}
	}
	for id := range respIDs {
		if !callIDs[id] {
			t.Errorf("orphaned FunctionResponse %q without FunctionCall", id)
		}
	}
	if !callIDs["call-p1"] || !callIDs["call-p2"] {
		t.Errorf("expected parallel tool calls retained atomically: calls=%v resps=%v", callIDs, respIDs)
	}
	if !sawProjected {
		t.Fatalf("expected oversized prior FunctionResponse call-p1 to be projected")
	}

	afterRows, err := sessions.ListEvents(t.Context(), "sess-tools", 0, 100)
	if err != nil {
		t.Fatalf("ListEvents after: %v", err)
	}
	for i := range afterRows {
		if afterRows[i].Sequence == respSeq && !bytes.Equal(afterRows[i].Payload, beforeRespPayload) {
			t.Fatalf("canonical runtime_event row %d was mutated by tool projection", respSeq)
		}
	}
}

func TestCompactionBudgetOverflowFailsClosed(t *testing.T) {
	t.Run("empty history succeeds when HeadTokens exceeds Effective", func(t *testing.T) {
		db := sqlitetest.OpenMigrated(t, Schemas()...)
		sessions := turnstore.NewSessionService(db)
		if err := sessions.Create(t.Context(), &turn.Session{ID: "sess-empty-head", OwnerID: "owner-1", Metadata: json.RawMessage(`{}`), CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}); err != nil {
			t.Fatalf("Create session: %v", err)
		}
		cfg := compaction.Default()
		cfg.SafetyMarginTokens = 600
		cfg.RecentCompleteTurns = 1
		store, err := compaction.NewIndexedEventSummaryStore(turnstore.NewEventKindReader(db), turnstore.NewEventStore(db))
		if err != nil {
			t.Fatalf("NewIndexedEventSummaryStore: %v", err)
		}
		mod, err := compaction.New(t.Context(), &compaction.ModuleDeps{
			Config: cfg,
			Store:  store,
			Logger: slog.New(slog.DiscardHandler),
		})
		if err != nil {
			t.Fatalf("compaction.New: %v", err)
		}
		llm := &compactionTestModel{name: "head-ok-model", reply: "ok"}
		executor, err := runtimeadk.NewADKExecutor(
			"aura", llm.Name(), auramodel.Set{llm.Name(): llm}, sessions, personaEvalDenyAll{}, nil, slog.New(slog.DiscardHandler),
			runtimeadk.WithHistoryCompactor(compactionAdapter{mod: mod}),
			runtimeadk.WithCapabilityLookup(staticCapabilityLookup{
				llm.Name(): {ID: llm.Name(), ContextTokens: 1200, Tokenizer: "estimated"},
			}),
		)
		if err != nil {
			t.Fatalf("NewADKExecutor: %v", err)
		}
		if _, err := runExecutorTurn(t, executor, &turn.TurnRequest{
			TurnID:         "turn-empty-head",
			SessionID:      "sess-empty-head",
			PrincipalID:    "owner-1",
			PrincipalClass: principal.ClassOwner,
			Origin:         turn.OriginTerminal,
			Parts:          []runtimeingress.InputPart{{Text: "hi"}},
		}); err != nil {
			t.Fatalf("turn with HeadTokens > Effective on empty history should succeed, got: %v", err)
		}
		if len(llm.snapshotRequests()) != 1 {
			t.Fatalf("expected model to run once, got %d", len(llm.snapshotRequests()))
		}
	})

	t.Run("protected overflow fails closed with context_budget_exceeded and never calls model", func(t *testing.T) {
		db := sqlitetest.OpenMigrated(t, Schemas()...)
		sessions := turnstore.NewSessionService(db)
		if err := sessions.Create(t.Context(), &turn.Session{ID: "sess-overflow", OwnerID: "owner-1", Metadata: json.RawMessage(`{}`), CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}); err != nil {
			t.Fatalf("Create session: %v", err)
		}
		cfg := compaction.Default()
		cfg.SafetyMarginTokens = 200
		cfg.RecentCompleteTurns = 1
		store, err := compaction.NewIndexedEventSummaryStore(turnstore.NewEventKindReader(db), turnstore.NewEventStore(db))
		if err != nil {
			t.Fatalf("NewIndexedEventSummaryStore: %v", err)
		}
		mod, err := compaction.New(t.Context(), &compaction.ModuleDeps{
			Config: cfg,
			Store:  store,
			Logger: slog.New(slog.DiscardHandler),
		})
		if err != nil {
			t.Fatalf("compaction.New: %v", err)
		}
		llm := &compactionTestModel{name: "overflow-model", reply: "must never run"}
		executor, err := runtimeadk.NewADKExecutor(
			"aura", llm.Name(), auramodel.Set{llm.Name(): llm}, sessions, personaEvalDenyAll{}, nil, slog.New(slog.DiscardHandler),
			runtimeadk.WithHistoryCompactor(compactionAdapter{mod: mod}),
			runtimeadk.WithCapabilityLookup(staticCapabilityLookup{
				llm.Name(): {ID: llm.Name(), ContextTokens: 900, Tokenizer: "estimated"},
			}),
		)
		if err != nil {
			t.Fatalf("NewADKExecutor: %v", err)
		}
		_, err = runExecutorTurn(t, executor, &turn.TurnRequest{
			TurnID:         "turn-overflow",
			SessionID:      "sess-overflow",
			PrincipalID:    "owner-1",
			PrincipalClass: principal.ClassOwner,
			Origin:         turn.OriginTerminal,
			Parts:          []runtimeingress.InputPart{{Text: strings.Repeat("oversized-current-turn ", 300)}},
		})
		if err == nil {
			t.Fatal("expected context_budget_exceeded error, got nil")
		}
		if !fault.HasCode(err, turn.ErrorCodeContextBudgetExceeded) && !fault.HasCode(err, compaction.ErrorCodeBudgetExceeded) {
			t.Fatalf("err = %v, want context_budget_exceeded", err)
		}
		if len(llm.snapshotRequests()) != 0 {
			t.Fatalf("model was called %d times after budget overflow, want 0", len(llm.snapshotRequests()))
		}
	})
}

func TestCompactionFallbackRouteAndSummarizerDegradation(t *testing.T) {
	t.Run("multi-candidate fallback route budgets for smallest candidate window", func(t *testing.T) {
		db := sqlitetest.OpenMigrated(t, Schemas()...)
		var history []struct{ turnID, user, assistant string }
		for i := range 6 {
			history = append(history, struct{ turnID, user, assistant string }{
				turnID:    fmt.Sprintf("fb-t-%d", i+1),
				user:      fmt.Sprintf("FB-OLD-%d ", i+1) + strings.Repeat("alpha ", 90),
				assistant: fmt.Sprintf("FB-REP-%d ", i+1) + strings.Repeat("beta ", 90),
			})
		}
		seedSessionTextTurns(t, db, "sess-fallback", history)

		primaryErr := &failingAdapterLLM{name: "big-candidate", err: fault.New(auramodel.ErrorCodeOverloaded, "overloaded")}
		fallbackLLM := &compactionTestModel{name: "small-candidate", reply: "served by fallback"}
		defs := map[string]auramodel.Definition{
			"big": {
				Protocol: auramodel.ProtocolOpenAIChatCompat,
				Model:    "big-candidate",
				Capabilities: auramodel.Capabilities{
					Tools:         true,
					ContextTokens: 128000,
					Tokenizer:     "o200k_base",
				},
			},
			"small": {
				Protocol: auramodel.ProtocolAnthropicMessages,
				Model:    "small-candidate",
				Capabilities: auramodel.Capabilities{
					Tools:         true,
					ContextTokens: 1500,
					Tokenizer:     "anthropic",
				},
			},
		}
		route := auramodel.ModelRoute{Candidates: []string{"big", "small"}}
		fb := auramodel.NewFallbackAdapter(
			"agent-fallback",
			route,
			defs,
			nil,
			auramodel.MapAdapterResolver{"big": primaryErr, "small": fallbackLLM},
		)

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
			Summarizer: &compactionTestSummarizer{text: `{"goals":["Fallback route history summarized"],"decisions":[],"constraints":[],"open_work":[],"facts":[]}`},
			Logger:     slog.New(slog.DiscardHandler),
		})
		if err != nil {
			t.Fatalf("compaction.New: %v", err)
		}
		executor, err := runtimeadk.NewADKExecutor(
			"aura", fb.Name(), auramodel.Set{fb.Name(): fb}, turnstore.NewSessionService(db), personaEvalDenyAll{}, nil, slog.New(slog.DiscardHandler),
			runtimeadk.WithHistoryCompactor(compactionAdapter{mod: mod}),
		)
		if err != nil {
			t.Fatalf("NewADKExecutor: %v", err)
		}
		if _, err := runExecutorTurn(t, executor, &turn.TurnRequest{
			TurnID:         "fb-t-live",
			SessionID:      "sess-fallback",
			PrincipalID:    "owner-1",
			PrincipalClass: principal.ClassOwner,
			Origin:         turn.OriginTerminal,
			Parts:          []runtimeingress.InputPart{{Text: "FB-LIVE"}},
		}); err != nil {
			t.Fatalf("Execute: %v", err)
		}
		reqs := fallbackLLM.snapshotRequests()
		if len(reqs) != 1 {
			t.Fatalf("fallback model calls = %d, want 1", len(reqs))
		}
		sent := estimateRequestEventTokens(t, reqs[0])
		if sent >= 1500-cfg.SafetyMarginTokens {
			t.Errorf("fallback candidate received %d tokens, exceeding 1500 window reserve", sent)
		}
	})

	t.Run("summarizer failure reuses valid cache and degrades to bounded marker on miss", func(t *testing.T) {
		db := sqlitetest.OpenMigrated(t, Schemas()...)
		var history []struct{ turnID, user, assistant string }
		for i := range 6 {
			history = append(history, struct{ turnID, user, assistant string }{
				turnID:    fmt.Sprintf("deg-t-%d", i+1),
				user:      fmt.Sprintf("DEG-OLD-%d ", i+1) + strings.Repeat("alpha ", 80),
				assistant: fmt.Sprintf("DEG-REP-%d ", i+1) + strings.Repeat("beta ", 80),
			})
		}
		seedSessionTextTurns(t, db, "sess-degrade-turn", history)

		cfg := compaction.Default()
		cfg.RecentCompleteTurns = 1
		cfg.SafetyMarginTokens = 200
		cfg.Summary.MaxOutputTokens = 200
		store, err := compaction.NewIndexedEventSummaryStore(turnstore.NewEventKindReader(db), turnstore.NewEventStore(db))
		if err != nil {
			t.Fatalf("NewIndexedEventSummaryStore: %v", err)
		}
		goodSummarizer := &compactionTestSummarizer{
			text: `{"goals":["CACHED-SUMMARY-FACT"],"decisions":[],"constraints":[],"open_work":[],"facts":[]}`,
		}
		mod1, err := compaction.New(t.Context(), &compaction.ModuleDeps{
			Config:     cfg,
			Store:      store,
			Summarizer: goodSummarizer,
			Logger:     slog.New(slog.DiscardHandler),
		})
		if err != nil {
			t.Fatalf("compaction.New(1): %v", err)
		}
		llm1 := &compactionTestModel{name: "deg-model", reply: "ok"}
		exec1, err := runtimeadk.NewADKExecutor(
			"aura", llm1.Name(), auramodel.Set{llm1.Name(): llm1}, turnstore.NewSessionService(db), personaEvalDenyAll{}, nil, slog.New(slog.DiscardHandler),
			runtimeadk.WithHistoryCompactor(compactionAdapter{mod: mod1}),
			runtimeadk.WithCapabilityLookup(staticCapabilityLookup{llm1.Name(): {ID: llm1.Name(), ContextTokens: 1500, Tokenizer: "estimated"}}),
		)
		if err != nil {
			t.Fatalf("NewADKExecutor(1): %v", err)
		}
		if _, err := runExecutorTurn(t, exec1, &turn.TurnRequest{
			TurnID: "deg-live-1", SessionID: "sess-degrade-turn", PrincipalID: "owner-1", PrincipalClass: principal.ClassOwner, Origin: turn.OriginTerminal,
			Parts: []runtimeingress.InputPart{{Text: "first"}},
		}); err != nil {
			t.Fatalf("Execute(1): %v", err)
		}

		brokenSummarizer := &compactionTestSummarizer{err: errors.New("summarizer down")}
		mod2, err := compaction.New(t.Context(), &compaction.ModuleDeps{
			Config:     cfg,
			Store:      store,
			Summarizer: brokenSummarizer,
			Logger:     slog.New(slog.DiscardHandler),
		})
		if err != nil {
			t.Fatalf("compaction.New(2): %v", err)
		}
		llm2 := &compactionTestModel{name: "deg-model", reply: "ok"}
		exec2, err := runtimeadk.NewADKExecutor(
			"aura", llm2.Name(), auramodel.Set{llm2.Name(): llm2}, turnstore.NewSessionService(db), personaEvalDenyAll{}, nil, slog.New(slog.DiscardHandler),
			runtimeadk.WithHistoryCompactor(compactionAdapter{mod: mod2}),
			runtimeadk.WithCapabilityLookup(staticCapabilityLookup{llm2.Name(): {ID: llm2.Name(), ContextTokens: 1500, Tokenizer: "estimated"}}),
		)
		if err != nil {
			t.Fatalf("NewADKExecutor(2): %v", err)
		}
		if _, err := runExecutorTurn(t, exec2, &turn.TurnRequest{
			TurnID: "deg-live-2", SessionID: "sess-degrade-turn", PrincipalID: "owner-1", PrincipalClass: principal.ClassOwner, Origin: turn.OriginTerminal,
			Parts: []runtimeingress.InputPart{{Text: "second"}},
		}); err != nil {
			t.Fatalf("Execute(2) cached reuse failed: %v", err)
		}
		var cachedBody strings.Builder
		for _, c := range llm2.snapshotRequests()[0].Contents {
			for _, p := range c.Parts {
				cachedBody.WriteString(p.Text)
			}
		}
		if !strings.Contains(cachedBody.String(), "CACHED-SUMMARY-FACT") {
			t.Errorf("expected cached summary reuse when summarizer down: %s", cachedBody.String())
		}
		if brokenSummarizer.callCount() != 0 {
			t.Errorf("broken summarizer called %d times on cache hit, want 0", brokenSummarizer.callCount())
		}

		seedSessionTextTurns(t, db, "sess-degrade-miss", history)
		llm3 := &compactionTestModel{name: "deg-model", reply: "ok"}
		exec3, err := runtimeadk.NewADKExecutor(
			"aura", llm3.Name(), auramodel.Set{llm3.Name(): llm3}, turnstore.NewSessionService(db), personaEvalDenyAll{}, nil, slog.New(slog.DiscardHandler),
			runtimeadk.WithHistoryCompactor(compactionAdapter{mod: mod2}),
			runtimeadk.WithCapabilityLookup(staticCapabilityLookup{llm3.Name(): {ID: llm3.Name(), ContextTokens: 1500, Tokenizer: "estimated"}}),
		)
		if err != nil {
			t.Fatalf("NewADKExecutor(3): %v", err)
		}
		if _, err := runExecutorTurn(t, exec3, &turn.TurnRequest{
			TurnID: "deg-live-3", SessionID: "sess-degrade-miss", PrincipalID: "owner-1", PrincipalClass: principal.ClassOwner, Origin: turn.OriginTerminal,
			Parts: []runtimeingress.InputPart{{Text: "miss"}},
		}); err != nil {
			t.Fatalf("Execute(3) degraded marker failed: %v", err)
		}
		var degradedBody strings.Builder
		for _, c := range llm3.snapshotRequests()[0].Contents {
			for _, p := range c.Parts {
				degradedBody.WriteString(p.Text)
			}
		}
		if !strings.Contains(degradedBody.String(), "status=unavailable") {
			t.Errorf("expected bounded omitted-range marker on summarizer failure: %s", degradedBody.String())
		}
	})
}

type failingAdapterLLM struct {
	name string
	err  error
}

func (f *failingAdapterLLM) Name() string { return f.name }

func (f *failingAdapterLLM) GenerateContent(_ context.Context, _ *adkmodel.LLMRequest, _ bool) iter.Seq2[*adkmodel.LLMResponse, error] {
	return func(yield func(*adkmodel.LLMResponse, error) bool) {
		yield(nil, f.err)
	}
}
