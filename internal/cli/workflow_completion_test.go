package cli

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	auraagent "github.com/anggasct/aura/internal/agent"
	"github.com/anggasct/aura/internal/channel/discord"
	"github.com/anggasct/aura/internal/config"
	"github.com/anggasct/aura/internal/principal"
	"github.com/anggasct/aura/internal/store"
	toolsbuiltin "github.com/anggasct/aura/internal/tools/builtin"
	"github.com/anggasct/aura/internal/workflow"
)

type fixedTarget struct {
	sessions map[string]discord.HandoffTarget
}

func (f fixedTarget) ReplyTarget(sessionID string) (discord.HandoffTarget, bool) {
	target, ok := f.sessions[sessionID]
	return target, ok
}

type completionHarness struct {
	notifier   *workflowCompletionNotifier
	controller *workflowToolController
	sent       []deliveredText
	failWith   error
}

func completionFixture(t *testing.T) *completionHarness {
	t.Helper()
	controller, runs, db := workflowToolFixture(t, "book_visit")
	visitor := caller(principal.ClassPublic, "discord:111")
	if err := store.NewSessionService(db).Create(t.Context(), &store.Session{ID: visitor.SessionID, OwnerID: visitor.PrincipalID, Metadata: []byte(`{}`), CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	controller.targets = []replyTargeter{fixedTarget{sessions: map[string]discord.HandoffTarget{visitor.SessionID: {Account: "front", ChannelID: "333"}}}}
	registry, err := auraagent.Build([]config.AgentDefinition{{
		ID: "lobby", Name: "Lobby", Description: "front desk", Instructions: "You help visitors.", Audience: config.AudiencePublic,
		Workflows: []string{"book_visit"}, Messages: map[string]string{config.MessageWorkflowSucceeded: "Selesai: {goal}"},
	}}, toolsbuiltin.DefinitionNames(), nil)
	if err != nil {
		t.Fatal(err)
	}
	harness := &completionHarness{controller: controller}
	cfg := &config.Config{Channels: config.Channels{Discord: config.Discord{Accounts: []config.DiscordAccount{{Name: "front"}}}}}
	harness.notifier = &workflowCompletionNotifier{
		cfg: cfg, db: db, runs: runs, registry: registry, logger: slog.New(slog.DiscardHandler),
		now: func() time.Time { return time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC) },
		deliver: func(_ context.Context, account *config.DiscordAccount, channelID, text string) error {
			if harness.failWith != nil {
				return harness.failWith
			}
			harness.sent = append(harness.sent, deliveredText{account: account.Name, channel: channelID, text: text})
			return nil
		},
	}
	return harness
}

func sessionTexts(t *testing.T, notifier *workflowCompletionNotifier, sessionID string) []string {
	t.Helper()
	rows, err := notifier.db.QueryContext(t.Context(), `SELECT payload_json FROM runtime_event WHERE session_id = ? ORDER BY sequence`, sessionID)
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
	run, err := controller.runs.Run(t.Context(), started.RunID)
	if err != nil || run.Origin.ReplyAccount != "front" || run.Origin.ReplyChannelID != "333" {
		t.Fatalf("origin = %+v, %v", run.Origin, err)
	}
	stranger, err := controller.Start(t.Context(), caller(principal.ClassPublic, "discord:999"), "book_visit", "")
	if err != nil {
		t.Fatal(err)
	}
	if run, err := controller.runs.Run(t.Context(), stranger.RunID); err != nil || run.Origin.ReplyChannelID != "" {
		t.Fatalf("a session without a known channel got a reply target: %+v, %v", run.Origin, err)
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
	notifier.sweep(t.Context())
	if len(h.sent) != 0 {
		t.Fatalf("a running workflow was announced: %+v", h.sent)
	}
	finishRun(t, controller.runs, started.RunID, workflow.RunSucceeded)
	notifier.sweep(t.Context())
	notifier.sweep(t.Context())
	if len(h.sent) != 1 || h.sent[0] != (deliveredText{account: "front", channel: "333", text: "Selesai: Book a visit"}) {
		t.Fatalf("sent = %+v", h.sent)
	}
	if texts := sessionTexts(t, notifier, visitor.SessionID); len(texts) != 1 || !strings.Contains(texts[0], started.RunID) || !strings.Contains(texts[0], "succeeded") {
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
	finishRun(t, controller.runs, started.RunID, workflow.RunFailed)
	h.failWith = errors.New("discord unavailable")
	notifier.sweep(t.Context())
	h.failWith = nil
	notifier.sweep(t.Context())
	if len(h.sent) != 1 || h.sent[0].text != "Sorry, Book a visit could not be completed." {
		t.Fatalf("sent after a retry = %+v", h.sent)
	}
	if texts := sessionTexts(t, notifier, visitor.SessionID); len(texts) != 1 {
		t.Fatalf("a retry appended the session event again: %v", texts)
	}
	again, err := controller.Start(t.Context(), visitor, "book_visit", "")
	if err != nil {
		t.Fatal(err)
	}
	finishRun(t, controller.runs, again.RunID, workflow.RunCancelled)
	h.failWith = errors.New("discord unavailable")
	for range workflow.MaxCompletionAttempts + 2 {
		notifier.sweep(t.Context())
	}
	if attempts, err := controller.runs.CompletionAttempts(t.Context(), again.RunID); err != nil || attempts != workflow.MaxCompletionAttempts {
		t.Fatalf("attempts = %d, %v", attempts, err)
	}
	h.failWith = nil
	notifier.sweep(t.Context())
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
