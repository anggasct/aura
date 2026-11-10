package telegram

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/channels/kit/approval"
)

func decideOnSession(t *testing.T, rig *accessRig, session, principalID string) <-chan decision {
	t.Helper()
	out := make(chan decision, 1)
	prompt := &approval.Prompt{
		ToolName:    "shell.exec",
		ToolVersion: "1",
		SessionID:   session,
		TurnID:      "turn-1",
		PrincipalID: principalID,
		Arguments:   `{"command":"ls"}`,
		ExpiresAt:   time.Now().Add(time.Minute),
	}
	go func() {
		accepted, err := rig.adapter.Decide(context.Background(), prompt)
		out <- decision{accepted: accepted, err: err}
	}()
	return out
}

func awaitCardPost(t *testing.T, rig *accessRig) postedCard {
	t.Helper()
	probe := &approvalRig{accessRig: rig}
	eventually(t, "approval card", func() bool { return len(probe.cards()) > 0 })
	return probe.cards()[0]
}

func callbackUpdate(updateID int64, id string, from int64, dest destination, messageID int64, data string) update {
	return update{UpdateID: updateID, CallbackQuery: pressQuery(id, from, dest, messageID, data)}
}

func awaitDecision(t *testing.T, result <-chan decision) decision {
	t.Helper()
	select {
	case outcome := <-result:
		return outcome
	case <-time.After(5 * time.Second):
		t.Fatal("no approval decision within 5s")
		return decision{}
	}
}

func TestPollSubscribesToMessagesAndCallbackQueries(t *testing.T) {
	rig := newAccessRig(t, accessOpts{})
	stop := startAdapter(t, rig.adapter, rig.sink)
	defer stop()
	rig.h.api.awaitPoll(t)
	polls := rig.h.api.bodies("getUpdates")
	if len(polls) == 0 {
		t.Fatal("no getUpdates call recorded")
	}
	allowed, _ := polls[0]["allowed_updates"].([]any)
	if len(allowed) != 2 || allowed[0] != "message" || allowed[1] != "callback_query" {
		t.Errorf("allowed_updates = %v, want message and callback_query", allowed)
	}
}

func TestOwnerApprovalThroughPollingInAPrivateChat(t *testing.T) {
	rig := newAccessRig(t, accessOpts{})
	stop := startAdapter(t, rig.adapter, rig.sink)
	defer stop()
	rig.h.api.queue(privateText(10, ownerUserID, "list the files"))
	awaitCursor(t, rig.h, 11)
	envelope := rig.sink.accepted()[0]
	result := decideOnSession(t, rig, envelope.ConversationID, envelope.PrincipalID)
	card := awaitCardPost(t, rig)
	if card.chatID != ownerUserID || card.threadID != 0 {
		t.Fatalf("card posted to %d/%d, want the owner's private chat", card.chatID, card.threadID)
	}
	rig.h.api.queue(callbackUpdate(11, "cb-1", ownerUserID, privateDest(ownerUserID), card.messageID, card.data(t, "Approve")))
	if outcome := awaitDecision(t, result); outcome.err != nil || !outcome.accepted {
		t.Fatalf("decision = %+v, want accepted", outcome)
	}
	awaitCursor(t, rig.h, 12)
	eventually(t, "keyboard removal", func() bool { return rig.h.api.count("editMessageReplyMarkup") == 1 })
	eventually(t, "callback answer", func() bool { return rig.h.api.count("answerCallbackQuery") == 1 })
	if calls, turns := rig.sink.stats("update:11"); calls != 0 || turns != 0 {
		t.Errorf("callback reached the runtime: calls=%d turns=%d", calls, turns)
	}
	if accepted := rig.sink.accepted(); len(accepted) != 1 {
		t.Errorf("accepted %d envelopes, want only the original message", len(accepted))
	}
	if got := rig.dropped(); len(got) != 0 {
		t.Errorf("drop records = %v, want none", got)
	}
}

func TestOwnerApprovalThroughPollingInAForumTopic(t *testing.T) {
	handle := "@" + testBotName
	rig := newGroupRig(t, accessOpts{})
	stop := startAdapter(t, rig.adapter, rig.sink)
	defer stop()
	eventually(t, "bot identity lookup", rig.identityKnown)
	rig.h.api.queue(inTopic(mentioning(10, ownerUserID, handle+" list the files", handle), forumTopic))
	awaitCursor(t, rig.h, 11)
	envelope := rig.sink.accepted()[0]
	result := decideOnSession(t, rig, envelope.ConversationID, envelope.PrincipalID)
	card := awaitCardPost(t, rig)
	if card.chatID != groupChatID || card.threadID != forumTopic {
		t.Fatalf("card posted to %d/%d, want the topic it came from", card.chatID, card.threadID)
	}
	dest := destination{chatID: groupChatID, threadID: forumTopic}
	rig.h.api.queue(callbackUpdate(11, "cb-1", ownerUserID, dest, card.messageID, card.data(t, "Reject")))
	if outcome := awaitDecision(t, result); outcome.err != nil || outcome.accepted {
		t.Fatalf("decision = %+v, want rejected", outcome)
	}
	awaitCursor(t, rig.h, 12)
}

func TestForgedCallbackThroughPollingIsAnsweredDroppedAndSkipped(t *testing.T) {
	rig := newAccessRig(t, accessOpts{})
	stop := startAdapter(t, rig.adapter, rig.sink)
	defer stop()
	rig.h.api.queue(privateText(10, ownerUserID, "list the files"))
	awaitCursor(t, rig.h, 11)
	envelope := rig.sink.accepted()[0]
	result := decideOnSession(t, rig, envelope.ConversationID, envelope.PrincipalID)
	card := awaitCardPost(t, rig)
	rig.h.api.queue(callbackUpdate(11, "cb-forged", strangerID, privateDest(ownerUserID), card.messageID, card.data(t, "Approve")))
	awaitCursor(t, rig.h, 12)
	eventually(t, "callback answer", func() bool { return rig.h.api.count("answerCallbackQuery") == 1 })
	if got := rig.dropped(); !slices.Equal(got, []string{string(dropCallbackRejected)}) {
		t.Errorf("drop records = %v, want one callback_rejected", got)
	}
	select {
	case outcome := <-result:
		t.Fatalf("forged callback settled the decision: %+v", outcome)
	default:
	}
	rig.h.api.queue(callbackUpdate(12, "cb-real", ownerUserID, privateDest(ownerUserID), card.messageID, card.data(t, "Approve")))
	if outcome := awaitDecision(t, result); outcome.err != nil || !outcome.accepted {
		t.Fatalf("decision = %+v, want accepted", outcome)
	}
	awaitCursor(t, rig.h, 13)
	if calls, _ := rig.sink.stats("update:11"); calls != 0 {
		t.Error("a callback update reached the runtime")
	}
	if got := len(rig.sink.accepted()); got != 1 {
		t.Errorf("accepted %d envelopes, want 1", got)
	}
}
