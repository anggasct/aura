package discord

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/anggasct/aura/internal/command"
)

func TestDiscordSkillInADMStartsOnePinnedTurn(t *testing.T) {
	f := newCommandFixture(t)
	text := "/skill review-pr check the auth module " + commandCanary
	f.admit(t, dmMessage("8800", "111", text))
	calls, turns := f.sink.stats("message:8800")
	if calls != 1 || turns != 1 {
		t.Fatalf("turns = %d/%d, want one", calls, turns)
	}
	env := f.sink.envelopes["message:8800"]
	want := command.SkillPin{SkillID: "local/review-pr@0123456789ab", Digest: "0123456789abcdef", InvocationID: "inv-1", PolicyVersion: "1"}
	if env.Skill == nil || *env.Skill != want {
		t.Fatalf("pin = %+v, want %+v", env.Skill, want)
	}
	if len(env.Parts) == 0 || env.Parts[0].Text != text {
		t.Errorf("turn input = %+v, want the original message", env.Parts)
	}
	if env.ConversationID != testSessionKey("dm:111") {
		t.Errorf("conversation = %s", env.ConversationID)
	}
	if replies := f.replies(t); len(replies) != 0 {
		t.Errorf("skill replied directly: %+v", replies)
	}
	if f.threadCreates() != 0 {
		t.Errorf("DM skill created a thread")
	}
	for _, req := range f.effects.requests {
		if req.Provider == ProviderCommand && (req.Operation != "skill" || strings.Contains(string(req.Request), "canary") || strings.Contains(string(req.Request), "review-pr")) {
			t.Errorf("command intent = %s %s", req.Operation, req.Request)
		}
	}
	if seen := f.recorder.observed(); !slices.Equal(seen, []commandObservation{{name: "skill", class: "owner", result: command.ResultOK}}) {
		t.Errorf("observed = %+v", seen)
	}
}

func TestDiscordSkillInAGuildParentCreatesTheThread(t *testing.T) {
	f := newCommandFixture(t)
	f.admit(t, threadTrigger("9300", "<@999> /skill pdf summarize this", []string{"999"}))
	if f.threadCreates() != 1 {
		t.Fatalf("threads created = %d, want 1", f.threadCreates())
	}
	env := f.sink.envelopes["message:9300"]
	if env == nil || env.Skill == nil || env.Skill.SkillID != "local/pdf@0123456789ab" {
		t.Fatalf("envelope = %+v", env)
	}
	if env.ConversationID != testSessionKey("thread:777") {
		t.Errorf("conversation = %s, want the new thread", env.ConversationID)
	}
	if replies := f.replies(t); len(replies) != 0 {
		t.Errorf("replies = %+v", replies)
	}
}

func TestDiscordSkillRedeliveryKeepsThePin(t *testing.T) {
	f := newCommandFixture(t)
	msg := dmMessage("8810", "111", "/skill pdf")
	f.admit(t, msg)
	f.admit(t, msg)
	if f.ports.pins != 1 {
		t.Errorf("activations = %d, want 1", f.ports.pins)
	}
	env := f.sink.envelopes["message:8810"]
	if env.Skill == nil || env.Skill.InvocationID != "inv-1" {
		t.Errorf("redelivered pin = %+v", env.Skill)
	}
	if _, turns := f.sink.stats("message:8810"); turns != 1 {
		t.Errorf("turns = %d, want 1", turns)
	}
	seen := f.recorder.observed()
	if len(seen) != 2 || seen[1] != (commandObservation{name: "skill", class: "owner", result: command.ResultReplayed}) {
		t.Errorf("observed = %+v", seen)
	}
}

func TestDiscordSkillFailuresReplyWithoutATurn(t *testing.T) {
	f := newCommandFixture(t)
	f.ports.skillErr = errors.New("skill_unavailable: skill is not available")
	f.admit(t, dmMessage("8820", "111", "/skill missing do it"))
	f.admit(t, dmMessage("8821", "112", "/skill pdf"))
	f.admit(t, dmMessage("8822", "111", "/skill"))
	want := []sentReply{
		{channel: "555", replyTo: "8820", content: "Skill missing is not available."},
		{channel: "555", replyTo: "8821", content: command.ReplyDenied},
		{channel: "555", replyTo: "8822", content: "usage: /skill <name> [request…]"},
	}
	if got := f.replies(t); !slices.Equal(got, want) {
		t.Fatalf("replies =\n%+v\nwant\n%+v", got, want)
	}
	for _, id := range []string{"message:8820", "message:8821", "message:8822"} {
		if calls, _ := f.sink.stats(id); calls != 0 {
			t.Errorf("%s reached the ingress sink", id)
		}
	}
	if f.ports.callCount() != 1 {
		t.Errorf("port calls = %v, want only the owner activation", f.ports.calls)
	}
}

func TestDiscordSkillCreateStagesADraft(t *testing.T) {
	f := newCommandFixture(t)
	f.admit(t, dmMessage("8830", "111", "/skill-create notes Keep tidy notes."))
	f.admit(t, dmMessage("8831", "111", "/skill-create notes"))
	want := []sentReply{
		{channel: "555", replyTo: "8830", content: "draft local/notes@0123456789ab\ndigest 0123456789abcdef\nreview with: aura skills review local/notes@0123456789ab"},
		{channel: "555", replyTo: "8831", content: "usage: /skill-create <name> <description…>"},
	}
	if got := f.replies(t); !slices.Equal(got, want) {
		t.Fatalf("replies =\n%+v\nwant\n%+v", got, want)
	}
}
