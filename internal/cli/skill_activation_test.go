package cli

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"iter"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/anggasct/aura/internal/command"
	"github.com/anggasct/aura/internal/config"
	"github.com/anggasct/aura/internal/principal"
	"github.com/anggasct/aura/internal/runtime"
	"github.com/anggasct/aura/internal/runtime/adk"
	"github.com/anggasct/aura/internal/runtime/engine"
	"github.com/anggasct/aura/internal/runtime/ingress"
	"github.com/anggasct/aura/internal/skills"
	"github.com/anggasct/aura/internal/store"

	"google.golang.org/adk/v2/agent"
	adkmodel "google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/functiontool"
	"google.golang.org/genai"
)

const skillBodyMarker = "REVIEW-PR-RULES-5d1e"

var skillModelSeq atomic.Int64

type skillRequest struct {
	texts []string
	tools []string
}

type skillCapturingModel struct {
	mu       sync.Mutex
	requests []skillRequest
}

func (m *skillCapturingModel) Name() string { return "skill-capturing-model" }

func (m *skillCapturingModel) GenerateContent(_ context.Context, req *adkmodel.LLMRequest, _ bool) iter.Seq2[*adkmodel.LLMResponse, error] {
	var captured skillRequest
	for _, content := range req.Contents {
		for _, part := range content.Parts {
			if part.Text != "" {
				captured.texts = append(captured.texts, part.Text)
			}
		}
	}
	if req.Config != nil {
		for _, declared := range req.Config.Tools {
			for _, fn := range declared.FunctionDeclarations {
				captured.tools = append(captured.tools, fn.Name)
			}
		}
	}
	slices.Sort(captured.tools)
	m.mu.Lock()
	m.requests = append(m.requests, captured)
	m.mu.Unlock()
	return func(yield func(*adkmodel.LLMResponse, error) bool) {
		yield(&adkmodel.LLMResponse{
			Content:      &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{Text: "ok"}}},
			TurnComplete: true,
		}, nil)
	}
}

func (m *skillCapturingModel) last(t *testing.T) skillRequest {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.requests) == 0 {
		t.Fatal("model was never called")
	}
	return m.requests[len(m.requests)-1]
}

func (r skillRequest) joined() string { return strings.Join(r.texts, "\n") }

type skillHarness struct {
	db      *sql.DB
	runtime *skillRuntime
	engine  *runtimeengine.Engine
	model   *skillCapturingModel
	skillID string
}

func newSkillHarness(t *testing.T, bodies map[string]string, cfg *config.Skills) *skillHarness {
	t.Helper()
	db, err := store.OpenDB(t.Context(), filepath.Join(t.TempDir(), "aura.db"))
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := store.Migrate(t.Context(), db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	root := t.TempDir()
	for name, body := range bodies {
		dir := filepath.Join(root, name)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		document := "---\nname: " + name + "\ndescription: Skill " + name + ".\n---\n" + body + "\n"
		if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(document), 0o600); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	cfg.Enabled, cfg.Roots = true, []string{root}
	rt, err := buildSkillRuntime(t.Context(), cfg, db, nil)
	if err != nil {
		t.Fatalf("buildSkillRuntime: %v", err)
	}
	records, err := (&skillStoreRegistry{inner: store.NewSkillStore(db)}).ListAll(t.Context(), 16)
	if err != nil {
		t.Fatalf("list skills: %v", err)
	}
	h := &skillHarness{db: db, runtime: rt, model: &skillCapturingModel{}}
	for i := range records {
		if _, err := rt.engine.Accept(t.Context(), records[i].ID, records[i].Digest, nil); err != nil {
			t.Fatalf("accept %s: %v", records[i].Name, err)
		}
		if records[i].Name == "review-pr" {
			h.skillID = records[i].ID
		}
	}
	name := fmt.Sprintf("skill-capturing-model-%d", skillModelSeq.Add(1))
	adkmodel.Register("^"+name+"$", func(context.Context, string) (adkmodel.LLM, error) { return h.model, nil })
	sampleTool, err := functiontool.New[map[string]any, map[string]any](
		functiontool.Config{Name: "sample_tool", Description: "sample"},
		func(agent.Context, map[string]any) (map[string]any, error) { return map[string]any{}, nil },
	)
	if err != nil {
		t.Fatalf("tool: %v", err)
	}
	sessions, events := store.NewSessionService(db), store.NewEventStore(db)
	executor, err := runtimeadk.NewADKExecutor("aura", name, sessions, events, terminalBroker{}, []tool.Tool{sampleTool}, slog.New(slog.DiscardHandler),
		runtimeadk.WithSkillContext(rt.context))
	if err != nil {
		t.Fatalf("NewADKExecutor: %v", err)
	}
	h.engine, err = runtimeengine.NewEngine(&runtimeengine.Config{MaxActiveTurns: 2, MaxPendingTurns: 4, Skills: rt.context}, events, store.NewDedupeStore(db), executor, nil)
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	executor.SetEventPublisher(h.engine)
	if err := sessions.Create(t.Context(), &store.Session{ID: "sess_skill", OwnerID: principal.OwnerID, Metadata: json.RawMessage(`{}`)}); err != nil {
		t.Fatalf("create session: %v", err)
	}
	return h
}

func (h *skillHarness) pin(t *testing.T, name string) *command.SkillPin {
	t.Helper()
	pin, err := h.runtime.bridge.Activate(t.Context(), name)
	if err != nil {
		t.Fatalf("activate %s: %v", name, err)
	}
	return &pin
}

func (h *skillHarness) turn(t *testing.T, text string, class principal.Class, pin *command.SkillPin) {
	t.Helper()
	req := &runtime.TurnRequest{
		SessionID:      "sess_skill",
		PrincipalID:    principal.OwnerID,
		PrincipalClass: class,
		Origin:         runtime.OriginTerminal,
		Parts:          []runtimeingress.InputPart{{Text: text}},
		Skill:          pin,
	}
	var last string
	for ev, err := range h.engine.Run(t.Context(), req) {
		if err != nil {
			t.Fatalf("run %q: %v", text, err)
		}
		last = ev.Kind
	}
	if last != runtime.EventKindTurnCompleted {
		t.Fatalf("turn %q ended with %s", text, last)
	}
}

func (h *skillHarness) activations(t *testing.T) []runtime.SkillActivation {
	t.Helper()
	activations, err := (&skillActivationLog{events: store.NewEventKindReader(h.db)}).Activations(t.Context(), "sess_skill", 16)
	if err != nil {
		t.Fatalf("activations: %v", err)
	}
	return activations
}

func defaultSkillConfig() *config.Skills {
	return &config.Skills{MaxIndexedSkills: 16, MaxInstructionTokens: 100, MaxSessionContextTokens: 300, MaxResourceBytes: 8388608, QuarantineRetention: config.Duration(1)}
}

func TestSkillActivationReachesTheModelAndPersistsInTheSession(t *testing.T) {
	h := newSkillHarness(t, map[string]string{"review-pr": skillBodyMarker + " Check auth paths first."}, defaultSkillConfig())
	h.turn(t, "hello", principal.ClassOwner, nil)
	baseline := h.model.last(t)
	if strings.Contains(baseline.joined(), runtime.SkillContextStart) {
		t.Fatal("block present before any activation")
	}

	invocation := "/skill review-pr check the auth module"
	h.turn(t, invocation, principal.ClassOwner, h.pin(t, "review-pr"))
	invoked := h.model.last(t)
	if !strings.Contains(invoked.joined(), runtime.SkillContextStart) || strings.Count(invoked.joined(), skillBodyMarker) != 1 || !strings.Contains(invoked.joined(), "untrusted third-party content") {
		t.Fatalf("invoking request lacks the pinned block:\n%s", invoked.joined())
	}
	if !slices.Contains(invoked.texts, invocation) {
		t.Fatalf("user input changed: %q", invoked.texts)
	}
	if !slices.Equal(invoked.tools, baseline.tools) || len(invoked.tools) == 0 {
		t.Fatalf("tools = %v, want the baseline %v", invoked.tools, baseline.tools)
	}
	activations := h.activations(t)
	if len(activations) != 1 || activations[0].SkillID != h.skillID || !strings.Contains(activations[0].Body, skillBodyMarker) {
		t.Fatalf("activations = %+v", activations)
	}

	h.turn(t, "and the tests?", principal.ClassOwner, nil)
	if follow := h.model.last(t); strings.Count(follow.joined(), skillBodyMarker) != 1 {
		t.Fatalf("follow-up turn lost the block:\n%s", follow.joined())
	}

	h.turn(t, "/skill review-pr again", principal.ClassOwner, h.pin(t, "review-pr"))
	if again := h.model.last(t); strings.Count(again.joined(), skillBodyMarker) != 1 {
		t.Fatalf("re-activation duplicated the body:\n%s", again.joined())
	}
	if activations := h.activations(t); len(activations) != 2 || activations[0].Body != "" {
		t.Fatalf("re-activation stored %+v", activations)
	}

	h.turn(t, "trusted asks", principal.ClassTrusted, nil)
	if trusted := h.model.last(t); strings.Contains(trusted.joined(), skillBodyMarker) {
		t.Fatal("skill block reached a non-owner turn")
	}

	if err := h.runtime.engine.Disable(t.Context(), h.skillID); err != nil {
		t.Fatalf("disable: %v", err)
	}
	h.turn(t, "after quarantine", principal.ClassOwner, nil)
	if after := h.model.last(t); strings.Contains(after.joined(), skillBodyMarker) {
		t.Fatalf("disabled skill still injected:\n%s", after.joined())
	}
	if _, err := h.runtime.bridge.Activate(t.Context(), "review-pr"); err == nil {
		t.Fatal("disabled skill activated")
	}
}

func TestSkillSessionBudgetEvictsTheOldestActivation(t *testing.T) {
	filler := strings.Repeat("y", 300)
	cfg := defaultSkillConfig()
	cfg.MaxSessionContextTokens = 200
	h := newSkillHarness(t, map[string]string{
		"first":  "FIRST-SKILL " + filler,
		"second": "SECOND-SKILL " + filler,
		"third":  "THIRD-SKILL " + filler,
	}, cfg)
	for _, name := range []string{"first", "second", "third"} {
		h.turn(t, "/skill "+name, principal.ClassOwner, h.pin(t, name))
	}
	got := h.model.last(t).joined()
	if strings.Contains(got, "FIRST-SKILL") || !strings.Contains(got, "SECOND-SKILL") || !strings.Contains(got, "THIRD-SKILL") {
		t.Fatalf("budgeted block:\n%s", got)
	}
	if strings.Index(got, "THIRD-SKILL") > strings.Index(got, "SECOND-SKILL") {
		t.Fatalf("block is not newest first:\n%s", got)
	}
}

func TestBuildSkillRuntimeRejectsASessionBoundBelowOneActivation(t *testing.T) {
	db, err := store.OpenDB(t.Context(), filepath.Join(t.TempDir(), "aura.db"))
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := store.Migrate(t.Context(), db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	cfg := defaultSkillConfig()
	cfg.Enabled, cfg.Roots, cfg.MaxSessionContextTokens = true, []string{t.TempDir()}, 99
	if _, err := buildSkillRuntime(t.Context(), cfg, db, nil); err == nil {
		t.Fatal("session bound below max_instruction_tokens accepted")
	} else if code, ok := skills.CodeOf(err); !ok || code != skills.ErrorCodeInvalidArgument {
		t.Fatalf("err = %v", err)
	}
}
