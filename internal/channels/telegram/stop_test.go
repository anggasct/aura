package telegram

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/anggasct/aura/internal/channels/kit/approval"
	"github.com/anggasct/aura/internal/channels/kit/status"
	"github.com/anggasct/aura/internal/platform/command"
	"github.com/anggasct/aura/internal/platform/principal"
	runtimechannelhost "github.com/anggasct/aura/internal/platform/turn/channelhost"
)

type spyStopTurns struct {
	mu          sync.Mutex
	activeTurn  string
	cancelTurns [][2]string
	cancelAll   []string
}

func (s *spyStopTurns) CancelActive(_ context.Context, sessionID string) (turnID string, cancelled bool, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cancelAll = append(s.cancelAll, sessionID)
	if s.activeTurn == "" {
		return "", false, nil
	}
	cancelledID := s.activeTurn
	s.activeTurn = ""
	return cancelledID, true, nil
}

func (s *spyStopTurns) CancelTurn(_ context.Context, sessionID, turnID string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cancelTurns = append(s.cancelTurns, [2]string{sessionID, turnID})
	if s.activeTurn != "" && s.activeTurn == turnID {
		s.activeTurn = ""
		return true, nil
	}
	return false, nil
}

func (s *spyStopTurns) Activity(string) (activeTurnID string, queued int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.activeTurn, 0
}

func (s *spyStopTurns) recordedTurns() [][2]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.cancelTurns)
}

func (s *spyStopTurns) recordedActive() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.cancelAll)
}

func newStopDispatcher(t *testing.T, turns command.Turns) *command.Dispatcher {
	t.Helper()
	registry, err := command.New(CommandBinding())
	if err != nil {
		t.Fatalf("command.New: %v", err)
	}
	dispatcher, err := command.NewDispatcher(registry, command.Ports{
		Sessions: &commandSessions{},
		Turns:    turns,
	}, nil)
	if err != nil {
		t.Fatalf("NewDispatcher: %v", err)
	}
	return dispatcher
}

func stopPresenceRequest(t *testing.T, h *harness, turnID string, class principal.Class, ref replyReference) *runtimechannelhost.DeliveryRequest {
	t.Helper()
	if err := h.sessions.EnsureSession(t.Context(), deliverySession, "local-owner", testPersona); err != nil {
		t.Fatalf("EnsureSession: %v", err)
	}
	rawRef, err := json.Marshal(ref)
	if err != nil {
		t.Fatalf("marshal reply ref: %v", err)
	}
	return &runtimechannelhost.DeliveryRequest{
		EffectID:       turnID,
		Channel:        "telegram:pocket",
		ConversationID: deliverySession,
		PrincipalClass: string(class),
		ReplyContext:   rawRef,
		IdempotencyKey: turnID,
	}
}

func extractTelegramStopButton(body map[string]any) (label, data string, ok bool) {
	markup, exists := body["reply_markup"].(map[string]any)
	if !exists {
		return "", "", false
	}
	rows, _ := markup["inline_keyboard"].([]any)
	if len(rows) != 1 {
		return "", "", false
	}
	row, _ := rows[0].([]any)
	if len(row) != 1 {
		return "", "", false
	}
	button, _ := row[0].(map[string]any)
	label, _ = button["text"].(string)
	data, _ = button["callback_data"].(string)
	return label, data, label != "" && data != ""
}

func flipHexTail(raw string) string {
	last := raw[len(raw)-1]
	if last == 'a' {
		return raw[:len(raw)-1] + "b"
	}
	return raw[:len(raw)-1] + "a"
}

func TestOwnerProgressMessageCarriesAStopButton(t *testing.T) {
	t.Run("owner gets button on post and every edit until delivery starts", func(t *testing.T) {
		h := newHarness(t)
		adapter := h.adapter(t, nil)
		useInMemoryBotAPI(adapter, h.api)
		req := stopPresenceRequest(t, h, "turn-owner-stop", principal.ClassOwner, replyReference{
			ChatID:    ownerUserID,
			MessageID: 100,
			FromID:    ownerUserID,
		})
		synctest.Test(t, func(t *testing.T) {
			req.Deadline = time.Now().Add(2 * time.Minute)
			if err := adapter.PresenceStarted(t.Context(), req); err != nil {
				t.Fatalf("PresenceStarted: %v", err)
			}
			time.Sleep(statusPostAfter)
			synctest.Wait()

			sends := h.api.sends()
			if len(sends) != 1 {
				t.Fatalf("status posts = %d, want 1", len(sends))
			}
			label, callbackData, ok := extractTelegramStopButton(sends[0])
			if !ok || label != status.DefaultTexts().StopButton {
				t.Fatalf("initial status button = (%q, %q, %v), want Stop button", label, callbackData, ok)
			}
			if _, action, _, valid := approval.ParseCallbackData(callbackData); !valid || action != approval.ActionStop {
				t.Fatalf("callback_data = %q, want valid stop callback", callbackData)
			}
			if adapter.stops.Len() != 1 {
				t.Fatalf("stop registry len = %d, want 1", adapter.stops.Len())
			}

			for range approval.MaxPending + 10 {
				if _, _, err := adapter.approvals.Issue(&approval.Binding{Actor: "1", Scope: [2]string{"1", "0"}}, time.Now().Add(time.Minute)); err != nil {
					t.Fatalf("approvals.Issue: %v", err)
				}
			}
			if adapter.stops.Len() != 1 {
				t.Fatalf("stop registry len = %d after approval pressure, want 1", adapter.stops.Len())
			}

			presenceUpdate(t, adapter, req, stepUpdate("c1", "Checking logs", runtimechannelhost.StepRunning))
			time.Sleep(time.Duration(adapter.cfg.MinEditInterval) + time.Second)
			synctest.Wait()
			presenceUpdate(t, adapter, req, stepUpdate("c1", "Checking logs", runtimechannelhost.StepDone))
			time.Sleep(time.Duration(adapter.cfg.MinEditInterval) + time.Second)
			synctest.Wait()

			edits := h.api.bodies("editMessageText")
			if len(edits) < 2 {
				t.Fatalf("status edits = %d, want at least 2", len(edits))
			}
			for i, edit := range edits {
				editLabel, editData, hasButton := extractTelegramStopButton(edit)
				if !hasButton || editLabel != status.DefaultTexts().StopButton || editData != callbackData {
					t.Fatalf("edit[%d] button = (%q, %q, %v), want (%q, %q, true)", i, editLabel, editData, hasButton, status.DefaultTexts().StopButton, callbackData)
				}
			}

			deliverReq := *req
			deliverReq.Parts = []runtimechannelhost.OutputPart{{Text: "final answer"}}
			if _, err := adapter.Deliver(t.Context(), &deliverReq); err != nil {
				t.Fatalf("Deliver: %v", err)
			}
			if adapter.stops.Len() != 0 {
				t.Fatalf("stop registry len = %d after Deliver, want 0", adapter.stops.Len())
			}
			clears := h.api.bodies("editMessageReplyMarkup")
			if len(clears) != 1 {
				t.Fatalf("keyboard removals on Deliver = %d, want 1", len(clears))
			}
			endPresence(t, adapter, req.EffectID)
		})
	})

	for _, class := range []principal.Class{principal.ClassTrusted, principal.ClassPublic} {
		t.Run("no button or token for "+string(class), func(t *testing.T) {
			h := newHarness(t)
			adapter := h.adapter(t, nil)
			useInMemoryBotAPI(adapter, h.api)
			req := stopPresenceRequest(t, h, "turn-guest-"+string(class), class, replyReference{
				ChatID:    ownerUserID,
				MessageID: 100,
				FromID:    requesterID,
			})
			synctest.Test(t, func(t *testing.T) {
				req.Deadline = time.Now().Add(2 * time.Minute)
				if err := adapter.PresenceStarted(t.Context(), req); err != nil {
					t.Fatalf("PresenceStarted: %v", err)
				}
				time.Sleep(statusPostAfter)
				synctest.Wait()

				sends := h.api.sends()
				if len(sends) != 1 {
					t.Fatalf("status posts = %d, want 1", len(sends))
				}
				if _, _, ok := extractTelegramStopButton(sends[0]); ok {
					t.Fatalf("%s status message carried a stop button: %v", class, sends[0]["reply_markup"])
				}
				if adapter.stops.Len() != 0 {
					t.Fatalf("stop registry len = %d for %s turn, want 0", adapter.stops.Len(), class)
				}
				endPresence(t, adapter, req.EffectID)
			})
		})
	}
}

func TestStopPressCancelsExactlyThatTurn(t *testing.T) {
	turns := &spyStopTurns{activeTurn: "turn-stop-tg"}
	h := newHarness(t)
	adapter := h.adapter(t, nil)
	adapter.commands = newStopDispatcher(t, turns)
	useInMemoryBotAPI(adapter, h.api)
	req := stopPresenceRequest(t, h, "turn-stop-tg", principal.ClassOwner, replyReference{
		ChatID:    ownerUserID,
		MessageID: 100,
		FromID:    ownerUserID,
	})
	synctest.Test(t, func(t *testing.T) {
		req.Deadline = time.Now().Add(2 * time.Minute)
		if err := adapter.PresenceStarted(t.Context(), req); err != nil {
			t.Fatalf("PresenceStarted: %v", err)
		}
		time.Sleep(statusPostAfter)
		synctest.Wait()

		_, callbackData, ok := extractTelegramStopButton(h.api.sends()[0])
		if !ok {
			t.Fatal("initial status message has no stop button")
		}
		adapter.handleCallback(t.Context(), &callbackQuery{
			ID:      "cb-stop-1",
			From:    &user{ID: ownerUserID},
			Message: &message{MessageID: 700, Chat: chat{ID: ownerUserID, Type: chatTypePrivate}},
			Data:    callbackData,
		})

		if got := turns.recordedTurns(); !slices.Equal(got, [][2]string{{deliverySession, "turn-stop-tg"}}) {
			t.Fatalf("CancelTurn calls = %v, want [[%q %q]]", got, deliverySession, "turn-stop-tg")
		}
		if got := turns.recordedActive(); len(got) != 0 {
			t.Fatalf("CancelActive calls = %v, want none", got)
		}
		if h.api.count("answerCallbackQuery") != 1 {
			t.Fatalf("answerCallbackQuery calls = %d, want 1", h.api.count("answerCallbackQuery"))
		}
		if h.api.count("editMessageText") != 0 {
			t.Fatalf("status edited before PresenceEnded: %d edits", h.api.count("editMessageText"))
		}

		if err := adapter.PresenceEnded(t.Context(), "turn-stop-tg", runtimechannelhost.PresenceEnd{Stopped: true}); err != nil {
			t.Fatalf("PresenceEnded: %v", err)
		}
		edits := h.api.bodies("editMessageText")
		if len(edits) != 1 {
			t.Fatalf("status edits after PresenceEnded = %d, want 1", len(edits))
		}
		if edits[0]["text"] != status.DefaultTexts().Stopped {
			t.Errorf("stopped status text = %q, want %q", edits[0]["text"], status.DefaultTexts().Stopped)
		}
		if _, hasMarkup := edits[0]["reply_markup"]; hasMarkup {
			t.Errorf("stopped status still carried reply_markup: %v", edits[0]["reply_markup"])
		}
	})
}

func TestStopPressIsRejectedWhenForgedOrMisplaced(t *testing.T) {
	const groupChatID = int64(-100777)
	const topicID = int64(42)

	cases := []struct {
		name           string
		ref            replyReference
		chatType       string
		beforeStatus   func(t *testing.T, adapter *Adapter, req *runtimechannelhost.DeliveryRequest)
		beforePress    func(t *testing.T, adapter *Adapter, req *runtimechannelhost.DeliveryRequest, data string)
		press          func(data string) *callbackQuery
		ownerCanFollow bool
	}{
		{
			name:     "other user",
			ref:      replyReference{ChatID: groupChatID, ThreadID: topicID, MessageID: 100, FromID: ownerUserID},
			chatType: "supergroup",
			press: func(data string) *callbackQuery {
				return &callbackQuery{
					ID:      "cb-other-user",
					From:    &user{ID: requesterID},
					Message: &message{MessageID: 700, Chat: chat{ID: groupChatID, Type: "supergroup"}, ThreadID: topicID, IsTopicMessage: true},
					Data:    data,
				}
			},
			ownerCanFollow: true,
		},
		{
			name:     "shared group member overwrote approval target",
			ref:      replyReference{ChatID: groupChatID, ThreadID: topicID, MessageID: 100, FromID: ownerUserID},
			chatType: "supergroup",
			beforeStatus: func(_ *testing.T, adapter *Adapter, req *runtimechannelhost.DeliveryRequest) {
				adapter.rememberTarget(req.ConversationID, &approvalTarget{
					binding: approval.Binding{
						Principal: "telegram:555001",
						Class:     principal.ClassTrusted,
						Persona:   testPersona,
						Actor:     "555001",
						Scope:     scopeOf(destination{chatID: groupChatID, threadID: topicID}),
					},
					dest: destination{chatID: groupChatID, threadID: topicID},
				})
			},
			press: func(data string) *callbackQuery {
				return &callbackQuery{
					ID:      "cb-shared-group",
					From:    &user{ID: requesterID},
					Message: &message{MessageID: 700, Chat: chat{ID: groupChatID, Type: "supergroup"}, ThreadID: topicID, IsTopicMessage: true},
					Data:    data,
				}
			},
			ownerCanFollow: true,
		},
		{
			name:     "other chat",
			ref:      replyReference{ChatID: groupChatID, ThreadID: topicID, MessageID: 100, FromID: ownerUserID},
			chatType: "supergroup",
			press: func(data string) *callbackQuery {
				return &callbackQuery{
					ID:      "cb-other-chat",
					From:    &user{ID: ownerUserID},
					Message: &message{MessageID: 700, Chat: chat{ID: groupChatID - 1, Type: "supergroup"}, ThreadID: topicID, IsTopicMessage: true},
					Data:    data,
				}
			},
			ownerCanFollow: true,
		},
		{
			name:     "other topic",
			ref:      replyReference{ChatID: groupChatID, ThreadID: topicID, MessageID: 100, FromID: ownerUserID},
			chatType: "supergroup",
			press: func(data string) *callbackQuery {
				return &callbackQuery{
					ID:      "cb-other-topic",
					From:    &user{ID: ownerUserID},
					Message: &message{MessageID: 700, Chat: chat{ID: groupChatID, Type: "supergroup"}, ThreadID: topicID + 1, IsTopicMessage: true},
					Data:    data,
				}
			},
			ownerCanFollow: true,
		},
		{
			name:     "expired",
			ref:      replyReference{ChatID: ownerUserID, MessageID: 100, FromID: ownerUserID},
			chatType: chatTypePrivate,
			beforePress: func(_ *testing.T, _ *Adapter, _ *runtimechannelhost.DeliveryRequest, _ string) {
				time.Sleep(3 * time.Minute)
			},
			press: func(data string) *callbackQuery {
				return &callbackQuery{
					ID:      "cb-expired",
					From:    &user{ID: ownerUserID},
					Message: &message{MessageID: 700, Chat: chat{ID: ownerUserID, Type: chatTypePrivate}},
					Data:    data,
				}
			},
		},
		{
			name:     "altered mac",
			ref:      replyReference{ChatID: ownerUserID, MessageID: 100, FromID: ownerUserID},
			chatType: chatTypePrivate,
			press: func(data string) *callbackQuery {
				return &callbackQuery{
					ID:      "cb-altered-mac",
					From:    &user{ID: ownerUserID},
					Message: &message{MessageID: 700, Chat: chat{ID: ownerUserID, Type: chatTypePrivate}},
					Data:    flipHexTail(data),
				}
			},
			ownerCanFollow: true,
		},
		{
			name:     "replayed id",
			ref:      replyReference{ChatID: ownerUserID, MessageID: 100, FromID: ownerUserID},
			chatType: chatTypePrivate,
			beforePress: func(_ *testing.T, adapter *Adapter, _ *runtimechannelhost.DeliveryRequest, _ string) {
				adapter.stops.NoteSeen("cb-replayed")
			},
			press: func(data string) *callbackQuery {
				return &callbackQuery{
					ID:      "cb-replayed",
					From:    &user{ID: ownerUserID},
					Message: &message{MessageID: 700, Chat: chat{ID: ownerUserID, Type: chatTypePrivate}},
					Data:    data,
				}
			},
		},
		{
			name:     "token after delivery started",
			ref:      replyReference{ChatID: ownerUserID, MessageID: 100, FromID: ownerUserID},
			chatType: chatTypePrivate,
			beforePress: func(t *testing.T, adapter *Adapter, req *runtimechannelhost.DeliveryRequest, _ string) {
				deliverReq := *req
				deliverReq.Parts = []runtimechannelhost.OutputPart{{Text: "answer"}}
				if _, err := adapter.Deliver(t.Context(), &deliverReq); err != nil {
					t.Fatalf("Deliver: %v", err)
				}
			},
			press: func(data string) *callbackQuery {
				return &callbackQuery{
					ID:      "cb-after-deliver",
					From:    &user{ID: ownerUserID},
					Message: &message{MessageID: 700, Chat: chat{ID: ownerUserID, Type: chatTypePrivate}},
					Data:    data,
				}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			turns := &spyStopTurns{activeTurn: "turn-reject-tg"}
			var dropped []string
			h := newHarness(t)
			adapter := h.adapter(t, nil)
			adapter.commands = newStopDispatcher(t, turns)
			adapter.dropped = func(_ context.Context, reason string) {
				dropped = append(dropped, reason)
			}
			useInMemoryBotAPI(adapter, h.api)
			req := stopPresenceRequest(t, h, "turn-reject-tg", principal.ClassOwner, tc.ref)

			synctest.Test(t, func(t *testing.T) {
				req.Deadline = time.Now().Add(2 * time.Minute)
				if err := adapter.PresenceStarted(t.Context(), req); err != nil {
					t.Fatalf("PresenceStarted: %v", err)
				}
				if tc.beforeStatus != nil {
					tc.beforeStatus(t, adapter, req)
				}
				time.Sleep(statusPostAfter)
				synctest.Wait()

				_, callbackData, ok := extractTelegramStopButton(h.api.sends()[0])
				if !ok {
					t.Fatal("status message missing stop button")
				}
				if tc.beforePress != nil {
					tc.beforePress(t, adapter, req, callbackData)
				}
				adapter.handleCallback(t.Context(), tc.press(callbackData))

				if got := turns.recordedTurns(); len(got) != 0 {
					t.Fatalf("CancelTurn calls = %v, want none", got)
				}
				if !slices.Contains(dropped, string(dropCallbackRejected)) {
					t.Fatalf("dropped reasons = %v, want %q", dropped, dropCallbackRejected)
				}
				if tc.ownerCanFollow {
					msg := &message{MessageID: 700, Chat: chat{ID: tc.ref.ChatID, Type: tc.chatType}}
					if tc.ref.ThreadID > 0 {
						msg.ThreadID = tc.ref.ThreadID
						msg.IsTopicMessage = true
					}
					adapter.handleCallback(t.Context(), &callbackQuery{
						ID:      "cb-owner-valid-followup",
						From:    &user{ID: ownerUserID},
						Message: msg,
						Data:    callbackData,
					})
					if got := turns.recordedTurns(); len(got) != 1 {
						t.Fatalf("owner follow-up CancelTurn calls = %v, want 1", got)
					}
				}
				endPresence(t, adapter, req.EffectID)
			})
		})
	}
}

func TestChannelTurnStopTokenExpiresAtTheTurnDeadline(t *testing.T) {
	t.Run("accepted before deadline", func(t *testing.T) {
		turns := &spyStopTurns{activeTurn: "turn-before-deadline"}
		h := newHarness(t)
		adapter := h.adapter(t, nil)
		adapter.commands = newStopDispatcher(t, turns)
		useInMemoryBotAPI(adapter, h.api)
		req := stopPresenceRequest(t, h, "turn-before-deadline", principal.ClassOwner, replyReference{
			ChatID: ownerUserID, MessageID: 100, FromID: ownerUserID,
		})
		synctest.Test(t, func(t *testing.T) {
			req.Deadline = time.Now().Add(45 * time.Second)
			if err := adapter.PresenceStarted(t.Context(), req); err != nil {
				t.Fatalf("PresenceStarted: %v", err)
			}
			time.Sleep(statusPostAfter)
			synctest.Wait()
			_, data, ok := extractTelegramStopButton(h.api.sends()[0])
			if !ok {
				t.Fatal("missing stop button")
			}
			time.Sleep(30 * time.Second)
			adapter.handleCallback(t.Context(), &callbackQuery{
				ID:      "cb-before-dl",
				From:    &user{ID: ownerUserID},
				Message: &message{MessageID: 700, Chat: chat{ID: ownerUserID, Type: chatTypePrivate}},
				Data:    data,
			})
			if len(turns.recordedTurns()) != 1 {
				t.Fatalf("CancelTurn calls = %v, want 1 before deadline", turns.recordedTurns())
			}
			endPresence(t, adapter, req.EffectID)
		})
	})

	t.Run("rejected after deadline", func(t *testing.T) {
		turns := &spyStopTurns{activeTurn: "turn-after-deadline"}
		var dropped []string
		h := newHarness(t)
		adapter := h.adapter(t, nil)
		adapter.commands = newStopDispatcher(t, turns)
		adapter.dropped = func(_ context.Context, reason string) { dropped = append(dropped, reason) }
		useInMemoryBotAPI(adapter, h.api)
		req := stopPresenceRequest(t, h, "turn-after-deadline", principal.ClassOwner, replyReference{
			ChatID: ownerUserID, MessageID: 100, FromID: ownerUserID,
		})
		synctest.Test(t, func(t *testing.T) {
			req.Deadline = time.Now().Add(45 * time.Second)
			if err := adapter.PresenceStarted(t.Context(), req); err != nil {
				t.Fatalf("PresenceStarted: %v", err)
			}
			time.Sleep(statusPostAfter)
			synctest.Wait()
			_, data, ok := extractTelegramStopButton(h.api.sends()[0])
			if !ok {
				t.Fatal("missing stop button")
			}
			time.Sleep(36 * time.Second)
			adapter.handleCallback(t.Context(), &callbackQuery{
				ID:      "cb-after-dl",
				From:    &user{ID: ownerUserID},
				Message: &message{MessageID: 700, Chat: chat{ID: ownerUserID, Type: chatTypePrivate}},
				Data:    data,
			})
			if len(turns.recordedTurns()) != 0 {
				t.Fatalf("CancelTurn calls = %v, want 0 after deadline", turns.recordedTurns())
			}
			if !slices.Contains(dropped, string(dropCallbackRejected)) {
				t.Fatalf("dropped = %v, want callback_rejected", dropped)
			}
			endPresence(t, adapter, req.EffectID)
		})
	})
}

func TestLateStopPressDoesNotCancelANewerTurn(t *testing.T) {
	turns := &spyStopTurns{activeTurn: "turn-newer"}
	h := newHarness(t)
	adapter := h.adapter(t, nil)
	adapter.commands = newStopDispatcher(t, turns)
	useInMemoryBotAPI(adapter, h.api)
	req := stopPresenceRequest(t, h, "turn-older", principal.ClassOwner, replyReference{
		ChatID: ownerUserID, MessageID: 100, FromID: ownerUserID,
	})
	synctest.Test(t, func(t *testing.T) {
		req.Deadline = time.Now().Add(2 * time.Minute)
		if err := adapter.PresenceStarted(t.Context(), req); err != nil {
			t.Fatalf("PresenceStarted: %v", err)
		}
		time.Sleep(statusPostAfter)
		synctest.Wait()
		_, data, ok := extractTelegramStopButton(h.api.sends()[0])
		if !ok {
			t.Fatal("missing stop button")
		}
		adapter.handleCallback(t.Context(), &callbackQuery{
			ID:      "cb-stale-turn",
			From:    &user{ID: ownerUserID},
			Message: &message{MessageID: 700, Chat: chat{ID: ownerUserID, Type: chatTypePrivate}},
			Data:    data,
		})
		if active, _ := turns.Activity(deliverySession); active != "turn-newer" {
			t.Fatalf("newer turn was cancelled: active = %q", active)
		}
		if len(turns.recordedActive()) != 0 {
			t.Fatalf("CancelActive was called: %v", turns.recordedActive())
		}
		endPresence(t, adapter, req.EffectID)
	})
}

func TestStopEndStates(t *testing.T) {
	t.Run("non-owner cancelled renders stopped without a button", func(t *testing.T) {
		h := newHarness(t)
		adapter := h.adapter(t, nil)
		useInMemoryBotAPI(adapter, h.api)
		req := stopPresenceRequest(t, h, "turn-guest-cancel", principal.ClassTrusted, replyReference{
			ChatID: ownerUserID, MessageID: 100, FromID: requesterID,
		})
		synctest.Test(t, func(t *testing.T) {
			req.Deadline = time.Now().Add(2 * time.Minute)
			if err := adapter.PresenceStarted(t.Context(), req); err != nil {
				t.Fatalf("PresenceStarted: %v", err)
			}
			time.Sleep(statusPostAfter)
			synctest.Wait()
			if err := adapter.PresenceEnded(t.Context(), req.EffectID, runtimechannelhost.PresenceEnd{Stopped: true}); err != nil {
				t.Fatalf("PresenceEnded: %v", err)
			}
			edits := h.api.bodies("editMessageText")
			if len(edits) != 1 || edits[0]["text"] != status.DefaultTexts().Stopped {
				t.Fatalf("edits = %v, want one stopped edit", edits)
			}
			if _, hasMarkup := edits[0]["reply_markup"]; hasMarkup {
				t.Errorf("non-owner stopped edit carried reply_markup: %v", edits[0]["reply_markup"])
			}
		})
	})

	t.Run("cancel before status posted posts nothing", func(t *testing.T) {
		h := newHarness(t)
		adapter := h.adapter(t, nil)
		useInMemoryBotAPI(adapter, h.api)
		req := stopPresenceRequest(t, h, "turn-early-cancel", principal.ClassOwner, replyReference{
			ChatID: ownerUserID, MessageID: 100, FromID: ownerUserID,
		})
		synctest.Test(t, func(t *testing.T) {
			req.Deadline = time.Now().Add(2 * time.Minute)
			if err := adapter.PresenceStarted(t.Context(), req); err != nil {
				t.Fatalf("PresenceStarted: %v", err)
			}
			time.Sleep(2 * time.Second)
			synctest.Wait()
			if err := adapter.PresenceEnded(t.Context(), req.EffectID, runtimechannelhost.PresenceEnd{Stopped: true}); err != nil {
				t.Fatalf("PresenceEnded: %v", err)
			}
			if h.api.count("sendMessage") != 0 || h.api.count("editMessageText") != 0 {
				t.Fatalf("sends=%d edits=%d, want 0 and 0", h.api.count("sendMessage"), h.api.count("editMessageText"))
			}
		})
	})

	t.Run("preempted turn clears button without stopped text", func(t *testing.T) {
		h := newHarness(t)
		adapter := h.adapter(t, nil)
		useInMemoryBotAPI(adapter, h.api)
		req := stopPresenceRequest(t, h, "turn-preempt-tg", principal.ClassOwner, replyReference{
			ChatID: ownerUserID, MessageID: 100, FromID: ownerUserID,
		})
		synctest.Test(t, func(t *testing.T) {
			req.Deadline = time.Now().Add(2 * time.Minute)
			if err := adapter.PresenceStarted(t.Context(), req); err != nil {
				t.Fatalf("PresenceStarted: %v", err)
			}
			time.Sleep(statusPostAfter)
			synctest.Wait()
			if err := adapter.PresenceEnded(t.Context(), req.EffectID, runtimechannelhost.PresenceEnd{Stopped: false}); err != nil {
				t.Fatalf("PresenceEnded: %v", err)
			}
			if h.api.count("editMessageText") != 0 {
				t.Fatalf("edits on preemption = %d, want 0", h.api.count("editMessageText"))
			}
			if h.api.count("editMessageReplyMarkup") != 1 {
				t.Fatalf("keyboard removals on preemption = %d, want 1", h.api.count("editMessageReplyMarkup"))
			}
			if adapter.stops.Len() != 0 {
				t.Fatalf("stop registry len = %d, want 0", adapter.stops.Len())
			}
		})
	})

	t.Run("rate limit hold skips stopped edit cleanly", func(t *testing.T) {
		h := newHarness(t)
		adapter := h.adapter(t, nil)
		adapter.rateCap = rateLimitCap
		useInMemoryBotAPI(adapter, h.api)
		req := stopPresenceRequest(t, h, "turn-held-tg", principal.ClassOwner, replyReference{
			ChatID: ownerUserID, MessageID: 100, FromID: ownerUserID,
		})
		synctest.Test(t, func(t *testing.T) {
			req.Deadline = time.Now().Add(2 * time.Minute)
			if err := adapter.PresenceStarted(t.Context(), req); err != nil {
				t.Fatalf("PresenceStarted: %v", err)
			}
			time.Sleep(statusPostAfter)
			synctest.Wait()
			h.api.failNextOn("sendChatAction", apiFailure(http.StatusTooManyRequests, 30))
			time.Sleep(typingRefreshInterval)
			synctest.Wait()
			if err := adapter.PresenceEnded(t.Context(), req.EffectID, runtimechannelhost.PresenceEnd{Stopped: true}); err != nil {
				t.Fatalf("PresenceEnded: %v", err)
			}
			if h.api.count("editMessageText") != 0 {
				t.Fatalf("edits during rate-limit hold = %d, want 0", h.api.count("editMessageText"))
			}
		})
	})

	t.Run("admit records sender from_id in reply context", func(t *testing.T) {
		h := newHarness(t)
		adapter := h.adapter(t, nil)
		env, _, ok := adapter.admit(t.Context(), &update{
			UpdateID: 1,
			Message: &message{
				MessageID: 101,
				From:      &user{ID: ownerUserID},
				Chat:      chat{ID: ownerUserID, Type: chatTypePrivate},
				Text:      "hello",
			},
		})
		if !ok {
			t.Fatal("owner message was not admitted")
		}
		var ref replyReference
		if err := json.Unmarshal(env.ReplyContext, &ref); err != nil {
			t.Fatalf("unmarshal reply context: %v", err)
		}
		if ref.FromID != ownerUserID {
			t.Errorf("replyReference.FromID = %d, want %d", ref.FromID, ownerUserID)
		}
	})
}
