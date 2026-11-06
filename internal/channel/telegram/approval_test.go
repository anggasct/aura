package telegram

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"
	"unicode/utf16"

	auraagent "github.com/anggasct/aura/internal/agent"
	"github.com/anggasct/aura/internal/channel/approval"
	"github.com/anggasct/aura/internal/config"
	"github.com/anggasct/aura/internal/effect"
	"github.com/anggasct/aura/internal/principal"
	"github.com/anggasct/aura/internal/tools/displaytest"
)

const (
	requesterID     = 555001
	forumTopic      = 7
	otherForumTopic = 8
	cardLifetime    = time.Minute
)

type inProcessTransport struct {
	handler http.Handler
}

func (t inProcessTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	recorder := httptest.NewRecorder()
	t.handler.ServeHTTP(recorder, req)
	return recorder.Result(), nil
}

type postedButton struct {
	text string
	data string
}

type postedCard struct {
	chatID    int64
	threadID  int64
	text      string
	buttons   []postedButton
	messageID int64
}

type standingRecorder struct {
	mu       sync.Mutex
	requests []approval.StandingGrantRequest
	err      error
}

func (s *standingRecorder) RecordStandingGrant(_ context.Context, grant *approval.StandingGrantRequest) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.requests = append(s.requests, *grant)
	return s.err
}

func (s *standingRecorder) recorded() []approval.StandingGrantRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.requests)
}

type routedLog struct {
	mu      sync.Mutex
	results []string
}

func (l *routedLog) observe(_ context.Context, result string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.results = append(l.results, result)
}

func (l *routedLog) recorded() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.results)
}

type approvalRig struct {
	*accessRig
	routed *routedLog
	grants *standingRecorder
}

type idleStore struct{}

func (idleStore) Load(context.Context, string) (next int64, found bool, err error) {
	return 0, false, nil
}

func (idleStore) Advance(context.Context, string, int64, time.Time) error {
	return nil
}

func (idleStore) Execute(context.Context, *effect.PrepareRequest, effect.Provider) (*effect.Intent, error) {
	return nil, errors.New("approval tests do not journal effects")
}

func (idleStore) EnsureSession(context.Context, string, string, string) error {
	return nil
}

func newBareHarness(t *testing.T) *harness {
	t.Helper()
	t.Setenv(testTokenEnv, testToken)
	return &harness{api: newFakeBotAPI(t), logs: &logCapture{}}
}

func newApprovalRig(t *testing.T, opts accessOpts) *approvalRig {
	t.Helper()
	routed := &routedLog{}
	grants := &standingRecorder{}
	opts.settings.ApprovalRouted = routed.observe
	opts.settings.Grants = grants
	opts.settings.StandingGrantTTL = 7 * 24 * time.Hour
	rig := assembleRig(t, newBareHarness(t), opts, idleStore{}, idleStore{}, idleStore{})
	rig.adapter.bot.http = &http.Client{Transport: inProcessTransport{handler: http.HandlerFunc(rig.h.api.serve)}}
	return &approvalRig{accessRig: rig, routed: routed, grants: grants}
}

func ownerTarget(dest destination) *approvalTarget {
	return &approvalTarget{
		binding: approval.Binding{Principal: principal.OwnerID, Class: principal.ClassOwner, Persona: testPersona, Actor: strconv.Itoa(ownerUserID), Scope: scopeOf(dest)},
		dest:    dest,
	}
}

func memberTarget(class principal.Class, dest destination) *approvalTarget {
	return &approvalTarget{
		binding: approval.Binding{Principal: "telegram:" + strconv.Itoa(requesterID), Class: class, Persona: testPersona, Actor: strconv.Itoa(requesterID), Scope: scopeOf(dest)},
		dest:    dest,
	}
}

func approvalPrompt(sessionID, principalID string) *approval.Prompt {
	return &approval.Prompt{
		ToolName:      "shell.exec",
		ToolVersion:   "1",
		SessionID:     sessionID,
		TurnID:        "turn-1",
		PrincipalID:   principalID,
		Arguments:     `{"command":"ls"}`,
		PolicyVersion: "v3",
		ReasonCode:    "approval_required",
		ExpiresAt:     time.Now().Add(cardLifetime),
	}
}

type decision struct {
	accepted bool
	err      error
}

func (r *approvalRig) decide(ctx context.Context, prompt *approval.Prompt) <-chan decision {
	out := make(chan decision, 1)
	go func() {
		accepted, err := r.adapter.Decide(ctx, prompt)
		out <- decision{accepted: accepted, err: err}
	}()
	return out
}

func (r *approvalRig) cards() []postedCard {
	var cards []postedCard
	r.h.api.mu.Lock()
	defer r.h.api.mu.Unlock()
	id := int64(700)
	for _, call := range r.h.api.calls {
		if call.method != "sendMessage" {
			continue
		}
		id++
		markup, _ := call.body["reply_markup"].(map[string]any)
		if markup == nil {
			continue
		}
		card := postedCard{messageID: id}
		chatID, _ := call.body["chat_id"].(float64)
		threadID, _ := call.body["message_thread_id"].(float64)
		card.chatID, card.threadID = int64(chatID), int64(threadID)
		card.text, _ = call.body["text"].(string)
		rows, _ := markup["inline_keyboard"].([]any)
		for _, row := range rows {
			buttons, _ := row.([]any)
			for _, raw := range buttons {
				button, _ := raw.(map[string]any)
				text, _ := button["text"].(string)
				data, _ := button["callback_data"].(string)
				card.buttons = append(card.buttons, postedButton{text: text, data: data})
			}
		}
		cards = append(cards, card)
	}
	return cards
}

func (c postedCard) data(t *testing.T, label string) string {
	t.Helper()
	for _, button := range c.buttons {
		if button.text == label {
			return button.data
		}
	}
	t.Fatalf("card has no %q button: %+v", label, c.buttons)
	return ""
}

func pressQuery(id string, from int64, dest destination, messageID int64, data string) *callbackQuery {
	chatType := chatTypePrivate
	if dest.chatID < 0 {
		chatType = chatTypeSupergroup
	}
	return &callbackQuery{
		ID:   id,
		From: &user{ID: from},
		Message: &message{
			MessageID:      messageID,
			Chat:           chat{ID: dest.chatID, Type: chatType},
			ThreadID:       dest.threadID,
			IsTopicMessage: dest.threadID > 0,
		},
		Data: data,
	}
}

func (r *approvalRig) press(t *testing.T, id string, from int64, card postedCard, data string) {
	t.Helper()
	r.pressAt(t, id, from, destination{chatID: card.chatID, threadID: card.threadID}, card.messageID, data)
}

func (r *approvalRig) pressAt(t *testing.T, id string, from int64, dest destination, messageID int64, data string) {
	t.Helper()
	r.adapter.handleCallback(t.Context(), pressQuery(id, from, dest, messageID, data))
	synctest.Wait()
}

func settled(result <-chan decision) (decision, bool) {
	select {
	case outcome := <-result:
		return outcome, true
	default:
		return decision{}, false
	}
}

func (r *approvalRig) mustCard(t *testing.T, want int) postedCard {
	t.Helper()
	synctest.Wait()
	cards := r.cards()
	if len(cards) != want {
		t.Fatalf("cards posted = %d, want %d", len(cards), want)
	}
	return cards[want-1]
}

func privateDest(chatID int64) destination {
	return destination{chatID: chatID}
}

func TestOwnerCardResolvesInTheSameChatAndTopic(t *testing.T) {
	cases := []struct {
		name   string
		dest   destination
		label  string
		accept bool
	}{
		{"private chat approve", privateDest(ownerUserID), "Approve", true},
		{"private chat reject", privateDest(ownerUserID), "Reject", false},
		{"group approve", destination{chatID: groupChatID}, "Approve", true},
		{"forum topic approve", destination{chatID: groupChatID, threadID: forumTopic}, "Approve", true},
		{"forum topic reject", destination{chatID: groupChatID, threadID: forumTopic}, "Reject", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rig := newApprovalRig(t, accessOpts{})
			synctest.Test(t, func(t *testing.T) {
				rig.adapter.rememberTarget("session-1", ownerTarget(tc.dest))
				result := rig.decide(t.Context(), approvalPrompt("session-1", principal.OwnerID))
				card := rig.mustCard(t, 1)
				if card.chatID != tc.dest.chatID || card.threadID != tc.dest.threadID {
					t.Fatalf("card posted to %d/%d, want %d/%d", card.chatID, card.threadID, tc.dest.chatID, tc.dest.threadID)
				}
				if !strings.Contains(card.text, "shell.exec") || !strings.Contains(card.text, "session-1") {
					t.Errorf("card text = %q", card.text)
				}
				if labels := []string{card.buttons[0].text, card.buttons[1].text}; len(card.buttons) != 2 || labels[0] != "Approve" || labels[1] != "Reject" {
					t.Fatalf("card buttons = %+v, want approve and reject only", card.buttons)
				}
				for _, button := range card.buttons {
					if len(button.data) > 64 {
						t.Errorf("callback data is %d bytes, over the Bot API limit", len(button.data))
					}
				}
				if _, done := settled(result); done {
					t.Fatal("decision settled before any press")
				}
				rig.press(t, "cb-1", ownerUserID, card, card.data(t, tc.label))
				outcome, done := settled(result)
				if !done || outcome.err != nil || outcome.accepted != tc.accept {
					t.Fatalf("decision = %+v (settled %v), want accepted=%v", outcome, done, tc.accept)
				}
				assertKeyboardRemoved(t, rig, card)
				assertAnswered(t, rig, 1)
				if got := rig.dropped(); len(got) != 0 {
					t.Errorf("resolving press produced drop records %v", got)
				}
				if rig.adapter.approvals.Len() != 0 {
					t.Error("pending approval kept after the decision")
				}
			})
		})
	}
}

func TestOwnerCardShowsEveryScenarioOfTheApprovalDisplayWithinTheMessageLimit(t *testing.T) {
	for _, scenario := range displaytest.Scenarios() {
		t.Run(scenario.Name, func(t *testing.T) {
			rig := newApprovalRig(t, accessOpts{})
			synctest.Test(t, func(t *testing.T) {
				rig.adapter.rememberTarget("session-1", ownerTarget(privateDest(ownerUserID)))
				prompt := approvalPrompt("session-1", principal.OwnerID)
				prompt.Arguments = scenario.Text(t)
				result := rig.decide(t.Context(), prompt)
				card := rig.mustCard(t, 1)
				scenario.Check(t, card.text, false)
				if units := len(utf16.Encode([]rune(card.text))); units > messageLimit {
					t.Errorf("card is %d UTF-16 code units, Telegram accepts %d", units, messageLimit)
				}
				rig.press(t, "cb-1", ownerUserID, card, card.data(t, "Reject"))
				if outcome, done := settled(result); !done || outcome.accepted {
					t.Fatalf("decision = %+v (settled %v), want a rejection", outcome, done)
				}
			})
		})
	}
}

func assertKeyboardRemoved(t *testing.T, rig *approvalRig, card postedCard) {
	t.Helper()
	edits := rig.h.api.bodies("editMessageReplyMarkup")
	if len(edits) != 1 {
		t.Fatalf("keyboard removals = %d, want 1", len(edits))
	}
	chatID, _ := edits[0]["chat_id"].(float64)
	messageID, _ := edits[0]["message_id"].(float64)
	markup, _ := edits[0]["reply_markup"].(map[string]any)
	rows, present := markup["inline_keyboard"].([]any)
	if int64(chatID) != card.chatID || int64(messageID) != card.messageID || !present || len(rows) != 0 {
		t.Errorf("keyboard removal = %v, want an empty keyboard on card %d/%d", edits[0], card.chatID, card.messageID)
	}
}

func assertAnswered(t *testing.T, rig *approvalRig, want int) {
	t.Helper()
	if got := len(rig.h.api.bodies("answerCallbackQuery")); got != want {
		t.Errorf("answered callbacks = %d, want %d", got, want)
	}
}

func TestCallbacksThatResolveNothingAreAnsweredAndDroppedOnce(t *testing.T) {
	topicDest := destination{chatID: groupChatID, threadID: forumTopic}
	cases := []struct {
		name   string
		dest   destination
		press  func(t *testing.T, rig *approvalRig, card postedCard)
		repeat bool
	}{
		{
			name: "forged mac",
			dest: privateDest(ownerUserID),
			press: func(t *testing.T, rig *approvalRig, card postedCard) {
				rig.press(t, "cb-bad", ownerUserID, card, flipLast(card.data(t, "Approve")))
			},
		},
		{
			name: "forged action",
			dest: privateDest(ownerUserID),
			press: func(t *testing.T, rig *approvalRig, card postedCard) {
				rig.press(t, "cb-bad", ownerUserID, card, strings.Replace(card.data(t, "Reject"), ":r:", ":a:", 1))
			},
		},
		{
			name: "wrong presser",
			dest: destination{chatID: groupChatID},
			press: func(t *testing.T, rig *approvalRig, card postedCard) {
				rig.press(t, "cb-bad", strangerID, card, card.data(t, "Approve"))
			},
		},
		{
			name: "wrong chat",
			dest: privateDest(ownerUserID),
			press: func(t *testing.T, rig *approvalRig, card postedCard) {
				rig.pressAt(t, "cb-bad", ownerUserID, destination{chatID: groupChatID}, card.messageID, card.data(t, "Approve"))
			},
		},
		{
			name: "wrong topic",
			dest: topicDest,
			press: func(t *testing.T, rig *approvalRig, card postedCard) {
				rig.pressAt(t, "cb-bad", ownerUserID, destination{chatID: groupChatID, threadID: otherForumTopic}, card.messageID, card.data(t, "Approve"))
			},
		},
		{
			name: "topic card pressed outside the topic",
			dest: topicDest,
			press: func(t *testing.T, rig *approvalRig, card postedCard) {
				rig.pressAt(t, "cb-bad", ownerUserID, destination{chatID: groupChatID}, card.messageID, card.data(t, "Approve"))
			},
		},
		{
			name: "unknown token",
			dest: privateDest(ownerUserID),
			press: func(t *testing.T, rig *approvalRig, card postedCard) {
				rig.press(t, "cb-bad", ownerUserID, card, approval.CallbackData("0123456789abcdef", approval.ActionApprove, "0123456789abcdef"))
			},
		},
		{
			name: "unrelated callback data",
			dest: privateDest(ownerUserID),
			press: func(t *testing.T, rig *approvalRig, card postedCard) {
				rig.press(t, "cb-bad", ownerUserID, card, "menu:open")
			},
		},
		{
			name: "standing action without the offer",
			dest: privateDest(ownerUserID),
			press: func(t *testing.T, rig *approvalRig, card postedCard) {
				data := card.data(t, "Approve")
				rig.press(t, "cb-bad", ownerUserID, card, strings.Replace(data, ":a:", ":s:", 1))
			},
		},
		{
			name: "presser is a bot",
			dest: privateDest(ownerUserID),
			press: func(t *testing.T, rig *approvalRig, card postedCard) {
				query := pressQuery("cb-bad", ownerUserID, privateDest(ownerUserID), card.messageID, card.data(t, "Approve"))
				query.From.IsBot = true
				rig.adapter.handleCallback(t.Context(), query)
				synctest.Wait()
			},
		},
		{
			name: "callback without a message",
			dest: privateDest(ownerUserID),
			press: func(t *testing.T, rig *approvalRig, card postedCard) {
				query := pressQuery("cb-bad", ownerUserID, privateDest(ownerUserID), card.messageID, card.data(t, "Approve"))
				query.Message = nil
				rig.adapter.handleCallback(t.Context(), query)
				synctest.Wait()
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rig := newApprovalRig(t, accessOpts{})
			synctest.Test(t, func(t *testing.T) {
				rig.adapter.rememberTarget("session-1", ownerTarget(tc.dest))
				result := rig.decide(t.Context(), approvalPrompt("session-1", principal.OwnerID))
				card := rig.mustCard(t, 1)
				tc.press(t, rig, card)
				if _, done := settled(result); done {
					t.Fatal("a rejected callback settled the decision")
				}
				assertAnswered(t, rig, 1)
				if got := rig.dropped(); !slices.Equal(got, []string{string(dropCallbackRejected)}) {
					t.Fatalf("drop records = %v, want exactly one callback_rejected", got)
				}
				if rig.adapter.approvals.Len() != 1 {
					t.Error("a rejected callback consumed the pending approval")
				}
				rig.press(t, "cb-good", ownerUserID, card, card.data(t, "Approve"))
				if outcome, done := settled(result); !done || !outcome.accepted || outcome.err != nil {
					t.Fatalf("valid press after the rejection = %+v (settled %v), want accepted", outcome, done)
				}
			})
		})
	}
}

func flipLast(data string) string {
	last := data[len(data)-1]
	if last == 'a' {
		return data[:len(data)-1] + "b"
	}
	return data[:len(data)-1] + "a"
}

func TestReplayedAndDuplicateCallbacksAreDropped(t *testing.T) {
	rig := newApprovalRig(t, accessOpts{})
	synctest.Test(t, func(t *testing.T) {
		rig.adapter.rememberTarget("session-1", ownerTarget(privateDest(ownerUserID)))
		result := rig.decide(t.Context(), approvalPrompt("session-1", principal.OwnerID))
		card := rig.mustCard(t, 1)
		rig.press(t, "cb-1", ownerUserID, card, card.data(t, "Approve"))
		if outcome, done := settled(result); !done || !outcome.accepted {
			t.Fatalf("first press = %+v (settled %v), want accepted", outcome, done)
		}
		rig.press(t, "cb-2", ownerUserID, card, card.data(t, "Reject"))
		rig.press(t, "cb-1", ownerUserID, card, card.data(t, "Approve"))
		assertAnswered(t, rig, 3)
		if got := rig.dropped(); !slices.Equal(got, []string{string(dropCallbackRejected), string(dropCallbackRejected)}) {
			t.Errorf("drop records = %v, want one per replayed press", got)
		}
		if got := len(rig.h.api.bodies("editMessageReplyMarkup")); got != 1 {
			t.Errorf("keyboard removals = %d, want 1", got)
		}
	})
}

func TestCallbackDropLogsCarryNoContentOrIdentifiers(t *testing.T) {
	rig := newApprovalRig(t, accessOpts{})
	synctest.Test(t, func(t *testing.T) {
		rig.adapter.rememberTarget("session-1", ownerTarget(privateDest(ownerUserID)))
		rig.decide(t.Context(), approvalPrompt("session-1", principal.OwnerID))
		card := rig.mustCard(t, 1)
		data := card.data(t, "Approve")
		rig.press(t, "cb-secret-id", strangerID, card, data)
		logs := rig.h.logs.String()
		if !strings.Contains(logs, "callback_rejected") {
			t.Fatalf("logs carry no callback_rejected record: %s", logs)
		}
		for _, leaked := range []string{data, "cb-secret-id", strconv.Itoa(strangerID), strconv.Itoa(ownerUserID), testToken, "shell.exec"} {
			if strings.Contains(logs, leaked) {
				t.Errorf("logs leak %q", leaked)
			}
		}
	})
}

func TestCardExpiryRemovesTheKeyboardAndRejectsLatePresses(t *testing.T) {
	rig := newApprovalRig(t, accessOpts{})
	synctest.Test(t, func(t *testing.T) {
		rig.adapter.rememberTarget("session-1", ownerTarget(privateDest(ownerUserID)))
		result := rig.decide(t.Context(), approvalPrompt("session-1", principal.OwnerID))
		card := rig.mustCard(t, 1)
		time.Sleep(2 * cardLifetime)
		synctest.Wait()
		outcome, done := settled(result)
		if !done || outcome.accepted || outcome.err != nil {
			t.Fatalf("expired decision = %+v (settled %v), want a plain denial", outcome, done)
		}
		assertKeyboardRemoved(t, rig, card)
		rig.press(t, "cb-late", ownerUserID, card, card.data(t, "Approve"))
		assertAnswered(t, rig, 1)
		if got := rig.dropped(); !slices.Equal(got, []string{string(dropCallbackRejected)}) {
			t.Errorf("drop records = %v, want the late press dropped", got)
		}
	})
}

func TestCancelledDecisionRemovesTheKeyboard(t *testing.T) {
	rig := newApprovalRig(t, accessOpts{})
	synctest.Test(t, func(t *testing.T) {
		rig.adapter.rememberTarget("session-1", ownerTarget(privateDest(ownerUserID)))
		ctx, cancel := context.WithCancel(t.Context())
		result := rig.decide(ctx, approvalPrompt("session-1", principal.OwnerID))
		card := rig.mustCard(t, 1)
		cancel()
		synctest.Wait()
		outcome, done := settled(result)
		if !done || outcome.accepted || !errors.Is(outcome.err, context.Canceled) {
			t.Fatalf("cancelled decision = %+v (settled %v), want a denial carrying context.Canceled", outcome, done)
		}
		assertKeyboardRemoved(t, rig, card)
		rig.press(t, "cb-late", ownerUserID, card, card.data(t, "Approve"))
		if got := rig.dropped(); !slices.Equal(got, []string{string(dropCallbackRejected)}) {
			t.Errorf("drop records = %v, want the late press dropped", got)
		}
	})
}

func TestDecideFailsClosedWithoutACard(t *testing.T) {
	rig := newApprovalRig(t, accessOpts{})
	synctest.Test(t, func(t *testing.T) {
		rig.adapter.rememberTarget("session-1", ownerTarget(privateDest(ownerUserID)))
		cases := []struct {
			name   string
			prompt *approval.Prompt
		}{
			{"unknown session", approvalPrompt("session-404", principal.OwnerID)},
			{"principal does not own the session", approvalPrompt("session-1", "telegram:1")},
			{"already expired", func() *approval.Prompt {
				prompt := approvalPrompt("session-1", principal.OwnerID)
				prompt.ExpiresAt = time.Now().Add(-time.Minute)
				return prompt
			}()},
		}
		for _, tc := range cases {
			accepted, err := rig.adapter.Decide(t.Context(), tc.prompt)
			if accepted || err != nil {
				t.Errorf("%s: Decide() = %v, %v; want a plain denial", tc.name, accepted, err)
			}
		}
		if got := len(rig.cards()); got != 0 {
			t.Errorf("cards posted = %d, want none", got)
		}
	})
}

func TestDecideRejectsInvalidArguments(t *testing.T) {
	rig := newApprovalRig(t, accessOpts{})
	blank := approvalPrompt("session-1", principal.OwnerID)
	blank.ToolName = " "
	var missing context.Context
	cases := []struct {
		name   string
		decide func() (bool, error)
	}{
		{"nil context", func() (bool, error) {
			return rig.adapter.Decide(missing, approvalPrompt("session-1", principal.OwnerID))
		}},
		{"nil prompt", func() (bool, error) { return rig.adapter.Decide(t.Context(), nil) }},
		{"blank tool", func() (bool, error) { return rig.adapter.Decide(t.Context(), blank) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			accepted, err := tc.decide()
			code, ok := CodeOf(err)
			if accepted || !ok || code != ErrorCodeInvalidArgument {
				t.Errorf("Decide() = %v, %v; want a denial with invalid_argument", accepted, err)
			}
		})
	}
}

func TestOwnerCardThatCannotBePostedDenies(t *testing.T) {
	rig := newApprovalRig(t, accessOpts{})
	synctest.Test(t, func(t *testing.T) {
		rig.adapter.rememberTarget("session-1", ownerTarget(privateDest(ownerUserID)))
		rig.h.api.failNextOn("sendMessage", apiFailure(http.StatusForbidden, 0))
		accepted, err := rig.adapter.Decide(t.Context(), approvalPrompt("session-1", principal.OwnerID))
		if accepted || err != nil {
			t.Errorf("Decide() = %v, %v; want a plain denial", accepted, err)
		}
		if rig.adapter.approvals.Len() != 0 {
			t.Error("pending approval kept after the card failed")
		}
		if strings.Contains(rig.h.logs.String(), testToken) {
			t.Error("bot token leaked into logs")
		}
	})
}

func routedRig(t *testing.T, definition auraagent.Definition, class principal.Class) (*approvalRig, *approvalTarget) {
	t.Helper()
	rig := newApprovalRig(t, accessOpts{definition: definition})
	return rig, memberTarget(class, destination{chatID: groupChatID, threadID: forumTopic})
}

func routedDefinition() auraagent.Definition {
	return auraagent.Definition{
		ID:              testPersona,
		Name:            "Main",
		Audience:        config.AudienceTrusted,
		ApprovalTimeout: 30 * time.Second,
		Messages: map[string]string{
			config.MessageWaitingForOwner: "waiting for the owner",
			config.MessageApprovalDenied:  "the owner said no",
			config.MessageApprovalTimeout: "the owner did not answer",
		},
	}
}

func noticesTo(rig *approvalRig, dest destination) []string {
	var texts []string
	for _, call := range rig.h.api.callsOf("sendMessage") {
		if call.body["reply_markup"] != nil {
			continue
		}
		chatID, _ := call.body["chat_id"].(float64)
		threadID, _ := call.body["message_thread_id"].(float64)
		if int64(chatID) == dest.chatID && int64(threadID) == dest.threadID {
			text, _ := call.body["text"].(string)
			texts = append(texts, text)
		}
	}
	return texts
}

func TestNonOwnerRequestsRouteTheCardToTheOwnerPrivateChat(t *testing.T) {
	cases := []struct {
		name       string
		class      principal.Class
		label      string
		accept     bool
		wantNotice []string
		wantRouted []string
	}{
		{"owner approves", principal.ClassPublic, "Approve", true, []string{"waiting for the owner"}, []string{approval.RoutedApproved}},
		{"owner rejects", principal.ClassPublic, "Reject", false, []string{"waiting for the owner", "the owner said no"}, []string{approval.RoutedRejected}},
		{"trusted requester approves once", principal.ClassTrusted, "Approve", true, []string{"waiting for the owner"}, []string{approval.RoutedApproved}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rig, target := routedRig(t, routedDefinition(), tc.class)
			synctest.Test(t, func(t *testing.T) {
				rig.adapter.rememberTarget("session-9", target)
				result := rig.decide(t.Context(), approvalPrompt("session-9", target.binding.Principal))
				card := rig.mustCard(t, 1)
				if card.chatID != ownerUserID || card.threadID != 0 {
					t.Fatalf("card posted to %d/%d, want the owner's private chat", card.chatID, card.threadID)
				}
				if !strings.Contains(card.text, "persona: Main (main)") {
					t.Errorf("card text = %q, want the persona label", card.text)
				}
				rig.press(t, "cb-requester", requesterID, card, card.data(t, "Approve"))
				if _, done := settled(result); done {
					t.Fatal("the requester decided its own approval")
				}
				rig.press(t, "cb-owner", ownerUserID, card, card.data(t, tc.label))
				outcome, done := settled(result)
				if !done || outcome.err != nil || outcome.accepted != tc.accept {
					t.Fatalf("decision = %+v (settled %v), want accepted=%v", outcome, done, tc.accept)
				}
				if got := noticesTo(rig, target.dest); !slices.Equal(got, tc.wantNotice) {
					t.Errorf("requester notices = %q, want %q", got, tc.wantNotice)
				}
				if got := rig.routed.recorded(); !slices.Equal(got, tc.wantRouted) {
					t.Errorf("routed observations = %v, want %v", got, tc.wantRouted)
				}
				assertKeyboardRemoved(t, rig, card)
				if len(rig.grants.recorded()) != 0 {
					t.Error("a one-off approval recorded a standing grant")
				}
			})
		})
	}
}

func TestNonOwnerRequestTimesOutWithTheOwnerApprovalTimeout(t *testing.T) {
	rig, target := routedRig(t, routedDefinition(), principal.ClassPublic)
	synctest.Test(t, func(t *testing.T) {
		rig.adapter.rememberTarget("session-9", target)
		prompt := approvalPrompt("session-9", target.binding.Principal)
		prompt.ExpiresAt = time.Now().Add(time.Hour)
		result := rig.decide(t.Context(), prompt)
		card := rig.mustCard(t, 1)
		time.Sleep(29 * time.Second)
		synctest.Wait()
		if _, done := settled(result); done {
			t.Fatal("decision settled before the owner timeout")
		}
		time.Sleep(2 * time.Second)
		synctest.Wait()
		outcome, done := settled(result)
		code, coded := CodeOf(outcome.err)
		if !done || outcome.accepted || !coded || code != ErrorCodeApprovalTimeout {
			t.Fatalf("decision = %+v (settled %v), want a denial with approval_timeout", outcome, done)
		}
		if got := noticesTo(rig, target.dest); !slices.Equal(got, []string{"waiting for the owner", "the owner did not answer"}) {
			t.Errorf("requester notices = %q", got)
		}
		if got := rig.routed.recorded(); !slices.Equal(got, []string{approval.RoutedTimeout}) {
			t.Errorf("routed observations = %v", got)
		}
		assertKeyboardRemoved(t, rig, card)
	})
}

func TestRoutedWaitIsCappedByTheBrokerExpiry(t *testing.T) {
	rig, target := routedRig(t, routedDefinition(), principal.ClassPublic)
	synctest.Test(t, func(t *testing.T) {
		rig.adapter.rememberTarget("session-9", target)
		prompt := approvalPrompt("session-9", target.binding.Principal)
		prompt.ExpiresAt = time.Now().Add(5 * time.Second)
		result := rig.decide(t.Context(), prompt)
		rig.mustCard(t, 1)
		time.Sleep(6 * time.Second)
		synctest.Wait()
		outcome, done := settled(result)
		if code, _ := CodeOf(outcome.err); !done || outcome.accepted || code != ErrorCodeApprovalTimeout {
			t.Fatalf("decision = %+v (settled %v), want a timeout at the broker expiry", outcome, done)
		}
	})
}

func TestTrustedRequestOffersAndRecordsAStandingGrant(t *testing.T) {
	rig, target := routedRig(t, routedDefinition(), principal.ClassTrusted)
	synctest.Test(t, func(t *testing.T) {
		rig.adapter.rememberTarget("session-9", target)
		result := rig.decide(t.Context(), approvalPrompt("session-9", target.binding.Principal))
		card := rig.mustCard(t, 1)
		if len(card.buttons) != 3 || card.buttons[0].text != "Approve" || card.buttons[1].text != "Allow 7 days" || card.buttons[2].text != "Reject" {
			t.Fatalf("card buttons = %+v, want approve, standing grant and reject", card.buttons)
		}
		rig.press(t, "cb-owner", ownerUserID, card, card.data(t, "Allow 7 days"))
		outcome, done := settled(result)
		if !done || outcome.err != nil || !outcome.accepted {
			t.Fatalf("decision = %+v (settled %v), want accepted", outcome, done)
		}
		want := approval.StandingGrantRequest{
			PrincipalID: target.binding.Principal,
			AgentID:     testPersona,
			ToolName:    "shell.exec",
			OwnerID:     "telegram:" + strconv.Itoa(ownerUserID),
			TTL:         7 * 24 * time.Hour,
		}
		if got := rig.grants.recorded(); len(got) != 1 || got[0] != want {
			t.Errorf("recorded grants = %+v, want %+v", got, want)
		}
		if got := rig.routed.recorded(); !slices.Equal(got, []string{approval.RoutedStanding}) {
			t.Errorf("routed observations = %v", got)
		}
	})
}

func TestStandingGrantIsOfferedOnlyToTrustedRequestsWithAStore(t *testing.T) {
	cases := []struct {
		name  string
		class principal.Class
		reset func(settings *AccessSettings)
	}{
		{"public requester", principal.ClassPublic, func(*AccessSettings) {}},
		{"no ttl", principal.ClassTrusted, func(settings *AccessSettings) { settings.StandingGrantTTL = 0 }},
		{"no store", principal.ClassTrusted, func(settings *AccessSettings) { settings.Grants = nil }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rig, target := routedRig(t, routedDefinition(), tc.class)
			tc.reset(&rig.adapter.access)
			synctest.Test(t, func(t *testing.T) {
				rig.adapter.rememberTarget("session-9", target)
				result := rig.decide(t.Context(), approvalPrompt("session-9", target.binding.Principal))
				card := rig.mustCard(t, 1)
				if len(card.buttons) != 2 {
					t.Fatalf("card buttons = %+v, want no standing grant", card.buttons)
				}
				forged := strings.Replace(card.data(t, "Approve"), ":a:", ":s:", 1)
				rig.press(t, "cb-forged", ownerUserID, card, forged)
				if _, done := settled(result); done {
					t.Fatal("a forged standing press settled the decision")
				}
				rig.press(t, "cb-owner", ownerUserID, card, card.data(t, "Reject"))
				if outcome, _ := settled(result); outcome.accepted {
					t.Error("rejection accepted")
				}
				if len(rig.grants.recorded()) != 0 {
					t.Error("a grant was recorded")
				}
			})
		})
	}
}

func TestStandingGrantRecordFailureStillApproves(t *testing.T) {
	rig, target := routedRig(t, routedDefinition(), principal.ClassTrusted)
	rig.grants.err = errors.New("store down")
	synctest.Test(t, func(t *testing.T) {
		rig.adapter.rememberTarget("session-9", target)
		result := rig.decide(t.Context(), approvalPrompt("session-9", target.binding.Principal))
		card := rig.mustCard(t, 1)
		rig.press(t, "cb-owner", ownerUserID, card, card.data(t, "Allow 7 days"))
		if outcome, done := settled(result); !done || !outcome.accepted || outcome.err != nil {
			t.Fatalf("decision = %+v (settled %v), want accepted", outcome, done)
		}
		if !strings.Contains(rig.h.logs.String(), "standing grant record failed") {
			t.Error("a failed grant record left no log")
		}
	})
}

func TestUndeliverableOwnerCardFailsWithApprovalUnavailable(t *testing.T) {
	cases := []struct {
		name  string
		setup func(t *testing.T, rig *approvalRig)
	}{
		{"owner never started the bot", func(_ *testing.T, rig *approvalRig) {
			rig.h.api.failNextOn("sendMessage", describedFailure(http.StatusForbidden, "Forbidden: bot can't initiate conversation with a user", 0))
		}},
		{"bot api failure", func(_ *testing.T, rig *approvalRig) {
			rig.h.api.failNextOn("sendMessage", apiFailure(http.StatusInternalServerError, 0))
		}},
		{"no telegram owner identity", func(t *testing.T, rig *approvalRig) {
			owners, err := principal.NewResolver([]string{"discord:123456789012345678"})
			if err != nil {
				t.Fatalf("NewResolver: %v", err)
			}
			rig.adapter.SetPrincipalResolver(owners)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rig, target := routedRig(t, routedDefinition(), principal.ClassTrusted)
			tc.setup(t, rig)
			synctest.Test(t, func(t *testing.T) {
				rig.adapter.rememberTarget("session-9", target)
				accepted, err := rig.adapter.Decide(t.Context(), approvalPrompt("session-9", target.binding.Principal))
				if code, ok := CodeOf(err); accepted || !ok || code != ErrorCodeApprovalUnavailable {
					t.Fatalf("Decide() = %v, %v; want a denial with approval_unavailable", accepted, err)
				}
				if got := rig.routed.recorded(); !slices.Equal(got, []string{approval.RoutedUnavailable}) {
					t.Errorf("routed observations = %v", got)
				}
				if got := noticesTo(rig, target.dest); len(got) != 0 {
					t.Errorf("requester notices = %q, want none when the card never posted", got)
				}
				if rig.adapter.approvals.Len() != 0 {
					t.Error("pending approval kept after the card failed")
				}
			})
		})
	}
}

func TestMissingBotTokenDeniesWithoutPosting(t *testing.T) {
	rig, target := routedRig(t, routedDefinition(), principal.ClassPublic)
	t.Setenv(testTokenEnv, "")
	synctest.Test(t, func(t *testing.T) {
		rig.adapter.rememberTarget("session-9", target)
		rig.adapter.rememberTarget("session-1", ownerTarget(privateDest(ownerUserID)))
		accepted, err := rig.adapter.Decide(t.Context(), approvalPrompt("session-9", target.binding.Principal))
		if code, ok := CodeOf(err); accepted || !ok || code != ErrorCodeApprovalUnavailable {
			t.Errorf("routed Decide() = %v, %v; want approval_unavailable", accepted, err)
		}
		accepted, err = rig.adapter.Decide(t.Context(), approvalPrompt("session-1", principal.OwnerID))
		if accepted || err != nil {
			t.Errorf("owner Decide() = %v, %v; want a plain denial", accepted, err)
		}
		if got := len(rig.h.api.callsOf("sendMessage")); got != 0 {
			t.Errorf("messages sent = %d, want none without a token", got)
		}
	})
}

func TestApprovalTargetsStayBounded(t *testing.T) {
	rig := newApprovalRig(t, accessOpts{})
	for i := range maxTrackedTargets + 100 {
		rig.adapter.rememberTarget("session-"+strconv.Itoa(i), ownerTarget(privateDest(ownerUserID)))
	}
	rig.adapter.targetsMu.Lock()
	tracked := len(rig.adapter.targets)
	rig.adapter.targetsMu.Unlock()
	if tracked > maxTrackedTargets {
		t.Errorf("tracked targets = %d, want at most %d", tracked, maxTrackedTargets)
	}
	if !rig.adapter.HoldsApprovalTarget("session-" + strconv.Itoa(maxTrackedTargets+99)) {
		t.Error("the newest target was evicted")
	}
}

func TestHoldsApprovalTarget(t *testing.T) {
	rig := newApprovalRig(t, accessOpts{})
	rig.adapter.rememberTarget("session-1", ownerTarget(privateDest(ownerUserID)))
	if !rig.adapter.HoldsApprovalTarget("session-1") || rig.adapter.HoldsApprovalTarget("session-2") {
		t.Error("HoldsApprovalTarget does not follow remembered targets")
	}
}
