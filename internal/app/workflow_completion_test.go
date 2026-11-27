package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	appconfig "github.com/anggasct/aura/internal/app/config"
	"github.com/anggasct/aura/internal/channels/discord"
	"github.com/anggasct/aura/internal/channels/kit/access"
	"github.com/anggasct/aura/internal/channels/telegram"
	"github.com/anggasct/aura/internal/modules/workflow"
	auraagent "github.com/anggasct/aura/internal/platform/agent"
	"github.com/anggasct/aura/internal/platform/principal"
	"github.com/anggasct/aura/internal/platform/turn"
	turnstore "github.com/anggasct/aura/internal/platform/turn/store"
)

type fixedTarget struct {
	sessions map[string]access.HandoffTarget
}

func (f fixedTarget) ReplyTarget(sessionID string) (access.HandoffTarget, bool) {
	target, ok := f.sessions[sessionID]
	return target, ok
}

const (
	telegramTopicSession = "lobby/telegram:pocket/chat:5"
	emailThreadSession   = "lobby/email:guest/thread:1"
)

var telegramTopicTarget = workflow.ReplyTarget{Channel: "telegram", Account: "pocket", ChannelID: "-1001234567890", ThreadID: "7"}

type deliveryRecorder struct {
	mu       sync.Mutex
	sent     []deliveredText
	telegram []deliveredTelegramText
	failWith error
}

func (r *deliveryRecorder) deliverDiscord(_ context.Context, account *discord.Account, channelID, text string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.failWith != nil {
		return r.failWith
	}
	r.sent = append(r.sent, deliveredText{account: account.Name, channel: channelID, text: text})
	return nil
}

func (r *deliveryRecorder) deliverTelegram(_ context.Context, account *telegram.Account, chatID, threadID, text string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.failWith != nil {
		return r.failWith
	}
	r.telegram = append(r.telegram, deliveredTelegramText{account: account.Name, chat: chatID, thread: threadID, text: text})
	return nil
}

func (r *deliveryRecorder) deliverer() *workflowCompletionDeliverer {
	cfg := &appconfig.Config{Channels: appconfig.Channels{
		Discord:  discord.Config{Accounts: []discord.Account{{Name: "front"}}},
		Telegram: telegram.Config{Accounts: []telegram.Account{{Name: "pocket"}}},
	}}
	return &workflowCompletionDeliverer{cfg: cfg, deliver: r.deliverDiscord, deliverTelegram: r.deliverTelegram}
}

type completionHarness struct {
	*deliveryRecorder
	notifier   *workflow.CompletionNotifier
	controller *workflow.Controller
	runs       *workflow.Store
	db         *sql.DB
}

func seedConversation(t *testing.T, db *sql.DB, sessionID, principalID string) {
	t.Helper()
	if err := turnstore.NewSessionService(db).Create(t.Context(), &turn.Session{ID: sessionID, OwnerID: principalID, Metadata: []byte(`{}`), CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
}

func newCompletionNotifier(t *testing.T, runs *workflow.Store, registry *auraagent.Registry, db *sql.DB, recorder *deliveryRecorder) *workflow.CompletionNotifier {
	t.Helper()
	notifier, err := workflow.NewCompletionNotifier(&workflow.CompletionNotifierDeps{
		Runs:     runs,
		Registry: registry,
		Appender: &workflowSessionTextAppender{db: db},
		Deliver:  recorder.deliverer(),
		Logger:   slog.New(slog.DiscardHandler),
		Now:      func() time.Time { return time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC) },
	})
	if err != nil {
		t.Fatal(err)
	}
	return notifier
}

func completionFixture(t *testing.T) *completionHarness {
	t.Helper()
	base, runs, db := workflowToolFixture(t, "book_visit")
	return completionHarnessFor(t, base, runs, db)
}

func completionHarnessFor(t *testing.T, base *workflow.Controller, runs *workflow.Store, db *sql.DB) *completionHarness {
	t.Helper()
	visitor := caller(principal.ClassPublic, "discord:111")
	seedConversation(t, db, visitor.SessionID, visitor.PrincipalID)
	seedConversation(t, db, telegramTopicSession, "telegram:5")
	registry, err := auraagent.Build([]auraagent.Persona{{
		ID: "lobby", Name: "Lobby", Description: "front desk", Instructions: "You help visitors.", Audience: auraagent.AudiencePublic,
		Workflows: []string{"book_visit"}, Messages: map[string]string{auraagent.MessageWorkflowSucceeded: "Selesai: {goal}"},
	}}, definitionNames(), appconfig.BuiltinToolDefinitions(), nil)
	if err != nil {
		t.Fatal(err)
	}
	targeter := &workflowReplyTargeterAdapter{
		target: fixedTarget{sessions: map[string]access.HandoffTarget{
			visitor.SessionID:    {Channel: "discord", Account: "front", ChannelID: "333"},
			telegramTopicSession: {Channel: "telegram", Account: "pocket", ChannelID: "-1001234567890", ThreadID: "7"},
			emailThreadSession:   {Channel: "email", Account: "mail", ChannelID: "guest@example.test"},
		}},
	}
	controller, err := workflow.NewController(&workflow.ControllerDeps{
		Interpreter: base.Interpreter(),
		Runs:        runs,
		Registry:    registry,
		Targets:     []workflow.ReplyTargeter{targeter},
	})
	if err != nil {
		t.Fatal(err)
	}
	harness := &completionHarness{deliveryRecorder: &deliveryRecorder{}, controller: controller, runs: runs, db: db}
	notifier := newCompletionNotifier(t, runs, registry, db, harness.deliveryRecorder)
	harness.notifier = notifier
	return harness
}

func sessionTexts(t *testing.T, db *sql.DB, sessionID string) []string {
	t.Helper()
	rows, err := db.QueryContext(t.Context(), `SELECT payload_json FROM runtime_event WHERE session_id = ? ORDER BY sequence`, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var texts []string
	for rows.Next() {
		var payload string
		if err := rows.Scan(&payload); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(payload, "workflow book_visit") {
			texts = append(texts, payload)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return texts
}

func TestWorkflowStartRecordsTheReplyTarget(t *testing.T) {
	controller := completionFixture(t).controller
	visitor := caller(principal.ClassPublic, "discord:111")
	started, err := controller.Start(t.Context(), visitor, "book_visit", "")
	if err != nil {
		t.Fatal(err)
	}
	run, err := controller.Store().Run(t.Context(), started.RunID)
	if err != nil || run.Origin.Reply != (workflow.ReplyTarget{Channel: "discord", Account: "front", ChannelID: "333"}) {
		t.Fatalf("origin = %+v, %v", run.Origin, err)
	}
	stranger, err := controller.Start(t.Context(), caller(principal.ClassPublic, "discord:999"), "book_visit", "")
	if err != nil {
		t.Fatal(err)
	}
	if run, err := controller.Store().Run(t.Context(), stranger.RunID); err != nil || !run.Origin.Reply.IsZero() {
		t.Fatalf("a session without a known channel got a reply target: %+v, %v", run.Origin, err)
	}
}

func TestWorkflowStartRecordsATelegramReplyTarget(t *testing.T) {
	controller := completionFixture(t).controller
	telegramCaller := &workflow.Caller{SessionID: telegramTopicSession, PrincipalID: "telegram:5", PrincipalClass: principal.ClassPublic, AgentID: "lobby"}
	started, err := controller.Start(t.Context(), telegramCaller, "book_visit", "")
	if err != nil {
		t.Fatal(err)
	}
	if run, err := controller.Store().Run(t.Context(), started.RunID); err != nil || run.Origin.Reply != telegramTopicTarget {
		t.Fatalf("origin = %+v, %v", run.Origin, err)
	}
}

func TestWorkflowStartIgnoresUnsupportedReplyTargets(t *testing.T) {
	controller := completionFixture(t).controller
	emailCaller := &workflow.Caller{SessionID: emailThreadSession, PrincipalID: "email:guest", PrincipalClass: principal.ClassPublic, AgentID: "lobby"}
	started, err := controller.Start(t.Context(), emailCaller, "book_visit", "")
	if err != nil {
		t.Fatal(err)
	}
	if run, err := controller.Store().Run(t.Context(), started.RunID); err != nil || !run.Origin.Reply.IsZero() {
		t.Fatalf("an email session got a reply target: %+v, %v", run.Origin, err)
	}
}

func TestCompletionIsDeliveredOnceToTheOriginConversation(t *testing.T) {
	h := completionFixture(t)
	notifier, controller := h.notifier, h.controller
	visitor := caller(principal.ClassPublic, "discord:111")
	started, err := controller.Start(t.Context(), visitor, "book_visit", "")
	if err != nil {
		t.Fatal(err)
	}
	notifier.Sweep(t.Context())
	if len(h.sent) != 0 {
		t.Fatalf("a running workflow was announced: %+v", h.sent)
	}
	finishRun(t, controller.Store(), started.RunID, workflow.RunSucceeded)
	notifier.Sweep(t.Context())
	notifier.Sweep(t.Context())
	if len(h.sent) != 1 || h.sent[0] != (deliveredText{account: "front", channel: "333", text: "Selesai: Book a visit"}) {
		t.Fatalf("sent = %+v", h.sent)
	}
	if texts := sessionTexts(t, h.db, visitor.SessionID); len(texts) != 1 || !strings.Contains(texts[0], started.RunID) || !strings.Contains(texts[0], "succeeded") {
		t.Fatalf("session events = %v", texts)
	}
}

func TestFailedCompletionDeliveryRetriesThenGivesUp(t *testing.T) {
	h := completionFixture(t)
	notifier, controller := h.notifier, h.controller
	visitor := caller(principal.ClassPublic, "discord:111")
	started, err := controller.Start(t.Context(), visitor, "book_visit", "")
	if err != nil {
		t.Fatal(err)
	}
	finishRun(t, controller.Store(), started.RunID, workflow.RunFailed)
	h.failWith = errors.New("discord unavailable")
	notifier.Sweep(t.Context())
	h.failWith = nil
	notifier.Sweep(t.Context())
	if len(h.sent) != 1 || h.sent[0].text != "Sorry, Book a visit could not be completed." {
		t.Fatalf("sent after a retry = %+v", h.sent)
	}
	if texts := sessionTexts(t, h.db, visitor.SessionID); len(texts) != 1 {
		t.Fatalf("a retry appended the session event again: %v", texts)
	}
	again, err := controller.Start(t.Context(), visitor, "book_visit", "")
	if err != nil {
		t.Fatal(err)
	}
	finishRun(t, controller.Store(), again.RunID, workflow.RunCancelled)
	h.failWith = errors.New("discord unavailable")
	for range workflow.MaxCompletionAttempts + 2 {
		notifier.Sweep(t.Context())
	}
	if attempts, err := controller.Store().CompletionAttempts(t.Context(), again.RunID); err != nil || attempts != workflow.MaxCompletionAttempts {
		t.Fatalf("attempts = %d, %v", attempts, err)
	}
	h.failWith = nil
	notifier.Sweep(t.Context())
	if len(h.sent) != 1 {
		t.Fatalf("an abandoned completion was delivered: %+v", h.sent)
	}
}

func finishRun(t *testing.T, runs *workflow.Store, runID, status string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		run, err := runs.Run(t.Context(), runID)
		if err == nil && run.Status == workflow.RunSuspended {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("run %s never parked on its wait step: %+v, %v", runID, run, err)
		}
		time.Sleep(5 * time.Millisecond)
	}
	if err := runs.SetRunStatus(t.Context(), runID, status); err != nil {
		t.Fatal(err)
	}
}

type webResultTools struct{}

func (webResultTools) Invoke(context.Context, string, json.RawMessage) (workflow.ToolStepResult, error) {
	return workflow.ToolStepResult{Output: json.RawMessage(`{"page": "news"}`), Tainted: true}, nil
}

func resultSpec() *workflow.Spec {
	spec := waitingSpec("book_visit", "Book a visit")
	tool := "web_fetch"
	fetch := workflow.StepSpec{ID: "fetch", Executor: workflow.ExecutorSpec{Kind: workflow.KindTool, ToolID: &tool, ToolArgs: json.RawMessage(`{"url":"https://example.test"}`)}, Timeout: time.Minute}
	spec.Steps[0].DependsOn = []string{"fetch"}
	spec.Steps = append([]workflow.StepSpec{fetch}, spec.Steps...)
	return spec
}

func resultCompletionFixture(t *testing.T) *completionHarness {
	t.Helper()
	options := &workflow.Options{Logger: slog.New(slog.DiscardHandler), Tools: webResultTools{}}
	base, runs, db := workflowFixtureFor(t, options, []*workflow.Spec{resultSpec()}, "book_visit")
	return completionHarnessFor(t, base, runs, db)
}

const completionWithResult = "Selesai: Book a visit\n\n" + "(from web content, not verified)\n" + `{"page":"news"}`

func TestCompletionWithAResultIsDeliveredOnceToTheDiscordConversation(t *testing.T) {
	h := resultCompletionFixture(t)
	started, err := h.controller.Start(t.Context(), caller(principal.ClassPublic, "discord:111"), "book_visit", "")
	if err != nil {
		t.Fatal(err)
	}
	finishRun(t, h.runs, started.RunID, workflow.RunSucceeded)
	h.notifier.Sweep(t.Context())
	h.notifier.Sweep(t.Context())
	if want := (deliveredText{account: "front", channel: "333", text: completionWithResult}); len(h.sent) != 1 || h.sent[0] != want || len(h.telegram) != 0 {
		t.Fatalf("discord = %+v, telegram = %+v; want one delivery %+v", h.sent, h.telegram, want)
	}
}

func TestCompletionWithAResultIsDeliveredOnceToTheTelegramTopic(t *testing.T) {
	h := resultCompletionFixture(t)
	started := startTelegramRun(t, h)
	finishRun(t, h.runs, started.RunID, workflow.RunSucceeded)
	h.notifier.Sweep(t.Context())
	h.notifier.Sweep(t.Context())
	want := deliveredTelegramText{account: "pocket", chat: "-1001234567890", thread: "7", text: completionWithResult}
	if len(h.telegram) != 1 || h.telegram[0] != want || len(h.sent) != 0 {
		t.Fatalf("telegram = %+v, discord = %+v; want one delivery %+v", h.telegram, h.sent, want)
	}
}

func startTelegramRun(t *testing.T, h *completionHarness) workflow.RunState {
	t.Helper()
	telegramCaller := &workflow.Caller{SessionID: telegramTopicSession, PrincipalID: "telegram:5", PrincipalClass: principal.ClassPublic, AgentID: "lobby"}
	started, err := h.controller.Start(t.Context(), telegramCaller, "book_visit", "")
	if err != nil {
		t.Fatal(err)
	}
	return started
}

func TestTelegramCompletionIsDeliveredOnceToTheOriginTopic(t *testing.T) {
	h := completionFixture(t)
	started := startTelegramRun(t, h)
	finishRun(t, h.runs, started.RunID, workflow.RunSucceeded)
	var sweeps sync.WaitGroup
	for range 2 {
		sweeps.Add(1)
		go func() {
			defer sweeps.Done()
			h.notifier.Sweep(t.Context())
		}()
	}
	sweeps.Wait()
	want := deliveredTelegramText{account: "pocket", chat: "-1001234567890", thread: "7", text: "Selesai: Book a visit"}
	if len(h.telegram) != 1 || h.telegram[0] != want || len(h.sent) != 0 {
		t.Fatalf("telegram = %+v, discord = %+v; want one delivery %+v", h.telegram, h.sent, want)
	}
	if texts := sessionTexts(t, h.db, telegramTopicSession); len(texts) != 1 || !strings.Contains(texts[0], started.RunID) {
		t.Fatalf("session events = %v", texts)
	}
}

func TestFailedTelegramCompletionRetriesThenGivesUp(t *testing.T) {
	h := completionFixture(t)
	started := startTelegramRun(t, h)
	finishRun(t, h.runs, started.RunID, workflow.RunFailed)
	h.failWith = errors.New("telegram unavailable")
	for range workflow.MaxCompletionAttempts + 2 {
		h.notifier.Sweep(t.Context())
	}
	if attempts, err := h.runs.CompletionAttempts(t.Context(), started.RunID); err != nil || attempts != workflow.MaxCompletionAttempts {
		t.Fatalf("attempts = %d, %v; want %d", attempts, err, workflow.MaxCompletionAttempts)
	}
	var notified sql.NullString
	if err := h.db.QueryRowContext(t.Context(), `SELECT completion_notified_at FROM workflow_run WHERE id = ?`, started.RunID).Scan(&notified); err != nil || !notified.Valid {
		t.Fatalf("completion_notified_at = %+v, %v; want it set after the last attempt", notified, err)
	}
	h.failWith = nil
	h.notifier.Sweep(t.Context())
	if len(h.telegram) != 0 {
		t.Fatalf("an abandoned completion was delivered: %+v", h.telegram)
	}
}

func TestCompletionForAnUnconfiguredTelegramAccountFails(t *testing.T) {
	recorder := &deliveryRecorder{}
	deliverer := recorder.deliverer()
	for name, target := range map[string]workflow.ReplyTarget{
		"telegram account":    {Channel: "telegram", Account: "ghost", ChannelID: "5"},
		"discord account":     {Channel: "discord", Account: "ghost", ChannelID: "5"},
		"unsupported channel": {Channel: "email", Account: "mail", ChannelID: "guest"},
	} {
		t.Run(name, func(t *testing.T) {
			if err := deliverer.DeliverReply(t.Context(), &target, "text"); err == nil {
				t.Fatalf("DeliverReply(%+v) succeeded", target)
			}
		})
	}
	if len(recorder.sent) != 0 || len(recorder.telegram) != 0 {
		t.Fatalf("an unconfigured target was delivered: %+v %+v", recorder.sent, recorder.telegram)
	}
}
