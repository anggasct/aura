package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/modules/broadcast"
	"github.com/anggasct/aura/internal/modules/child"
	"github.com/anggasct/aura/internal/modules/schedule"
	"github.com/anggasct/aura/internal/platform/durable"
	"github.com/anggasct/aura/internal/platform/durable/sqlite"
	"github.com/anggasct/aura/internal/platform/principal"
	"github.com/anggasct/aura/internal/platform/turn"
	runtimeengine "github.com/anggasct/aura/internal/platform/turn/engine"
	runtimesessions "github.com/anggasct/aura/internal/platform/turn/sessions"
	"github.com/anggasct/aura/internal/store"
)

const (
	serverTurnShutdownTimeout = 3 * time.Second
	drainShutdownTimeout      = 2 * time.Second
	stubAnswerDelay           = 200 * time.Millisecond
	stubRequestLimit          = 1 << 20
	leftTurnID                = "turn-left"
	leftSessionID             = "sess-left"
	stubChatReply             = `{"id":"stub-1","object":"chat.completion","model":"test-model","choices":[{"index":0,"message":{"role":"assistant","content":"Hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":1,"total_tokens":4}}`
	stubChatStream            = "data: {\"id\":\"stub-1\",\"model\":\"test-model\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"Hello\"},\"finish_reason\":null}]}\n\n" +
		"data: {\"id\":\"stub-1\",\"model\":\"test-model\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":1,\"total_tokens\":4}}\n\n" +
		"data: [DONE]\n\n"
)

type stubAnswer int

const (
	stubAnswers stubAnswer = iota
	stubHoldsItsAnswer
	stubNeverAnswers
)

type stubModel struct {
	url     string
	answer  stubAnswer
	calls   atomic.Int32
	arrived chan struct{}
	arrival sync.Once
	gate    chan struct{}
	opening sync.Once
}

func newStubModel(t *testing.T, answer stubAnswer) *stubModel {
	t.Helper()
	model := &stubModel{answer: answer, arrived: make(chan struct{}), gate: make(chan struct{})}
	if answer == stubAnswers {
		model.open()
	}
	srv := httptest.NewServer(http.HandlerFunc(model.serve))
	t.Cleanup(func() {
		srv.CloseClientConnections()
		srv.Close()
	})
	model.url = srv.URL
	return model
}

func (m *stubModel) open() {
	m.opening.Do(func() { close(m.gate) })
}

func (m *stubModel) serve(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || r.URL.Path != "/v1/chat/completions" {
		http.NotFound(w, r)
		return
	}
	var request struct {
		Stream bool `json:"stream"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, stubRequestLimit)).Decode(&request); err != nil {
		http.Error(w, "malformed chat request", http.StatusBadRequest)
		return
	}
	m.calls.Add(1)
	m.arrival.Do(func() { close(m.arrived) })
	if !m.mayAnswer(r.Context()) {
		return
	}
	if request.Stream {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, stubChatStream)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(w, stubChatReply)
}

func (m *stubModel) mayAnswer(ctx context.Context) bool {
	if m.answer == stubNeverAnswers {
		<-ctx.Done()
		return false
	}
	select {
	case <-m.gate:
		return true
	case <-ctx.Done():
		return false
	}
}

func (m *stubModel) awaitFirstCall(t *testing.T) {
	t.Helper()
	select {
	case <-m.arrived:
	case <-time.After(compositionServerWait):
		t.Fatal("the model was never called")
	}
}

func stubModelsYAML(baseURL string) string {
	models := strings.Replace(compositionModelsYAML, "      protocol: openai_chat_compat\n", "      protocol: openai_chat_compat\n      base_url: "+baseURL+"\n", 1)
	return strings.Replace(models, "        tokenizer: anthropic\n", "        tokenizer: anthropic\n        micros_per_input_token: 1\n        micros_per_output_token: 1\n", 1)
}

type turnServerConfig struct {
	path     string
	chatAddr string
}

func writeTurnServerConfig(t *testing.T, model *stubModel, shutdownTimeout time.Duration) turnServerConfig {
	t.Helper()
	t.Setenv("AURA_TEST_MODEL_KEY", "test-key")
	chatAddr := freeLoopbackAddress(t)
	extra := "runtime:\n  shutdown_timeout: " + shutdownTimeout.String() + "\n" +
		"channels:\n  http:\n    enabled: true\n    listen_address: " + chatAddr + "\n" +
		"children:\n  enabled: true\n"
	path := writeCompositionConfig(t, freeLoopbackAddress(t), stubModelsYAML(model.url), extra)
	seedConfiguredStorage(t, path)
	return turnServerConfig{path: path, chatAddr: chatAddr}
}

type turnServer struct {
	*compositionServer
	logs *syncBuffer
}

func buildTurnServer(t *testing.T, configPath string) *turnServer {
	t.Helper()
	logs := &syncBuffer{}
	srv := &compositionServer{app: buildApp(t, serverCompositionOptions(loadCompositionConfig(t, configPath), logs))}
	t.Cleanup(func() {
		if srv.ran != nil {
			srv.cancel()
			<-srv.ran
		}
		_ = srv.app.Close(context.Background())
	})
	return &turnServer{compositionServer: srv, logs: logs}
}

func (s *turnServer) awaitStarted(t *testing.T) {
	t.Helper()
	awaitSchedule(t, "every lifecycle entry has started", func() bool {
		return slices.Contains(lifecycleNames(t, s.logs.String(), "lifecycle started"), "persona_eval_schedules")
	})
}

func (s *turnServer) stopWithinBound(t *testing.T) {
	t.Helper()
	s.stopRun(t)
	closed := make(chan error, 1)
	go func() { closed <- s.app.Close(t.Context()) }()
	select {
	case err := <-closed:
		if err != nil {
			t.Fatalf("Close: %v", err)
		}
	case <-time.After(compositionServerWait):
		t.Fatal("Close did not return")
	}
}

func (s *turnServer) hostedHandlers(t *testing.T) []string {
	t.Helper()
	for line := range strings.SplitSeq(s.logs.String(), "\n") {
		if line == "" {
			continue
		}
		var record struct {
			Msg      string   `json:"msg"`
			Handlers []string `json:"handlers"`
		}
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("log line %q is not JSON: %v", line, err)
		}
		if record.Msg == "durable host started" {
			return record.Handlers
		}
	}
	t.Fatal("the durable host never logged its start")
	return nil
}

func createOwnerAPIKey(t *testing.T, configPath string) string {
	t.Helper()
	stdout, _, err := runAPIKey(t, configPath, "create", "--persona", "main", "--class", "owner", "--label", "test", "--confirm-owner")
	if err != nil {
		t.Fatalf("apikey create: %v", err)
	}
	return strings.TrimSpace(stdout)
}

func awaitChatListener(t *testing.T, addr string) {
	t.Helper()
	dialer := &net.Dialer{Timeout: 100 * time.Millisecond}
	awaitSchedule(t, "the chat listener accepts", func() bool {
		conn, err := dialer.DialContext(t.Context(), "tcp", addr)
		if err != nil {
			return false
		}
		_ = conn.Close()
		return true
	})
}

func postChatTurn(t *testing.T, addr, key string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), compositionServerWait)
	defer cancel()
	body := strings.NewReader(`{"messages":[{"role":"user","content":"hi"}]}`)
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://"+addr+"/v1/chat/completions", body)
	if err != nil {
		t.Fatalf("chat request: %v", err)
	}
	request.Header.Set("Authorization", "Bearer "+key)
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("POST /v1/chat/completions: %v", err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("chat status = %d, want 200", response.StatusCode)
	}
	var reply struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, stubRequestLimit)).Decode(&reply); err != nil {
		t.Fatalf("decode the chat reply: %v", err)
	}
	turnID, found := strings.CutPrefix(reply.ID, "chatcmpl-")
	if !found || turnID == "" {
		t.Fatalf("chat reply id = %q, want chatcmpl-<turn id>", reply.ID)
	}
	return turnID
}

func terminalTurnKind(t *testing.T, db *sql.DB, turnID string) string {
	t.Helper()
	var kind string
	err := db.QueryRowContext(t.Context(), `SELECT kind FROM runtime_event WHERE turn_id = ? AND kind IN (?, ?, ?)`,
		turnID, turn.EventKindTurnCompleted, turn.EventKindTurnFailed, turn.EventKindTurnCancelled).Scan(&kind)
	if errors.Is(err, sql.ErrNoRows) {
		return ""
	}
	if err != nil {
		t.Fatalf("read the terminal event of %s: %v", turnID, err)
	}
	return kind
}

type storedRun struct {
	handler string
	state   string
}

func readStoredRun(t *testing.T, db *sql.DB, key string) storedRun {
	t.Helper()
	var run storedRun
	if err := db.QueryRowContext(t.Context(), `SELECT handler, state FROM durable_run WHERE key = ?`, key).Scan(&run.handler, &run.state); err != nil {
		t.Fatalf("read the run %s: %v", key, err)
	}
	return run
}

func awaitRunEnded(t *testing.T, db *sql.DB, key string) storedRun {
	t.Helper()
	awaitSchedule(t, "the run "+key+" has ended", func() bool {
		return countRows(t, db, `SELECT COUNT(*) FROM durable_run WHERE key = ? AND state <> 'running'`, key) == 1
	})
	return readStoredRun(t, db, key)
}

type leftTurnOutcome struct {
	kind string
	run  storedRun
}

func requireLeftTurnOutcome(t *testing.T, db *sql.DB, want leftTurnOutcome) {
	t.Helper()
	got := leftTurnOutcome{kind: terminalTurnKind(t, db, leftTurnID), run: readStoredRun(t, db, leftTurnID)}
	if got != want {
		t.Fatalf("left turn = %+v, want %+v", got, want)
	}
}

func leftTurnDescriptor() runtimesessions.Descriptor {
	return runtimesessions.Descriptor{
		TurnID: leftTurnID, SessionID: leftSessionID, PrincipalID: principal.OwnerID,
		PrincipalClass: string(principal.ClassOwner), Origin: string(turn.OriginInternal),
		Parts: []runtimesessions.Part{{Text: "hi"}}, AgentID: "main",
	}
}

func admitLeftTurn(t *testing.T, db *sql.DB) {
	t.Helper()
	now := time.Now().UTC()
	if err := store.NewSessionService(db).Create(t.Context(), &store.Session{
		ID: leftSessionID, OwnerID: principal.OwnerID, Metadata: json.RawMessage(`{}`), CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("create the session: %v", err)
	}
	sessions, err := sqlite.NewSessionStore(&sqlite.SessionStoreOptions{DB: db})
	if err != nil {
		t.Fatalf("NewSessionStore: %v", err)
	}
	admitted, err := sessions.Admit(t.Context(), &runtimesessions.AdmitRequest{Turn: leftTurnDescriptor(), AcceptedEventID: "evt-left-accepted", MaxPending: 16})
	if err != nil {
		t.Fatalf("Admit: %v", err)
	}
	if admitted.ToStart == nil {
		t.Fatal("the left turn was queued, want granted")
	}
}

func startLeftTurnRun(t *testing.T, db *sql.DB) {
	t.Helper()
	runtime, err := sqlite.New(&sqlite.Options{DB: db, Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatalf("sqlite.New: %v", err)
	}
	runtime.DeclareHandlers(runtimeengine.HandlerName)
	payload, err := json.Marshal(leftTurnDescriptor())
	if err != nil {
		t.Fatalf("encode the descriptor: %v", err)
	}
	if _, err := runtime.Start(t.Context(), durable.StartRequest{Handler: runtimeengine.HandlerName, Key: leftTurnID, Payload: payload}); err != nil {
		t.Fatalf("Start: %v", err)
	}
}

func TestServerTurnRunsAsADurableRun(t *testing.T) {
	restoreDefaultLogger(t)
	model := newStubModel(t, stubAnswers)
	cfg := writeTurnServerConfig(t, model, serverTurnShutdownTimeout)
	key := createOwnerAPIKey(t, cfg.path)
	srv := buildTurnServer(t, cfg.path)
	srv.run(t)
	awaitChatListener(t, cfg.chatAddr)
	requireNames(t, "hosted handlers", srv.hostedHandlers(t),
		[]string{runtimeengine.HandlerName, broadcast.HandlerName, schedule.HandlerName, child.HandlerName})
	turnID := postChatTurn(t, cfg.chatAddr, key)
	db := compositionDatabase(t, cfg.path)
	if got, want := awaitRunEnded(t, db, turnID), (storedRun{handler: runtimeengine.HandlerName, state: "succeeded"}); got != want {
		t.Errorf("turn run = %+v, want %+v", got, want)
	}
	if got := terminalTurnKind(t, db, turnID); got != turn.EventKindTurnCompleted {
		t.Errorf("terminal event = %q, want %q", got, turn.EventKindTurnCompleted)
	}
	if n := countRows(t, db, `SELECT COUNT(*) FROM durable_session_turn`); n != 0 {
		t.Errorf("session turn rows after the turn = %d, want 0", n)
	}
	srv.close(t)
}

func TestServerRecoversATurnAtBoot(t *testing.T) {
	cases := []struct {
		name string
		seed func(t *testing.T, db *sql.DB)
	}{
		{name: "a granted turn without a run", seed: admitLeftTurn},
		{name: "a granted turn whose run never executed", seed: func(t *testing.T, db *sql.DB) {
			t.Helper()
			admitLeftTurn(t, db)
			startLeftTurnRun(t, db)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			restoreDefaultLogger(t)
			model := newStubModel(t, stubAnswers)
			cfg := writeTurnServerConfig(t, model, serverTurnShutdownTimeout)
			db := compositionDatabase(t, cfg.path)
			tc.seed(t, db)
			srv := buildTurnServer(t, cfg.path)
			srv.run(t)
			awaitSchedule(t, "the left turn ends", func() bool { return terminalTurnKind(t, db, leftTurnID) != "" })
			if got := terminalTurnKind(t, db, leftTurnID); got != turn.EventKindTurnCompleted {
				t.Fatalf("terminal event = %q, want %q", got, turn.EventKindTurnCompleted)
			}
			if got := model.calls.Load(); got != 1 {
				t.Fatalf("model calls = %d, want 1", got)
			}
			srv.close(t)
		})
	}
}

func TestServerRunRetentionDeletesOrphanedTurnRuns(t *testing.T) {
	restoreDefaultLogger(t)
	model := newStubModel(t, stubAnswers)
	cfg := writeTurnServerConfig(t, model, serverTurnShutdownTimeout)
	db := compositionDatabase(t, cfg.path)
	const orphanKey = "turn-of-an-erased-session"
	stamp := time.Now().UTC().Add(-time.Hour).Format(time.RFC3339Nano)
	if _, err := db.ExecContext(t.Context(), `INSERT INTO durable_run (key, handler, payload, state, created_at, updated_at) VALUES (?, ?, ?, 'failed', ?, ?)`,
		orphanKey, runtimeengine.HandlerName, []byte(`{"turn_id":"`+orphanKey+`","session_id":"sess-erased"}`), stamp, stamp); err != nil {
		t.Fatalf("insert the orphaned run: %v", err)
	}
	srv := buildTurnServer(t, cfg.path)
	srv.run(t)
	awaitSchedule(t, "the orphaned turn run is deleted", func() bool {
		var runs int
		if err := db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM durable_run WHERE key = ?`, orphanKey).Scan(&runs); err != nil {
			t.Fatalf("count the orphaned run: %v", err)
		}
		return runs == 0
	})
	if got := model.calls.Load(); got != 0 {
		t.Fatalf("model calls = %d, want 0", got)
	}
	srv.close(t)
}

func TestServerStopDrainsItsTurns(t *testing.T) {
	t.Run("a turn whose model answers during the stop completes", func(t *testing.T) {
		restoreDefaultLogger(t)
		model := newStubModel(t, stubHoldsItsAnswer)
		cfg := writeTurnServerConfig(t, model, drainShutdownTimeout)
		db := compositionDatabase(t, cfg.path)
		admitLeftTurn(t, db)
		srv := buildTurnServer(t, cfg.path)
		srv.run(t)
		model.awaitFirstCall(t)
		answer := time.AfterFunc(stubAnswerDelay, model.open)
		defer answer.Stop()
		srv.stopWithinBound(t)
		requireLeftTurnOutcome(t, db, leftTurnOutcome{kind: turn.EventKindTurnCompleted, run: storedRun{handler: runtimeengine.HandlerName, state: "succeeded"}})
	})

	t.Run("a turn whose model never answers is cancelled and not resumed", func(t *testing.T) {
		restoreDefaultLogger(t)
		model := newStubModel(t, stubNeverAnswers)
		cfg := writeTurnServerConfig(t, model, drainShutdownTimeout)
		db := compositionDatabase(t, cfg.path)
		admitLeftTurn(t, db)
		srv := buildTurnServer(t, cfg.path)
		srv.run(t)
		model.awaitFirstCall(t)
		srv.stopWithinBound(t)
		requireLeftTurnOutcome(t, db, leftTurnOutcome{kind: turn.EventKindTurnCancelled, run: storedRun{handler: runtimeengine.HandlerName, state: "cancelled"}})
		if n := countRows(t, db, `SELECT COUNT(*) FROM durable_session_turn`); n != 0 {
			t.Fatalf("session turn rows after the stop = %d, want 0", n)
		}
		second := buildTurnServer(t, cfg.path)
		second.run(t)
		second.awaitStarted(t)
		second.close(t)
		if got := model.calls.Load(); got != 1 {
			t.Fatalf("model calls after a second server = %d, want 1", got)
		}
	})
}

func TestServerScheduleRunCompletesItsTurn(t *testing.T) {
	restoreDefaultLogger(t)
	model := newStubModel(t, stubAnswers)
	cfg := writeTurnServerConfig(t, model, serverTurnShutdownTimeout)
	gf := &globalFlags{configPath: cfg.path}
	jobID := addScheduleJob(t, gf)
	srv := buildTurnServer(t, cfg.path)
	srv.run(t)
	out, err := runScheduleCommand(t, gf, "run-now", jobID)
	if err != nil {
		t.Fatalf("schedule run-now: %v\n%s", err, out)
	}
	_, occurrenceID, found := strings.Cut(out, "occurrence: ")
	if !found {
		t.Fatalf("run-now output lacks the occurrence:\n%s", out)
	}
	occurrenceID = strings.TrimSpace(occurrenceID)
	db := compositionDatabase(t, cfg.path)
	awaitSchedule(t, "the fired occurrence settles", func() bool {
		return countRows(t, db, `SELECT COUNT(*) FROM scheduled_occurrence WHERE id = ? AND state <> 'fired'`, occurrenceID) == 1
	})
	var state, turnID, scheduledFor string
	if err := db.QueryRowContext(t.Context(), `SELECT state, COALESCE(turn_id, ''), scheduled_for_utc FROM scheduled_occurrence WHERE id = ?`, occurrenceID).
		Scan(&state, &turnID, &scheduledFor); err != nil {
		t.Fatalf("read the occurrence: %v", err)
	}
	if state != "completed" || turnID == "" {
		t.Fatalf("occurrence = %s with turn %q, want completed with a turn", state, turnID)
	}
	instant, err := time.Parse(time.RFC3339Nano, scheduledFor)
	if err != nil {
		t.Fatalf("parse the occurrence instant %q: %v", scheduledFor, err)
	}
	if got, want := awaitRunEnded(t, db, scheduleRunKey(jobID, instant)), (storedRun{handler: schedule.HandlerName, state: "succeeded"}); got != want {
		t.Fatalf("schedule run = %+v, want %+v", got, want)
	}
	if got, want := awaitRunEnded(t, db, turnID), (storedRun{handler: runtimeengine.HandlerName, state: "succeeded"}); got != want {
		t.Fatalf("turn run = %+v, want %+v", got, want)
	}
	srv.close(t)
}
