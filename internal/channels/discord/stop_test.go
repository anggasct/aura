package discord

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"slices"
	"strings"
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

type spyDiscordStopTurns struct {
	mu          sync.Mutex
	activeTurn  string
	cancelTurns [][2]string
	cancelAll   []string
}

func (s *spyDiscordStopTurns) CancelActive(_ context.Context, sessionID string) (turnID string, cancelled bool, err error) {
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

func (s *spyDiscordStopTurns) CancelTurn(_ context.Context, sessionID, turnID string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cancelTurns = append(s.cancelTurns, [2]string{sessionID, turnID})
	if s.activeTurn != "" && s.activeTurn == turnID {
		s.activeTurn = ""
		return true, nil
	}
	return false, nil
}

func (s *spyDiscordStopTurns) Activity(string) (activeTurnID string, queued int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.activeTurn, 0
}

func (s *spyDiscordStopTurns) recordedTurns() [][2]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.cancelTurns)
}

func (s *spyDiscordStopTurns) recordedActive() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.cancelAll)
}

type noopDiscordSessions struct{}

func (noopDiscordSessions) EventCount(context.Context, string) (int, error) { return 0, nil }

func (noopDiscordSessions) Create(context.Context, *command.Caller) (command.Session, error) {
	return command.Session{}, nil
}

func (noopDiscordSessions) Switch(context.Context, *command.Caller, string) (command.Session, error) {
	return command.Session{}, nil
}

func newDiscordStopDispatcher(t *testing.T, turns command.Turns) *command.Dispatcher {
	t.Helper()
	registry, err := command.New(CommandBinding())
	if err != nil {
		t.Fatalf("command.New: %v", err)
	}
	dispatcher, err := command.NewDispatcher(registry, &command.Ports{
		Sessions: noopDiscordSessions{},
		Turns:    turns,
	}, nil)
	if err != nil {
		t.Fatalf("NewDispatcher: %v", err)
	}
	return dispatcher
}

func newStopDiscordAdapter(t *testing.T, turns command.Turns) (*Adapter, *memoryDiscordAPI, *logCapture) {
	t.Helper()
	adapter, api := newMemoryDiscordAdapter(t, nil)
	logs := &logCapture{}
	adapter.logger = slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	if turns != nil {
		adapter.commands = newDiscordStopDispatcher(t, turns)
	}
	return adapter, api, logs
}

func discordStopRequest(t *testing.T, turnID string, class principal.Class, ref replyReference, deadline time.Time) *runtimechannelhost.DeliveryRequest {
	t.Helper()
	rawRef, err := json.Marshal(ref)
	if err != nil {
		t.Fatalf("marshal reply ref: %v", err)
	}
	return &runtimechannelhost.DeliveryRequest{
		EffectID:       turnID,
		Channel:        "discord:main",
		ConversationID: "main/discord:main/channel:333",
		PrincipalClass: string(class),
		ReplyContext:   rawRef,
		IdempotencyKey: turnID,
		Deadline:       deadline,
	}
}

func extractDiscordStopButton(body []byte) (label, customID string, style int, ok bool) {
	var decoded map[string]any
	if err := json.Unmarshal(body, &decoded); err != nil {
		return "", "", 0, false
	}
	rows, exists := decoded["components"].([]any)
	if !exists || len(rows) != 1 {
		return "", "", 0, false
	}
	row, _ := rows[0].(map[string]any)
	buttons, _ := row["components"].([]any)
	if len(buttons) != 1 {
		return "", "", 0, false
	}
	button, _ := buttons[0].(map[string]any)
	label, _ = button["label"].(string)
	customID, _ = button["custom_id"].(string)
	styleFloat, _ := button["style"].(float64)
	return label, customID, int(styleFloat), label != "" && customID != ""
}

func (m *memoryDiscordAPI) interactionAcks() []timedRestCall {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []timedRestCall
	for _, call := range m.calls {
		if call.method == http.MethodPost && strings.HasSuffix(call.path, "/callback") {
			out = append(out, call)
		}
	}
	return out
}

func (m *memoryDiscordAPI) messageEdits() []timedRestCall {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []timedRestCall
	for _, call := range m.calls {
		if call.method == http.MethodPatch {
			out = append(out, call)
		}
	}
	return out
}

func flipDiscordHexTail(raw string) string {
	last := raw[len(raw)-1]
	if last == 'a' {
		return raw[:len(raw)-1] + "b"
	}
	return raw[:len(raw)-1] + "a"
}

func TestOwnerProgressMessageCarriesAStopButton(t *testing.T) {
	t.Run("owner gets secondary stop button and it clears on delivery start", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			adapter, api, _ := newStopDiscordAdapter(t, nil)
			req := discordStopRequest(t, "turn-owner-discord", principal.ClassOwner, replyReference{
				ChannelID: "333",
				MessageID: "501",
				GuildID:   "222",
				AuthorID:  "111",
			}, time.Now().Add(2*time.Minute))

			if err := adapter.PresenceStarted(t.Context(), req); err != nil {
				t.Fatalf("PresenceStarted: %v", err)
			}
			time.Sleep(statusPostAfter)
			synctest.Wait()

			posts := api.messagePosts()
			if len(posts) != 1 {
				t.Fatalf("status posts = %d, want 1", len(posts))
			}
			label, customID, style, ok := extractDiscordStopButton(posts[0].body)
			if !ok || label != status.DefaultTexts().StopButton || style != buttonStyleSecondary {
				t.Fatalf("button = (%q, %q, style=%d, ok=%v), want Stop secondary button", label, customID, style, ok)
			}
			if _, action, _, valid := approval.ParseCallbackData(customID); !valid || action != approval.ActionStop {
				t.Fatalf("custom_id = %q, want valid stop callback", customID)
			}
			if adapter.stops.Len() != 1 {
				t.Fatalf("stop registry len = %d, want 1", adapter.stops.Len())
			}

			for range approval.MaxPending + 10 {
				if _, _, err := adapter.approvals.Issue(&approval.Binding{Actor: "1", Scope: [2]string{"222", "333"}}, time.Now().Add(time.Minute)); err != nil {
					t.Fatalf("approvals.Issue: %v", err)
				}
			}
			if adapter.stops.Len() != 1 {
				t.Fatalf("stop registry len = %d after approval pressure, want 1", adapter.stops.Len())
			}

			if err := adapter.PresenceUpdated(t.Context(), req, runtimechannelhost.PresenceUpdate{
				CallIDs: []string{"c1"}, Activity: "Reading config", Outcome: runtimechannelhost.StepRunning,
			}); err != nil {
				t.Fatalf("PresenceUpdated: %v", err)
			}
			time.Sleep(time.Duration(adapter.cfg.MinEditInterval) + time.Second)
			synctest.Wait()

			edits := api.messageEdits()
			if len(edits) != 1 {
				t.Fatalf("status edits = %d, want 1", len(edits))
			}
			var editPayload map[string]any
			if err := json.Unmarshal(edits[0].body, &editPayload); err != nil {
				t.Fatalf("unmarshal edit: %v", err)
			}
			if _, setsComponents := editPayload["components"]; setsComponents {
				t.Fatalf("normal discord status edit unexpectedly set components: %v", editPayload["components"])
			}

			deliverReq := *req
			deliverReq.Parts = []runtimechannelhost.OutputPart{{Text: "final answer"}}
			if _, err := adapter.Deliver(t.Context(), &deliverReq); err != nil {
				t.Fatalf("Deliver: %v", err)
			}
			if adapter.stops.Len() != 0 {
				t.Fatalf("stop registry len = %d after Deliver, want 0", adapter.stops.Len())
			}
			editsAfterDeliver := api.messageEdits()
			cleared := false
			for _, edit := range editsAfterDeliver[1:] {
				var body map[string]any
				_ = json.Unmarshal(edit.body, &body)
				if comps, has := body["components"].([]any); has && len(comps) == 0 {
					cleared = true
					break
				}
			}
			if !cleared {
				t.Fatalf("expected components: [] PATCH when delivery started; edits = %d", len(editsAfterDeliver))
			}
			if err := adapter.PresenceEnded(t.Context(), req.EffectID, runtimechannelhost.PresenceEnd{}); err != nil {
				t.Fatalf("PresenceEnded: %v", err)
			}
		})
	})

	for _, class := range []principal.Class{principal.ClassTrusted, principal.ClassPublic} {
		t.Run("no button or token for "+string(class), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				adapter, api, _ := newStopDiscordAdapter(t, nil)
				req := discordStopRequest(t, "turn-guest-"+string(class), class, replyReference{
					ChannelID: "333",
					MessageID: "501",
					GuildID:   "222",
					AuthorID:  "999",
				}, time.Now().Add(2*time.Minute))

				if err := adapter.PresenceStarted(t.Context(), req); err != nil {
					t.Fatalf("PresenceStarted: %v", err)
				}
				time.Sleep(statusPostAfter)
				synctest.Wait()

				posts := api.messagePosts()
				if len(posts) != 1 {
					t.Fatalf("status posts = %d, want 1", len(posts))
				}
				if _, _, _, ok := extractDiscordStopButton(posts[0].body); ok {
					t.Fatalf("%s status message carried a stop button", class)
				}
				if adapter.stops.Len() != 0 {
					t.Fatalf("stop registry len = %d for %s turn, want 0", adapter.stops.Len(), class)
				}
				if err := adapter.PresenceEnded(t.Context(), req.EffectID, runtimechannelhost.PresenceEnd{}); err != nil {
					t.Fatalf("PresenceEnded: %v", err)
				}
			})
		})
	}
}

func TestStopPressCancelsExactlyThatTurn(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		turns := &spyDiscordStopTurns{activeTurn: "turn-stop-dc"}
		adapter, api, _ := newStopDiscordAdapter(t, turns)
		req := discordStopRequest(t, "turn-stop-dc", principal.ClassOwner, replyReference{
			ChannelID: "333",
			MessageID: "501",
			GuildID:   "222",
			ThreadID:  "444",
			AuthorID:  "111",
		}, time.Now().Add(2*time.Minute))

		if err := adapter.PresenceStarted(t.Context(), req); err != nil {
			t.Fatalf("PresenceStarted: %v", err)
		}
		time.Sleep(statusPostAfter)
		synctest.Wait()

		_, customID, _, ok := extractDiscordStopButton(api.messagePosts()[0].body)
		if !ok {
			t.Fatal("initial status missing stop button")
		}
		adapter.resolveComponentClick(t.Context(), clickPayload("ix-stop-1", "111", "222", "444", customID))

		if got := turns.recordedTurns(); !slices.Equal(got, [][2]string{{req.ConversationID, "turn-stop-dc"}}) {
			t.Fatalf("CancelTurn calls = %v, want [[%q %q]]", got, req.ConversationID, "turn-stop-dc")
		}
		if len(turns.recordedActive()) != 0 {
			t.Fatalf("CancelActive calls = %v, want none", turns.recordedActive())
		}
		if len(api.interactionAcks()) != 1 {
			t.Fatalf("interaction acks = %d, want 1", len(api.interactionAcks()))
		}
		if len(api.messageEdits()) != 0 {
			t.Fatalf("status edited before PresenceEnded: %d edits", len(api.messageEdits()))
		}

		if err := adapter.PresenceEnded(t.Context(), "turn-stop-dc", runtimechannelhost.PresenceEnd{Stopped: true}); err != nil {
			t.Fatalf("PresenceEnded: %v", err)
		}
		edits := api.messageEdits()
		if len(edits) != 1 {
			t.Fatalf("edits after PresenceEnded = %d, want 1", len(edits))
		}
		var body map[string]any
		if err := json.Unmarshal(edits[0].body, &body); err != nil {
			t.Fatalf("unmarshal stopped edit: %v", err)
		}
		if body["content"] != status.DefaultTexts().Stopped {
			t.Errorf("stopped content = %q, want %q", body["content"], status.DefaultTexts().Stopped)
		}
		comps, hasComps := body["components"].([]any)
		if !hasComps || len(comps) != 0 {
			t.Errorf("stopped edit components = %v, want empty []", body["components"])
		}
	})
}

func TestStopPressIsRejectedWhenForgedOrMisplaced(t *testing.T) {
	cases := []struct {
		name           string
		ref            replyReference
		beforeStatus   func(t *testing.T, adapter *Adapter, req *runtimechannelhost.DeliveryRequest)
		beforePress    func(t *testing.T, adapter *Adapter, req *runtimechannelhost.DeliveryRequest, customID string)
		press          func(customID string) *interactionPayload
		ownerCanFollow bool
	}{
		{
			name: "other user",
			ref:  replyReference{ChannelID: "333", MessageID: "501", GuildID: "222", AuthorID: "111"},
			press: func(customID string) *interactionPayload {
				return clickPayload("ix-other-user", "999", "222", "333", customID)
			},
			ownerCanFollow: true,
		},
		{
			name: "shared group member overwrote approval target",
			ref:  replyReference{ChannelID: "333", MessageID: "501", GuildID: "222", AuthorID: "111"},
			beforeStatus: func(_ *testing.T, adapter *Adapter, req *runtimechannelhost.DeliveryRequest) {
				adapter.rememberTarget(req.ConversationID, &approvalBinding{
					principal: "discord:999",
					class:     principal.ClassTrusted,
					persona:   "main",
					actor:     "999",
					guildID:   "222",
					channelID: "333",
				})
			},
			press: func(customID string) *interactionPayload {
				return clickPayload("ix-shared-group", "999", "222", "333", customID)
			},
			ownerCanFollow: true,
		},
		{
			name: "other guild",
			ref:  replyReference{ChannelID: "333", MessageID: "501", GuildID: "222", AuthorID: "111"},
			press: func(customID string) *interactionPayload {
				return clickPayload("ix-other-guild", "111", "223", "333", customID)
			},
			ownerCanFollow: true,
		},
		{
			name: "other channel or thread parent",
			ref:  replyReference{ChannelID: "333", MessageID: "501", GuildID: "222", ThreadID: "444", AuthorID: "111"},
			press: func(customID string) *interactionPayload {
				return clickPayload("ix-parent-channel", "111", "222", "333", customID)
			},
			ownerCanFollow: true,
		},
		{
			name: "expired",
			ref:  replyReference{ChannelID: "333", MessageID: "501", GuildID: "222", AuthorID: "111"},
			beforePress: func(_ *testing.T, _ *Adapter, _ *runtimechannelhost.DeliveryRequest, _ string) {
				time.Sleep(3 * time.Minute)
			},
			press: func(customID string) *interactionPayload {
				return clickPayload("ix-expired", "111", "222", "333", customID)
			},
		},
		{
			name: "altered mac",
			ref:  replyReference{ChannelID: "333", MessageID: "501", GuildID: "222", AuthorID: "111"},
			press: func(customID string) *interactionPayload {
				return clickPayload("ix-altered-mac", "111", "222", "333", flipDiscordHexTail(customID))
			},
			ownerCanFollow: true,
		},
		{
			name: "replayed id",
			ref:  replyReference{ChannelID: "333", MessageID: "501", GuildID: "222", AuthorID: "111"},
			beforePress: func(_ *testing.T, adapter *Adapter, _ *runtimechannelhost.DeliveryRequest, _ string) {
				adapter.stops.NoteSeen("ix-replayed")
			},
			press: func(customID string) *interactionPayload {
				return clickPayload("ix-replayed", "111", "222", "333", customID)
			},
		},
		{
			name: "token after delivery started",
			ref:  replyReference{ChannelID: "333", MessageID: "501", GuildID: "222", AuthorID: "111"},
			beforePress: func(t *testing.T, adapter *Adapter, req *runtimechannelhost.DeliveryRequest, _ string) {
				deliverReq := *req
				deliverReq.Parts = []runtimechannelhost.OutputPart{{Text: "answer"}}
				if _, err := adapter.Deliver(t.Context(), &deliverReq); err != nil {
					t.Fatalf("Deliver: %v", err)
				}
			},
			press: func(customID string) *interactionPayload {
				return clickPayload("ix-after-deliver", "111", "222", "333", customID)
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				turns := &spyDiscordStopTurns{activeTurn: "turn-reject-dc"}
				adapter, api, logs := newStopDiscordAdapter(t, turns)
				req := discordStopRequest(t, "turn-reject-dc", principal.ClassOwner, tc.ref, time.Now().Add(2*time.Minute))

				if err := adapter.PresenceStarted(t.Context(), req); err != nil {
					t.Fatalf("PresenceStarted: %v", err)
				}
				if tc.beforeStatus != nil {
					tc.beforeStatus(t, adapter, req)
				}
				time.Sleep(statusPostAfter)
				synctest.Wait()

				_, customID, _, ok := extractDiscordStopButton(api.messagePosts()[0].body)
				if !ok {
					t.Fatal("status message missing stop button")
				}
				if tc.beforePress != nil {
					tc.beforePress(t, adapter, req, customID)
				}
				adapter.resolveComponentClick(t.Context(), tc.press(customID))

				if got := turns.recordedTurns(); len(got) != 0 {
					t.Fatalf("CancelTurn calls = %v, want none", got)
				}
				if !strings.Contains(logs.String(), "stop press rejected") {
					t.Fatalf("debug logs = %q, want stop press rejected", logs.String())
				}
				if strings.Contains(logs.String(), customID) {
					t.Fatalf("debug logs leaked callback token: %q", logs.String())
				}
				if tc.ownerCanFollow {
					targetChannel := tc.ref.ChannelID
					if tc.ref.ThreadID != "" {
						targetChannel = tc.ref.ThreadID
					}
					adapter.resolveComponentClick(t.Context(), clickPayload("ix-owner-valid-followup", "111", tc.ref.GuildID, targetChannel, customID))
					if got := turns.recordedTurns(); len(got) != 1 {
						t.Fatalf("owner follow-up CancelTurn calls = %v, want 1", got)
					}
				}
				if err := adapter.PresenceEnded(t.Context(), req.EffectID, runtimechannelhost.PresenceEnd{}); err != nil {
					t.Fatalf("PresenceEnded: %v", err)
				}
			})
		})
	}
}

func TestChannelTurnStopTokenExpiresAtTheTurnDeadline(t *testing.T) {
	t.Run("accepted before deadline", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			turns := &spyDiscordStopTurns{activeTurn: "turn-dc-before-dl"}
			adapter, api, _ := newStopDiscordAdapter(t, turns)
			req := discordStopRequest(t, "turn-dc-before-dl", principal.ClassOwner, replyReference{
				ChannelID: "333", MessageID: "501", GuildID: "222", AuthorID: "111",
			}, time.Now().Add(45*time.Second))

			if err := adapter.PresenceStarted(t.Context(), req); err != nil {
				t.Fatalf("PresenceStarted: %v", err)
			}
			time.Sleep(statusPostAfter)
			synctest.Wait()
			_, customID, _, ok := extractDiscordStopButton(api.messagePosts()[0].body)
			if !ok {
				t.Fatal("missing stop button")
			}
			time.Sleep(30 * time.Second)
			adapter.resolveComponentClick(t.Context(), clickPayload("ix-before-dl", "111", "222", "333", customID))
			if len(turns.recordedTurns()) != 1 {
				t.Fatalf("CancelTurn calls = %v, want 1", turns.recordedTurns())
			}
			if err := adapter.PresenceEnded(t.Context(), req.EffectID, runtimechannelhost.PresenceEnd{}); err != nil {
				t.Fatalf("PresenceEnded: %v", err)
			}
		})
	})

	t.Run("rejected after deadline", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			turns := &spyDiscordStopTurns{activeTurn: "turn-dc-after-dl"}
			adapter, api, logs := newStopDiscordAdapter(t, turns)
			req := discordStopRequest(t, "turn-dc-after-dl", principal.ClassOwner, replyReference{
				ChannelID: "333", MessageID: "501", GuildID: "222", AuthorID: "111",
			}, time.Now().Add(45*time.Second))

			if err := adapter.PresenceStarted(t.Context(), req); err != nil {
				t.Fatalf("PresenceStarted: %v", err)
			}
			time.Sleep(statusPostAfter)
			synctest.Wait()
			_, customID, _, ok := extractDiscordStopButton(api.messagePosts()[0].body)
			if !ok {
				t.Fatal("missing stop button")
			}
			time.Sleep(36 * time.Second)
			adapter.resolveComponentClick(t.Context(), clickPayload("ix-after-dl", "111", "222", "333", customID))
			if len(turns.recordedTurns()) != 0 {
				t.Fatalf("CancelTurn calls = %v, want 0 after deadline", turns.recordedTurns())
			}
			if !strings.Contains(logs.String(), "stop press rejected") {
				t.Fatalf("logs = %q, want stop press rejected", logs.String())
			}
			if err := adapter.PresenceEnded(t.Context(), req.EffectID, runtimechannelhost.PresenceEnd{}); err != nil {
				t.Fatalf("PresenceEnded: %v", err)
			}
		})
	})
}

func TestLateStopPressDoesNotCancelANewerTurn(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		turns := &spyDiscordStopTurns{activeTurn: "turn-newer"}
		adapter, api, _ := newStopDiscordAdapter(t, turns)
		req := discordStopRequest(t, "turn-older", principal.ClassOwner, replyReference{
			ChannelID: "333", MessageID: "501", GuildID: "222", AuthorID: "111",
		}, time.Now().Add(2*time.Minute))

		if err := adapter.PresenceStarted(t.Context(), req); err != nil {
			t.Fatalf("PresenceStarted: %v", err)
		}
		time.Sleep(statusPostAfter)
		synctest.Wait()
		_, customID, _, ok := extractDiscordStopButton(api.messagePosts()[0].body)
		if !ok {
			t.Fatal("missing stop button")
		}
		adapter.resolveComponentClick(t.Context(), clickPayload("ix-stale", "111", "222", "333", customID))
		if active, _ := turns.Activity(req.ConversationID); active != "turn-newer" {
			t.Fatalf("newer turn was cancelled: active = %q", active)
		}
		if len(turns.recordedActive()) != 0 {
			t.Fatalf("CancelActive was called: %v", turns.recordedActive())
		}
		if err := adapter.PresenceEnded(t.Context(), req.EffectID, runtimechannelhost.PresenceEnd{}); err != nil {
			t.Fatalf("PresenceEnded: %v", err)
		}
	})
}

func TestStopEndStates(t *testing.T) {
	t.Run("non-owner cancelled renders stopped without components", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			adapter, api, _ := newStopDiscordAdapter(t, nil)
			req := discordStopRequest(t, "turn-dc-guest-cancel", principal.ClassTrusted, replyReference{
				ChannelID: "333", MessageID: "501", GuildID: "222", AuthorID: "999",
			}, time.Now().Add(2*time.Minute))
			if err := adapter.PresenceStarted(t.Context(), req); err != nil {
				t.Fatalf("PresenceStarted: %v", err)
			}
			time.Sleep(statusPostAfter)
			synctest.Wait()
			if err := adapter.PresenceEnded(t.Context(), req.EffectID, runtimechannelhost.PresenceEnd{Stopped: true}); err != nil {
				t.Fatalf("PresenceEnded: %v", err)
			}
			edits := api.messageEdits()
			if len(edits) != 1 {
				t.Fatalf("edits = %d, want 1", len(edits))
			}
			var body map[string]any
			if err := json.Unmarshal(edits[0].body, &body); err != nil {
				t.Fatalf("unmarshal edit: %v", err)
			}
			if body["content"] != status.DefaultTexts().Stopped {
				t.Errorf("content = %q, want %q", body["content"], status.DefaultTexts().Stopped)
			}
			comps, has := body["components"].([]any)
			if !has || len(comps) != 0 {
				t.Errorf("components = %v, want empty []", body["components"])
			}
		})
	})

	t.Run("cancel before status posted posts nothing", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			adapter, api, _ := newStopDiscordAdapter(t, nil)
			req := discordStopRequest(t, "turn-dc-early-cancel", principal.ClassOwner, replyReference{
				ChannelID: "333", MessageID: "501", GuildID: "222", AuthorID: "111",
			}, time.Now().Add(2*time.Minute))
			if err := adapter.PresenceStarted(t.Context(), req); err != nil {
				t.Fatalf("PresenceStarted: %v", err)
			}
			time.Sleep(2 * time.Second)
			synctest.Wait()
			if err := adapter.PresenceEnded(t.Context(), req.EffectID, runtimechannelhost.PresenceEnd{Stopped: true}); err != nil {
				t.Fatalf("PresenceEnded: %v", err)
			}
			if len(api.messagePosts()) != 0 || len(api.messageEdits()) != 0 {
				t.Fatalf("posts=%d edits=%d, want 0 and 0", len(api.messagePosts()), len(api.messageEdits()))
			}
		})
	})

	t.Run("preempted turn clears components without stopped text", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			adapter, api, _ := newStopDiscordAdapter(t, nil)
			req := discordStopRequest(t, "turn-dc-preempt", principal.ClassOwner, replyReference{
				ChannelID: "333", MessageID: "501", GuildID: "222", AuthorID: "111",
			}, time.Now().Add(2*time.Minute))
			if err := adapter.PresenceStarted(t.Context(), req); err != nil {
				t.Fatalf("PresenceStarted: %v", err)
			}
			time.Sleep(statusPostAfter)
			synctest.Wait()
			if err := adapter.PresenceEnded(t.Context(), req.EffectID, runtimechannelhost.PresenceEnd{Stopped: false}); err != nil {
				t.Fatalf("PresenceEnded: %v", err)
			}
			edits := api.messageEdits()
			if len(edits) != 1 {
				t.Fatalf("edits on preemption = %d, want 1 component clear", len(edits))
			}
			var body map[string]any
			if err := json.Unmarshal(edits[0].body, &body); err != nil {
				t.Fatalf("unmarshal edit: %v", err)
			}
			if _, hasContent := body["content"]; hasContent {
				t.Errorf("preempted edit set content = %q, want components-only clear", body["content"])
			}
			comps, has := body["components"].([]any)
			if !has || len(comps) != 0 {
				t.Errorf("components = %v, want empty []", body["components"])
			}
			if adapter.stops.Len() != 0 {
				t.Fatalf("stop registry len = %d, want 0", adapter.stops.Len())
			}
		})
	})

	t.Run("ingress records author_id in direct and thread reply contexts", func(t *testing.T) {
		account := &Account{Name: "main", AcceptDMs: true, AllowedUserIDs: []string{"111"}}
		owners, err := principal.NewResolver([]string{"discord:111"})
		if err != nil {
			t.Fatalf("NewResolver: %v", err)
		}
		msg := &messagePayload{
			ID:        "501",
			ChannelID: "333",
			GuildID:   "222",
			Content:   "<@900> hello",
			Author:    userPayload{ID: "111"},
			Mentions:  []userPayload{{ID: "900"}},
		}
		env, reason, err := normalizeMessage(account, owners, "900", authorAccess{}, msg)
		if err != nil || reason != "" {
			t.Fatalf("normalizeMessage = %q, %v", reason, err)
		}
		var ref replyReference
		if err := json.Unmarshal(env.ReplyContext, &ref); err != nil {
			t.Fatalf("unmarshal direct ref: %v", err)
		}
		if ref.AuthorID != "111" {
			t.Errorf("direct AuthorID = %q, want 111", ref.AuthorID)
		}
		if err := rewriteEnvelopeForThread(env, msg, "444", "333"); err != nil {
			t.Fatalf("rewriteEnvelopeForThread: %v", err)
		}
		ref = replyReference{}
		if err := json.Unmarshal(env.ReplyContext, &ref); err != nil {
			t.Fatalf("unmarshal rewritten ref: %v", err)
		}
		if ref.AuthorID != "111" || ref.ThreadID != "444" {
			t.Errorf("rewritten ref = %+v, want author 111 thread 444", ref)
		}
		threadEnv, err := buildThreadEnvelope("main", principal.OwnerID, principal.ClassOwner, msg, "444", "333")
		if err != nil {
			t.Fatalf("buildThreadEnvelope: %v", err)
		}
		ref = replyReference{}
		if err := json.Unmarshal(threadEnv.ReplyContext, &ref); err != nil {
			t.Fatalf("unmarshal thread ref: %v", err)
		}
		if ref.AuthorID != "111" || ref.ThreadID != "444" {
			t.Errorf("thread ref = %+v, want author 111 thread 444", ref)
		}
	})
}
