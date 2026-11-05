package telegram

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/anggasct/aura/internal/channel/access"
	"github.com/anggasct/aura/internal/config"
	"github.com/anggasct/aura/internal/principal"
)

const handoffSession = "main/telegram:pocket/chat:555001"

func TestNotifyHandoffTellsTheOwnerAndTheRequester(t *testing.T) {
	cases := []struct {
		name string
		dest destination
		want access.HandoffTarget
	}{
		{
			name: "private chat",
			dest: privateDest(requesterID),
			want: access.HandoffTarget{Channel: "telegram", Account: "pocket", ChannelID: strconv.Itoa(requesterID)},
		},
		{
			name: "forum topic",
			dest: destination{chatID: groupChatID, threadID: forumTopic},
			want: access.HandoffTarget{Channel: "telegram", Account: "pocket", ChannelID: strconv.Itoa(groupChatID), ThreadID: strconv.Itoa(forumTopic)},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rig := newApprovalRig(t, accessOpts{})
			rig.adapter.rememberTarget(handoffSession, memberTarget(principal.ClassTrusted, tc.dest))
			target, err := rig.adapter.NotifyHandoff(t.Context(), handoffSession, "  wants a refund  ")
			if err != nil || target != tc.want {
				t.Fatalf("NotifyHandoff = %+v, %v; want %+v", target, err, tc.want)
			}
			owner := strings.Join(noticesTo(rig, privateDest(ownerUserID)), "\n")
			for _, want := range []string{
				"handoff requested",
				"persona: Main (main)",
				"session: " + handoffSession,
				"principal: telegram:555001",
				"reason: wants a refund",
				"reply: aura reply " + handoffSession + " <text>",
				"release: aura handoff release " + handoffSession,
			} {
				if !strings.Contains(owner, want) {
					t.Errorf("owner notice %q lacks %q", owner, want)
				}
			}
			if got := noticesTo(rig, tc.dest); !slices.Equal(got, []string{personaMessage(config.MessageHandoffOpen)}) {
				t.Errorf("requester notices = %q, want the handoff_open message", got)
			}
			for _, call := range rig.h.api.callsOf("sendMessage") {
				if chatID, _ := call.body["chat_id"].(float64); int64(chatID) == ownerUserID && call.body["message_thread_id"] != nil {
					t.Errorf("owner notice carries a thread id: %v", call.body)
				}
			}
		})
	}
}

func TestNotifyHandoffTruncatesTheReason(t *testing.T) {
	rig := newApprovalRig(t, accessOpts{})
	rig.adapter.rememberTarget(handoffSession, memberTarget(principal.ClassTrusted, privateDest(requesterID)))
	if _, err := rig.adapter.NotifyHandoff(t.Context(), handoffSession, strings.Repeat("x", 5000)); err != nil {
		t.Fatalf("NotifyHandoff: %v", err)
	}
	owner := strings.Join(noticesTo(rig, privateDest(ownerUserID)), "\n")
	if !strings.Contains(owner, "reason: "+strings.Repeat("x", handoffForwardRunes)+"\nreply:") {
		t.Errorf("reason was not bounded to %d runes: %d bytes", handoffForwardRunes, len(owner))
	}
}

func TestNotifyHandoffRefusals(t *testing.T) {
	t.Run("unknown session", func(t *testing.T) {
		rig := newApprovalRig(t, accessOpts{})
		_, err := rig.adapter.NotifyHandoff(t.Context(), "main/telegram:pocket/chat:unknown", "x")
		if !errors.Is(err, access.ErrNoHandoffTarget) {
			t.Fatalf("err = %v, want ErrNoHandoffTarget", err)
		}
		if sends := rig.h.api.sends(); len(sends) != 0 {
			t.Errorf("sent %d messages for an unknown session", len(sends))
		}
	})
	t.Run("owner without a telegram identity", func(t *testing.T) {
		rig := newApprovalRig(t, accessOpts{})
		owners, err := principal.NewResolver(nil)
		if err != nil {
			t.Fatalf("NewResolver: %v", err)
		}
		rig.adapter.SetPrincipalResolver(owners)
		rig.adapter.rememberTarget(handoffSession, memberTarget(principal.ClassTrusted, privateDest(requesterID)))
		_, err = rig.adapter.NotifyHandoff(t.Context(), handoffSession, "x")
		if code, ok := CodeOf(err); !ok || code != ErrorCodeHandoffUnavailable {
			t.Fatalf("err = %v, want %s", err, ErrorCodeHandoffUnavailable)
		}
		if sends := rig.h.api.sends(); len(sends) != 0 {
			t.Errorf("sent %d messages without an owner chat", len(sends))
		}
	})
	t.Run("unresolvable bot token", func(t *testing.T) {
		rig := newApprovalRig(t, accessOpts{})
		rig.adapter.rememberTarget(handoffSession, memberTarget(principal.ClassTrusted, privateDest(requesterID)))
		t.Setenv(testTokenEnv, "")
		_, err := rig.adapter.NotifyHandoff(t.Context(), handoffSession, "x")
		if code, ok := CodeOf(err); !ok || code != ErrorCodeConnectionFailed {
			t.Fatalf("err = %v, want %s", err, ErrorCodeConnectionFailed)
		}
	})
	t.Run("owner notice that cannot be delivered", func(t *testing.T) {
		rig := newApprovalRig(t, accessOpts{})
		rig.adapter.rememberTarget(handoffSession, memberTarget(principal.ClassTrusted, privateDest(requesterID)))
		rig.h.api.failNextOn("sendMessage", apiFailure(http.StatusForbidden, 0))
		_, err := rig.adapter.NotifyHandoff(t.Context(), handoffSession, "x")
		if code, ok := CodeOf(err); !ok || code != ErrorCodeBotAPIRejected {
			t.Fatalf("err = %v, want %s", err, ErrorCodeBotAPIRejected)
		}
		if got := noticesTo(rig, privateDest(requesterID)); len(got) != 0 {
			t.Errorf("the requester was told about a handoff the owner never received: %q", got)
		}
		for _, leak := range []string{testToken, strconv.Itoa(requesterID), strconv.Itoa(ownerUserID)} {
			if strings.Contains(err.Error(), leak) || strings.Contains(rig.h.logs.String(), leak) {
				t.Errorf("%q leaked into the error or the logs", leak)
			}
		}
	})
}

func TestReplyTargetNamesTheRequesterChat(t *testing.T) {
	rig := newApprovalRig(t, accessOpts{})
	if _, ok := rig.adapter.ReplyTarget(handoffSession); ok {
		t.Fatal("an unseen session has a reply target")
	}
	rig.adapter.rememberTarget(handoffSession, memberTarget(principal.ClassTrusted, destination{chatID: groupChatID, threadID: forumTopic}))
	want := access.HandoffTarget{Channel: "telegram", Account: "pocket", ChannelID: strconv.Itoa(groupChatID), ThreadID: strconv.Itoa(forumTopic)}
	if target, ok := rig.adapter.ReplyTarget(handoffSession); !ok || target != want {
		t.Fatalf("ReplyTarget = %+v, %v; want %+v", target, ok, want)
	}
	rig.adapter.rememberTarget("main/telegram:pocket/chat:blank", memberTarget(principal.ClassTrusted, destination{}))
	if _, ok := rig.adapter.ReplyTarget("main/telegram:pocket/chat:blank"); ok {
		t.Error("a target without a chat was offered")
	}
}

func TestParseDestination(t *testing.T) {
	cases := []struct {
		name     string
		chatID   string
		threadID string
		want     destination
		wantErr  bool
	}{
		{name: "private chat", chatID: "555001", want: destination{chatID: 555001}},
		{name: "supergroup topic", chatID: "-1001234567890", threadID: "7", want: destination{chatID: -1001234567890, threadID: 7}},
		{name: "empty chat", wantErr: true},
		{name: "zero chat", chatID: "0", wantErr: true},
		{name: "non numeric chat", chatID: "chat", wantErr: true},
		{name: "overflowing chat", chatID: "99999999999999999999", wantErr: true},
		{name: "non numeric thread", chatID: "555001", threadID: "topic", wantErr: true},
		{name: "negative thread", chatID: "555001", threadID: "-3", wantErr: true},
		{name: "zero thread", chatID: "555001", threadID: "0", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseDestination(tc.chatID, tc.threadID)
			if tc.wantErr {
				if code, ok := CodeOf(err); !ok || code != ErrorCodeInvalidArgument {
					t.Fatalf("err = %v, want %s", err, ErrorCodeInvalidArgument)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("parseDestination = %+v, %v; want %+v", got, err, tc.want)
			}
		})
	}
}

func TestSendPlainTextPostsEveryChunkToTheTopic(t *testing.T) {
	rig := newApprovalRig(t, accessOpts{})
	dest := destination{chatID: groupChatID, threadID: forumTopic}
	line := strings.Repeat("y", 1000) + "\n"
	if err := sendPlainText(t.Context(), rig.adapter.bot, testToken, dest, strings.Repeat(line, 9)); err != nil {
		t.Fatalf("sendPlainText: %v", err)
	}
	sends := rig.h.api.sends()
	if len(sends) < 2 {
		t.Fatalf("a text over the message limit went out in %d messages", len(sends))
	}
	for _, body := range sends {
		if body["chat_id"] != float64(groupChatID) || body["message_thread_id"] != float64(forumTopic) || body["parse_mode"] != nil {
			t.Errorf("send = %v, want plain text in the topic", body)
		}
	}
	if err := sendPlainText(t.Context(), rig.adapter.bot, testToken, dest, "  \n"); err == nil {
		t.Error("blank text accepted")
	}
}

func TestSendPlainTextErrorsNeverCarryTheToken(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	base := server.URL
	server.Close()
	bot := newBotClient()
	bot.base = base
	err := sendPlainText(t.Context(), bot, testToken, privateDest(requesterID), "hello")
	if code, ok := CodeOf(err); !ok || code != ErrorCodeConnectionFailed {
		t.Fatalf("err = %v, want %s", err, ErrorCodeConnectionFailed)
	}
	if strings.Contains(err.Error(), testToken) {
		t.Errorf("token leaked into %q", err)
	}
}

func TestDeliverTextValidatesBeforeSending(t *testing.T) {
	t.Setenv(testTokenEnv, testToken)
	valid := &config.TelegramAccount{Name: "pocket", BotTokenRef: "env://" + testTokenEnv}
	cases := []struct {
		name     string
		account  *config.TelegramAccount
		chatID   string
		threadID string
		want     ErrorCode
	}{
		{name: "nil account", chatID: "555001", want: ErrorCodeInvalidArgument},
		{name: "corrupt chat id", account: valid, chatID: "not-a-chat", want: ErrorCodeInvalidArgument},
		{name: "corrupt thread id", account: valid, chatID: "555001", threadID: "-1", want: ErrorCodeInvalidArgument},
		{name: "unsupported token reference", account: &config.TelegramAccount{Name: "pocket", BotTokenRef: "literal"}, chatID: "555001", want: ErrorCodeInvalidArgument},
		{name: "unset token", account: &config.TelegramAccount{Name: "pocket", BotTokenRef: "env://AURA_TEST_UNSET_TELEGRAM_TOKEN"}, chatID: "555001", want: ErrorCodeConnectionFailed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := DeliverText(t.Context(), tc.account, tc.chatID, tc.threadID, "hello")
			if code, ok := CodeOf(err); !ok || code != tc.want {
				t.Fatalf("err = %v, want %s", err, tc.want)
			}
		})
	}
	var missing context.Context
	if code, ok := CodeOf(DeliverText(missing, valid, "555001", "", "hello")); !ok || code != ErrorCodeInvalidArgument {
		t.Errorf("nil context code = %q, %v", code, ok)
	}
}

func TestOpenHandoffHoldsTheNextRequesterMessage(t *testing.T) {
	handoffs := &fakeHandoffs{state: access.HandoffStateNone}
	rig := newAccessRig(t, accessOpts{allowed: []string{strconv.Itoa(allowedUserID)}, settings: AccessSettings{Handoffs: handoffs}})
	rig.run(t, privateText(10, allowedUserID, "I need a person"))
	_, _, session := rig.onlyEnvelope(t)
	target, err := rig.adapter.NotifyHandoff(t.Context(), session, "wants a human")
	if err != nil || target.ChannelID != strconv.Itoa(allowedUserID) || target.Channel != "telegram" {
		t.Fatalf("NotifyHandoff = %+v, %v", target, err)
	}
	if got := sentTo(rig.h.api, allowedUserID); !slices.Equal(got, []string{personaMessage(config.MessageHandoffOpen)}) {
		t.Fatalf("requester notices = %q, want handoff_open", got)
	}
	handoffs.state = access.HandoffStateOpen
	rig.run(t, privateText(11, allowedUserID, "are you there?"))
	if accepted := rig.sink.accepted(); len(accepted) != 1 {
		t.Fatalf("a message during the handoff reached the persona: %d envelopes", len(accepted))
	}
	forwarded := sentTo(rig.h.api, ownerUserID)
	if len(forwarded) != 2 || !strings.Contains(forwarded[1], "are you there?") || !strings.Contains(forwarded[1], session) {
		t.Fatalf("owner messages = %q, want the notice then the forwarded message", forwarded)
	}
}
