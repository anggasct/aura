package telegram

import (
	"context"
	"encoding/json"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	auraagent "github.com/anggasct/aura/internal/agent"
	"github.com/anggasct/aura/internal/channel/access"
	"github.com/anggasct/aura/internal/config"
	"github.com/anggasct/aura/internal/principal"
	runtimechannelhost "github.com/anggasct/aura/internal/runtime/channelhost"
)

const (
	groupChatID  = -1001234567890
	otherChatID  = -1009999999999
	otherBotName = "other_bot"
)

var groupChats = []string{strconv.Itoa(groupChatID)}

func groupMessage(updateID, userID int64, text string) update {
	return update{UpdateID: updateID, Message: &message{
		MessageID: updateID * 10,
		From:      &user{ID: userID},
		Chat:      chat{ID: groupChatID, Type: chatTypeSupergroup},
		Text:      text,
	}}
}

func entityOf(text, token, kind string) messageEntity {
	index := strings.Index(text, token)
	return messageEntity{Type: kind, Offset: utf16Len(text[:index]), Length: utf16Len(token)}
}

func mentioning(updateID, userID int64, text, token string) update {
	item := groupMessage(updateID, userID, text)
	item.Message.Entities = []messageEntity{entityOf(text, token, entityMention)}
	return item
}

func inTopic(item update, topic int64) update {
	item.Message.ThreadID = topic
	item.Message.IsTopicMessage = true
	return item
}

func replyingTo(item update, senderID int64) update {
	item.Message.ReplyTo = &message{MessageID: 5, From: &user{ID: senderID, IsBot: senderID == testBotID}, Chat: item.Message.Chat}
	return item
}

func newGroupRig(t *testing.T, opts accessOpts) *accessRig {
	t.Helper()
	if opts.chats == nil {
		opts.chats = groupChats
	}
	return newAccessRig(t, opts)
}

func (r *accessRig) identityKnown() bool {
	_, known := r.adapter.selfIdentity()
	return known
}

func (r *accessRig) runGroup(t *testing.T, updates ...update) {
	t.Helper()
	stop := startAdapter(t, r.adapter, r.sink)
	defer stop()
	eventually(t, "bot identity lookup", r.identityKnown)
	r.h.api.queue(updates...)
	awaitCursor(t, r.h, updates[len(updates)-1].UpdateID+1)
}

func (r *accessRig) onlyText(t *testing.T) string {
	t.Helper()
	accepted := r.sink.accepted()
	if len(accepted) != 1 {
		t.Fatalf("accepted %d envelopes, want 1", len(accepted))
	}
	return accepted[0].Parts[0].Text
}

func TestGroupAddressingFormsAreAdmitted(t *testing.T) {
	handle := "@" + testBotName
	mentionInMiddle := mentioning(10, ownerUserID, "hey "+handle+" what is up", handle)
	caseInsensitive := mentioning(10, ownerUserID, "@AURA_Test_BOT hello", "@AURA_Test_BOT")
	textMention := groupMessage(10, ownerUserID, "Aura hello")
	textMention.Message.Entities = []messageEntity{{Type: entityTextMention, Offset: 0, Length: 4, User: &user{ID: testBotID, IsBot: true}}}
	reply := replyingTo(groupMessage(10, ownerUserID, "and then?"), testBotID)
	command := groupMessage(10, ownerUserID, "/status"+handle+" now")
	command.Message.Entities = []messageEntity{entityOf(command.Message.Text, "/status"+handle, entityBotCommand)}
	emojiFirst := mentioning(10, ownerUserID, "😀 "+handle+" hi", handle)
	cases := []struct {
		name string
		item update
		want string
	}{
		{"mention at the start", mentioning(10, ownerUserID, handle+" hello there", handle), "hello there"},
		{"mention in the middle", mentionInMiddle, "hey what is up"},
		{"mention is case insensitive", caseInsensitive, "hello"},
		{"mention after an astral character", emojiFirst, "😀 hi"},
		{"text mention of the bot", textMention, "hello"},
		{"reply to a message the bot sent", reply, "and then?"},
		{"command addressed to the bot", command, "/status now"},
		{"other mentions stay as written", mentioning(10, ownerUserID, handle+" ping @alice", handle), "ping @alice"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rig := newGroupRig(t, accessOpts{})
			rig.runGroup(t, tc.item)
			if got := rig.onlyText(t); got != tc.want {
				t.Errorf("model text = %q, want %q", got, tc.want)
			}
			id, class, conversation := rig.onlyEnvelope(t)
			if id != principal.OwnerID || class != string(principal.ClassOwner) {
				t.Errorf("principal = %q %q, want the owner", id, class)
			}
			if want := "main/telegram:pocket/chat:" + strconv.Itoa(groupChatID); conversation != want {
				t.Errorf("conversation = %q, want %q", conversation, want)
			}
			if got := rig.dropped(); len(got) != 0 {
				t.Errorf("admitted message produced drop records %v", got)
			}
			var reference replyReference
			if err := json.Unmarshal(rig.sink.accepted()[0].ReplyContext, &reference); err != nil || reference.ChatID != groupChatID || reference.MessageID != 100 || reference.ThreadID != 0 {
				t.Errorf("reply context = %s (%v)", rig.sink.accepted()[0].ReplyContext, err)
			}
		})
	}
}

func TestGroupMessagesThatDoNotAddressTheBotAreDropped(t *testing.T) {
	handle := "@" + testBotName
	otherMention := mentioning(10, ownerUserID, "@"+otherBotName+" hello", "@"+otherBotName)
	otherUser := groupMessage(10, ownerUserID, "Alice hello")
	otherUser.Message.Entities = []messageEntity{{Type: entityTextMention, Offset: 0, Length: 5, User: &user{ID: 31337}}}
	bareCommand := groupMessage(10, ownerUserID, "/status now")
	bareCommand.Message.Entities = []messageEntity{entityOf("/status now", "/status", entityBotCommand)}
	otherCommand := groupMessage(10, ownerUserID, "/status@"+otherBotName+" now")
	otherCommand.Message.Entities = []messageEntity{entityOf(otherCommand.Message.Text, "/status@"+otherBotName, entityBotCommand)}
	topicCreation := replyingTo(inTopic(groupMessage(10, ownerUserID, "first post"), 9), testBotID)
	topicCreation.Message.ReplyTo.ForumTopicCreated = &forumTopicCreation{}
	outOfRange := groupMessage(10, ownerUserID, "short")
	outOfRange.Message.Entities = []messageEntity{{Type: entityMention, Offset: 2, Length: 40}, {Type: entityMention, Offset: -1, Length: 3}, {Type: entityMention, Offset: 0, Length: 0}}
	plainTextWithHandle := groupMessage(10, ownerUserID, "talking about "+handle+" without an entity")
	cases := map[string]update{
		"plain text":                       groupMessage(10, ownerUserID, "lunch anyone"),
		"mention of another bot":           otherMention,
		"text mention of another user":     otherUser,
		"reply to another user":            replyingTo(groupMessage(10, ownerUserID, "agreed"), 31337),
		"unaddressed command":              bareCommand,
		"command for another bot":          otherCommand,
		"reply to a topic creation notice": topicCreation,
		"out of range entities":            outOfRange,
		"handle without an entity":         plainTextWithHandle,
	}
	for name, item := range cases {
		t.Run(name, func(t *testing.T) {
			rig := newGroupRig(t, accessOpts{})
			rig.runGroup(t, item)
			rig.assertQuiet(t, "not_mentioned")
			if sends := rig.h.api.sends(); len(sends) != 0 {
				t.Errorf("dropped message produced %d replies", len(sends))
			}
		})
	}
}

func TestGroupMentionWithoutTextIsEmptyContent(t *testing.T) {
	handle := "@" + testBotName
	rig := newGroupRig(t, accessOpts{})
	rig.runGroup(t, mentioning(10, ownerUserID, handle+"  ", handle))
	rig.assertQuiet(t, "empty_content")
}

func TestGroupOutsideTheAllowlistIsDropped(t *testing.T) {
	handle := "@" + testBotName
	foreign := mentioning(10, ownerUserID, handle+" hello", handle)
	foreign.Message.Chat.ID = otherChatID
	rig := newGroupRig(t, accessOpts{})
	rig.runGroup(t, foreign)
	rig.assertQuiet(t, "chat_not_allowed")
	if sends := rig.h.api.sends(); len(sends) != 0 {
		t.Errorf("non-allowlisted group received %d replies", len(sends))
	}
}

func TestGroupChatTypesOtherThanGroupsStayUnsupported(t *testing.T) {
	handle := "@" + testBotName
	channelPost := mentioning(10, ownerUserID, handle+" hello", handle)
	channelPost.Message.Chat.Type = "channel"
	rig := newGroupRig(t, accessOpts{})
	rig.runGroup(t, channelPost)
	rig.assertQuiet(t, "chat_not_supported")
}

func TestGroupsStayDisabledWithoutAllowedChats(t *testing.T) {
	handle := "@" + testBotName
	rig := newAccessRig(t, accessOpts{})
	rig.run(t, mentioning(10, ownerUserID, handle+" hello", handle))
	rig.assertQuiet(t, "chat_not_supported")
	if calls := rig.h.api.count("getMe"); calls != 0 {
		t.Errorf("getMe was called %d times for an account that serves no group", calls)
	}
}

func TestGroupMessagesAreDroppedUntilTheBotIdentityIsKnown(t *testing.T) {
	handle := "@" + testBotName
	rig := newGroupRig(t, accessOpts{})
	rig.h.api.setMe(true, testBotName)
	stop := startAdapter(t, rig.adapter, rig.sink)
	defer stop()
	eventually(t, "second identity attempt", func() bool { return rig.h.api.count("getMe") >= 2 })
	rig.h.api.queue(mentioning(10, ownerUserID, handle+" early", handle), privateText(11, ownerUserID, "private is unaffected"))
	awaitCursor(t, rig.h, 12)
	if got := rig.dropped(); !slices.Equal(got, []string{"bot_identity_unknown"}) {
		t.Fatalf("drop records = %v, want one bot_identity_unknown", got)
	}
	if accepted := rig.sink.accepted(); len(accepted) != 1 || accepted[0].Parts[0].Text != "private is unaffected" {
		t.Fatalf("accepted = %d envelopes, want only the private message", len(accepted))
	}
	rig.h.api.setMe(false, testBotName)
	eventually(t, "bot identity lookup", rig.identityKnown)
	rig.h.api.queue(mentioning(12, ownerUserID, handle+" later", handle))
	awaitCursor(t, rig.h, 13)
	if accepted := rig.sink.accepted(); len(accepted) != 2 || accepted[1].Parts[0].Text != "later" {
		t.Fatalf("accepted = %d envelopes, want the group message once the identity is known", len(accepted))
	}
}

func TestBotIdentityIsRefreshedAfterPollRecovery(t *testing.T) {
	rig := newGroupRig(t, accessOpts{})
	stop := startAdapter(t, rig.adapter, rig.sink)
	defer stop()
	eventually(t, "bot identity lookup", rig.identityKnown)
	rig.h.api.setMe(false, "renamed_bot")
	rig.h.api.failNextOn("getUpdates", apiFailure(500, 0))
	eventually(t, "identity refresh after recovery", func() bool {
		self, known := rig.adapter.selfIdentity()
		return known && self.Username == "renamed_bot"
	})
	handle := "@renamed_bot"
	rig.h.api.queue(mentioning(10, ownerUserID, handle+" hello", handle))
	awaitCursor(t, rig.h, 11)
	if got := rig.onlyText(t); got != "hello" {
		t.Errorf("model text = %q, want hello", got)
	}
}

func TestIdentityLookupFailuresNeverLeakTheToken(t *testing.T) {
	rig := newGroupRig(t, accessOpts{})
	rig.h.api.setMe(true, testBotName)
	stop := startAdapter(t, rig.adapter, rig.sink)
	defer stop()
	eventually(t, "identity attempts", func() bool { return rig.h.api.count("getMe") >= 2 })
	if logs := rig.h.logs.String(); strings.Contains(logs, testToken) || !strings.Contains(logs, "bot identity lookup failed") {
		t.Errorf("logs = %s", logs)
	}
}

func TestForumTopicsAreSeparateConversationsAndRepliesStayInTheTopic(t *testing.T) {
	handle := "@" + testBotName
	cases := []struct {
		name string
		item update
		want string
		wire int64
	}{
		{"topic message", inTopic(mentioning(10, ownerUserID, handle+" a", handle), 7), "chat:-1001234567890/topic:7", 7},
		{"another topic", inTopic(mentioning(10, ownerUserID, handle+" a", handle), 8), "chat:-1001234567890/topic:8", 8},
		{"general topic without a thread id", mentioning(10, ownerUserID, handle+" a", handle), "chat:-1001234567890", 0},
		{"reply thread of a plain supergroup", func() update {
			item := mentioning(10, ownerUserID, handle+" a", handle)
			item.Message.ThreadID = 40
			return item
		}(), "chat:-1001234567890", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rig := newGroupRig(t, accessOpts{})
			rig.runGroup(t, tc.item)
			_, _, conversation := rig.onlyEnvelope(t)
			if want := "main/telegram:pocket/" + tc.want; conversation != want {
				t.Errorf("conversation = %q, want %q", conversation, want)
			}
			var reference replyReference
			if err := json.Unmarshal(rig.sink.accepted()[0].ReplyContext, &reference); err != nil || reference.ThreadID != tc.wire {
				t.Errorf("reply context = %s (%v), want thread %d", rig.sink.accepted()[0].ReplyContext, err, tc.wire)
			}
			scopes := rig.scopes.recorded()
			if len(scopes) != 1 || scopes[0].ChatID != strconv.Itoa(groupChatID) || scopes[0].UserID != strconv.Itoa(ownerUserID) {
				t.Fatalf("route scopes = %+v", scopes)
			}
			wantThread := ""
			if tc.wire != 0 {
				wantThread = strconv.FormatInt(tc.wire, 10)
			}
			if scopes[0].ThreadID != wantThread {
				t.Errorf("route thread = %q, want %q", scopes[0].ThreadID, wantThread)
			}
		})
	}
}

func TestPrivateChatsRouteWithoutChatOrThread(t *testing.T) {
	rig := newGroupRig(t, accessOpts{})
	rig.run(t, privateText(10, ownerUserID, "hello"))
	scopes := rig.scopes.recorded()
	if len(scopes) != 1 || scopes[0].ChatID != "" || scopes[0].ThreadID != "" || scopes[0].UserID != strconv.Itoa(ownerUserID) {
		t.Fatalf("route scopes = %+v", scopes)
	}
}

func TestGroupConversationKeysAreScopedPerPrincipalUnlessShared(t *testing.T) {
	handle := "@" + testBotName
	trusted := strconv.Itoa(allowedUserID)
	cases := []struct {
		name       string
		definition auraagent.Definition
		item       update
		want       string
	}{
		{"owner shares the group", auraagent.Definition{ID: testPersona, Audience: config.AudiencePublic}, mentioning(10, ownerUserID, handle+" a", handle), "chat:-1001234567890"},
		{"non-owner is per principal", auraagent.Definition{ID: testPersona, Audience: config.AudiencePublic}, mentioning(10, allowedUserID, handle+" a", handle), "chat:-1001234567890/principal:telegram:" + trusted},
		{"non-owner in a topic is per principal", auraagent.Definition{ID: testPersona, Audience: config.AudiencePublic}, inTopic(mentioning(10, allowedUserID, handle+" a", handle), 7), "chat:-1001234567890/topic:7/principal:telegram:" + trusted},
		{"shared group context", auraagent.Definition{ID: testPersona, Audience: config.AudiencePublic, GroupContext: config.GroupContextShared}, mentioning(10, allowedUserID, handle+" a", handle), "chat:-1001234567890"},
		{"shared group context in a topic", auraagent.Definition{ID: testPersona, Audience: config.AudiencePublic, GroupContext: config.GroupContextShared}, inTopic(mentioning(10, allowedUserID, handle+" a", handle), 7), "chat:-1001234567890/topic:7"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rig := newGroupRig(t, accessOpts{allowed: []string{trusted}, definition: tc.definition})
			rig.runGroup(t, tc.item)
			_, _, conversation := rig.onlyEnvelope(t)
			if want := "main/telegram:pocket/" + tc.want; conversation != want {
				t.Errorf("conversation = %q, want %q", conversation, want)
			}
		})
	}
}

func TestGroupSendersResolveToTheSameClassesAsPrivateChats(t *testing.T) {
	handle := "@" + testBotName
	granted := &fakePairing{granted: map[string]bool{"telegram:" + strconv.Itoa(grantedUserID) + "@pocket": true}}
	cases := []struct {
		name      string
		userID    int64
		public    bool
		wantID    string
		wantClass principal.Class
		wantDrops []string
		wantDeny  []string
	}{
		{name: "owner", userID: ownerUserID, wantID: principal.OwnerID, wantClass: principal.ClassOwner},
		{name: "allowlisted", userID: allowedUserID, wantID: "telegram:555001", wantClass: principal.ClassTrusted},
		{name: "pairing granted", userID: grantedUserID, wantID: "telegram:555002", wantClass: principal.ClassTrusted},
		{name: "served public", userID: publicUserID, public: true, wantID: "telegram:555003", wantClass: principal.ClassPublic},
		{name: "unknown", userID: strangerID, wantDrops: []string{"author_not_allowed"}, wantDeny: []string{DeniedReasonIdentity}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rig := newGroupRig(t, accessOpts{allowed: []string{strconv.Itoa(allowedUserID)}, public: tc.public, settings: AccessSettings{Pairing: granted}})
			rig.runGroup(t, mentioning(10, tc.userID, handle+" hello", handle))
			if tc.wantID == "" {
				rig.assertQuiet(t, tc.wantDrops...)
				if got := rig.h.denied(); !slices.Equal(got, tc.wantDeny) {
					t.Fatalf("denials = %v, want %v", got, tc.wantDeny)
				}
				return
			}
			id, class, _ := rig.onlyEnvelope(t)
			if id != tc.wantID || class != string(tc.wantClass) {
				t.Errorf("principal = %q %q, want %q %q", id, class, tc.wantID, tc.wantClass)
			}
		})
	}
}

func TestPairingNeverIssuesACodeInAGroup(t *testing.T) {
	handle := "@" + testBotName
	pairing := &fakePairing{result: access.PairingResult{Outcome: access.PairingOutcomeIssued, Code: "ABCD-2345"}}
	trusted := auraagent.Definition{ID: testPersona, Audience: config.AudienceTrusted, Pairing: true}
	t.Run("unknown sender", func(t *testing.T) {
		rig := newGroupRig(t, accessOpts{definition: trusted, settings: AccessSettings{Pairing: pairing}})
		rig.runGroup(t, mentioning(10, strangerID, handle+" let me in", handle))
		rig.assertQuiet(t, "author_not_allowed")
	})
	t.Run("public sender below the audience", func(t *testing.T) {
		rig := newGroupRig(t, accessOpts{definition: trusted, public: true, settings: AccessSettings{Pairing: pairing}})
		rig.runGroup(t, mentioning(10, publicUserID, handle+" let me in", handle))
		rig.assertQuiet(t, "audience")
	})
	if requests := pairing.requested(); len(requests) != 0 {
		t.Errorf("pairing requests from a group = %v", requests)
	}
}

func TestGroupSendersPassThroughTheSharedGates(t *testing.T) {
	handle := "@" + testBotName
	trustedID := strconv.Itoa(allowedUserID)
	t.Run("budget refusal answers inside the topic", func(t *testing.T) {
		budgets := &fakeBudgets{refusal: access.BudgetRefusalExhausted}
		rig := newGroupRig(t, accessOpts{allowed: []string{trustedID}, settings: AccessSettings{Budgets: budgets}})
		rig.runGroup(t, inTopic(mentioning(10, allowedUserID, handle+" hello", handle), 7))
		rig.assertQuiet(t, "budget_exhausted")
		sends := rig.h.api.sends()
		if len(sends) != 1 || sends[0]["chat_id"] != float64(groupChatID) || sends[0]["message_thread_id"] != float64(7) {
			t.Fatalf("refusal sends = %v, want one inside the topic", sends)
		}
		if calls := budgets.recorded(); len(calls) != 1 || calls[0].principalID != "telegram:555001" {
			t.Errorf("budget calls = %+v", calls)
		}
	})
	t.Run("eval gate answers in the group", func(t *testing.T) {
		evals := &fakeEvals{}
		trusted := auraagent.Definition{ID: testPersona, Audience: config.AudienceTrusted}
		rig := newGroupRig(t, accessOpts{allowed: []string{trustedID}, definition: trusted, settings: AccessSettings{Evals: evals}})
		rig.runGroup(t, mentioning(10, allowedUserID, handle+" hello", handle))
		rig.assertQuiet(t, "persona_eval_missing")
		if got := sentTo(rig.h.api, groupChatID); len(got) != 1 {
			t.Errorf("group notices = %v, want one", got)
		}
	})
	t.Run("forget-me addressed to the bot", func(t *testing.T) {
		eraser := &fakeEraser{}
		rig := newGroupRig(t, accessOpts{allowed: []string{trustedID}, settings: AccessSettings{Erasure: eraser}})
		command := groupMessage(10, allowedUserID, "/forgetme"+handle+" confirm")
		command.Message.Entities = []messageEntity{entityOf(command.Message.Text, "/forgetme"+handle, entityBotCommand)}
		rig.runGroup(t, command)
		if got := eraser.erased(); !slices.Equal(got, []string{"telegram:555001"}) {
			t.Errorf("erased = %v", got)
		}
		if accepted := rig.sink.accepted(); len(accepted) != 0 {
			t.Errorf("forget-me reached the runtime: %d envelopes", len(accepted))
		}
	})
	t.Run("open handoff holds the message", func(t *testing.T) {
		handoffs := &fakeHandoffs{state: access.HandoffStateOpen}
		rig := newGroupRig(t, accessOpts{allowed: []string{trustedID}, settings: AccessSettings{Handoffs: handoffs}})
		rig.runGroup(t, mentioning(10, allowedUserID, handle+" hello", handle))
		if accepted := rig.sink.accepted(); len(accepted) != 0 {
			t.Errorf("held message reached the runtime: %d envelopes", len(accepted))
		}
		if want := "main/telegram:pocket/chat:-1001234567890/principal:telegram:555001"; len(handoffs.sessions) != 1 || handoffs.sessions[0] != want {
			t.Errorf("handoff sessions = %v, want %q", handoffs.sessions, want)
		}
	})
}

func TestGroupDropLogsCarryNoContentOrIdentifiers(t *testing.T) {
	handle := "@" + testBotName
	rig := newGroupRig(t, accessOpts{})
	foreign := mentioning(10, ownerUserID, handle+" secret-phrase", handle)
	foreign.Message.Chat.ID = otherChatID
	rig.runGroup(t, foreign, groupMessage(11, ownerUserID, "unaddressed secret-phrase"), mentioning(12, strangerID, handle+" secret-phrase", handle))
	logs := rig.h.logs.String()
	for _, leak := range []string{"secret-phrase", strconv.Itoa(groupChatID), strconv.Itoa(otherChatID), strconv.Itoa(strangerID), testBotName} {
		if strings.Contains(logs, leak) {
			t.Errorf("logs carry %q: %s", leak, logs)
		}
	}
}

func groupDelivery(t *testing.T, h *harness, effectID string, thread int64) *runtimechannelhost.DeliveryRequest {
	t.Helper()
	req := deliveryRequest(t, h, effectID, "reply text")
	reference, err := json.Marshal(replyReference{ChatID: groupChatID, MessageID: 100, ThreadID: thread})
	if err != nil {
		t.Fatalf("marshal reply context: %v", err)
	}
	req.ReplyContext = reference
	return req
}

func TestDeliveryCarriesTheTopicThread(t *testing.T) {
	h := newHarness(t)
	adapter := h.adapter(t, nil)
	if _, err := adapter.Deliver(t.Context(), groupDelivery(t, h, "turn-topic", 7)); err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	sends := h.api.sends()
	if len(sends) != 1 || sends[0]["chat_id"] != float64(groupChatID) || sends[0]["message_thread_id"] != float64(7) {
		t.Fatalf("sends = %v, want one into thread 7", sends)
	}
	reply, _ := sends[0]["reply_parameters"].(map[string]any)
	if reply["message_id"] != float64(100) {
		t.Errorf("reply parameters = %v, want a reply to the trigger", reply)
	}
}

func TestDeliveryWithoutAThreadOmitsIt(t *testing.T) {
	h := newHarness(t)
	adapter := h.adapter(t, nil)
	if _, err := adapter.Deliver(t.Context(), groupDelivery(t, h, "turn-plain", 0)); err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	if _, present := h.api.sends()[0]["message_thread_id"]; present {
		t.Error("a message without a topic carried message_thread_id")
	}
}

func TestReplyContextWithoutAThreadStillDelivers(t *testing.T) {
	h := newHarness(t)
	adapter := h.adapter(t, nil)
	req := deliveryRequest(t, h, "turn-legacy", "reply text")
	req.ReplyContext = json.RawMessage(`{"chat_id":424242,"message_id":100}`)
	if _, err := adapter.Deliver(t.Context(), req); err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	sends := h.api.sends()
	if len(sends) != 1 || sends[0]["chat_id"] != float64(ownerUserID) {
		t.Fatalf("sends = %v", sends)
	}
	if _, present := sends[0]["message_thread_id"]; present {
		t.Error("a legacy reply context produced a thread id")
	}
}

func TestPresenceStaysInsideTheTopic(t *testing.T) {
	h := newHarness(t)
	adapter := presenceAdapter(t, h, 30*time.Millisecond, 15*time.Millisecond, 0)
	req := groupDelivery(t, h, "turn-presence-topic", 7)
	req.Parts = nil
	if err := adapter.PresenceStarted(t.Context(), req); err != nil {
		t.Fatalf("PresenceStarted: %v", err)
	}
	eventually(t, "status post", func() bool { return h.api.count("sendMessage") == 1 })
	presenceUpdate(t, adapter, req, "searching", "", false)
	eventually(t, "status edit", func() bool { return h.api.count("editMessageText") >= 1 })
	endPresence(t, adapter, "turn-presence-topic")
	for _, body := range h.api.bodies("sendChatAction") {
		if body["chat_id"] != float64(groupChatID) || body["message_thread_id"] != float64(7) {
			t.Errorf("typing action = %v, want it inside the topic", body)
		}
	}
	status := h.api.sends()[0]
	if status["chat_id"] != float64(groupChatID) || status["message_thread_id"] != float64(7) {
		t.Errorf("status post = %v, want it inside the topic", status)
	}
	if edit := h.api.bodies("editMessageText")[0]; edit["chat_id"] != float64(groupChatID) {
		t.Errorf("status edit = %v", edit)
	}
}

func TestGroupAdmissionHonoursCancellation(t *testing.T) {
	rig := newGroupRig(t, accessOpts{})
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- rig.adapter.Start(ctx, rig.sink) }()
	eventually(t, "bot identity lookup", rig.identityKnown)
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Start: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Start did not return after cancel with the identity loop running")
	}
}
