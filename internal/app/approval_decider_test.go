package app

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/channels/kit/approval"
	"github.com/anggasct/aura/internal/channels/terminal"
	"github.com/anggasct/aura/internal/platform/toolbroker"
)

type fakeApprovalChannel struct {
	sessions map[string]bool
	accept   bool
	err      error
	prompts  []approval.Prompt
}

func (f *fakeApprovalChannel) HoldsApprovalTarget(sessionID string) bool {
	return f.sessions[sessionID]
}

func (f *fakeApprovalChannel) Decide(_ context.Context, prompt *approval.Prompt) (bool, error) {
	f.prompts = append(f.prompts, *prompt)
	return f.accept, f.err
}

func brokerPrompt(sessionID string) *toolbroker.ApprovalPrompt {
	return &toolbroker.ApprovalPrompt{
		ToolName:       "shell.exec",
		ToolVersion:    "1",
		SessionID:      sessionID,
		TurnID:         "turn-1",
		PrincipalID:    "local-owner",
		Arguments:      `{"command":"ls"}`,
		Network:        true,
		Timeout:        time.Second,
		MaxOutputBytes: 64,
		PolicyVersion:  "v3",
		ReasonCode:     "approval_required",
		ExpiresAt:      time.Unix(1700000000, 0),
	}
}

func TestChannelApprovalDeciderRoutesToTheChannelHoldingTheSession(t *testing.T) {
	discordLike := &fakeApprovalChannel{sessions: map[string]bool{"main/discord:owner/dm:1": true}, accept: false}
	telegramLike := &fakeApprovalChannel{sessions: map[string]bool{"main/telegram:pocket/chat:2": true}, accept: true}
	decider := channelApprovalDecider([]approvalChannel{discordLike, telegramLike})
	cases := []struct {
		name       string
		session    string
		want       bool
		wantOnDisc int
		wantOnTele int
	}{
		{"discord session", "main/discord:owner/dm:1", false, 1, 0},
		{"telegram session", "main/telegram:pocket/chat:2", true, 1, 1},
		{"unheld session is denied", "main/other/chat:3", false, 1, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			accepted, err := decider(t.Context(), brokerPrompt(tc.session))
			if err != nil || accepted != tc.want {
				t.Errorf("decider() = %v, %v; want %v", accepted, err, tc.want)
			}
			if len(discordLike.prompts) != tc.wantOnDisc || len(telegramLike.prompts) != tc.wantOnTele {
				t.Errorf("prompts on discord = %d, telegram = %d; want %d and %d", len(discordLike.prompts), len(telegramLike.prompts), tc.wantOnDisc, tc.wantOnTele)
			}
		})
	}
}

func TestChannelApprovalDeciderCarriesTheWholePrompt(t *testing.T) {
	channel := &fakeApprovalChannel{sessions: map[string]bool{"s": true}}
	if _, err := channelApprovalDecider([]approvalChannel{channel})(t.Context(), brokerPrompt("s")); err != nil {
		t.Fatalf("decider(): %v", err)
	}
	want := approval.Prompt{
		ToolName: "shell.exec", ToolVersion: "1", SessionID: "s", TurnID: "turn-1", PrincipalID: "local-owner",
		Arguments: `{"command":"ls"}`, Network: true, Timeout: time.Second, MaxOutputBytes: 64,
		PolicyVersion: "v3", ReasonCode: "approval_required", ExpiresAt: time.Unix(1700000000, 0),
	}
	if len(channel.prompts) != 1 || channel.prompts[0] != want {
		t.Errorf("prompt = %+v, want %+v", channel.prompts, want)
	}
}

func TestChannelApprovalPromptCarriesTheStandingGrantFlag(t *testing.T) {
	for _, allowed := range []bool{true, false} {
		channel := &fakeApprovalChannel{sessions: map[string]bool{"s": true}}
		prompt := brokerPrompt("s")
		prompt.StandingGrantAllowed = allowed
		if _, err := channelApprovalDecider([]approvalChannel{channel})(t.Context(), prompt); err != nil {
			t.Fatalf("decider(): %v", err)
		}
		if len(channel.prompts) != 1 || channel.prompts[0].StandingGrantAllowed != allowed {
			t.Errorf("channel prompts = %+v, want StandingGrantAllowed %t", channel.prompts, allowed)
		}
	}
}

func TestChannelApprovalDeciderPropagatesTheChannelError(t *testing.T) {
	failure := errors.New("owner approval unavailable")
	channel := &fakeApprovalChannel{sessions: map[string]bool{"s": true}, err: failure}
	accepted, err := channelApprovalDecider([]approvalChannel{channel})(t.Context(), brokerPrompt("s"))
	if accepted || !errors.Is(err, failure) {
		t.Errorf("decider() = %v, %v; want a denial carrying the channel error", accepted, err)
	}
}

func TestChannelApprovalDeciderFailsClosed(t *testing.T) {
	if channelApprovalDecider(nil) != nil {
		t.Error("a decider was built without channels")
	}
	decider := channelApprovalDecider([]approvalChannel{&fakeApprovalChannel{}})
	if accepted, err := decider(t.Context(), nil); accepted || err == nil {
		t.Errorf("decider(nil) = %v, %v; want a denial with an error", accepted, err)
	}
}

func TestChannelApprovalReachableMirrorsTheDeciderRouting(t *testing.T) {
	discordLike := &fakeApprovalChannel{sessions: map[string]bool{"main/discord:owner/dm:1": true}}
	telegramLike := &fakeApprovalChannel{sessions: map[string]bool{"main/telegram:pocket/chat:2": true}}
	channels := []approvalChannel{discordLike, telegramLike}
	reachable := channelApprovalReachable(channels)
	for session, want := range map[string]bool{
		"main/discord:owner/dm:1":     true,
		"main/telegram:pocket/chat:2": true,
		"main/other/chat:3":           false,
		"":                            false,
	} {
		if got := reachable(session); got != want {
			t.Errorf("reachable(%q) = %t, want %t", session, got, want)
		}
	}
	if len(discordLike.prompts)+len(telegramLike.prompts) != 0 {
		t.Error("asking whether a session is reachable must never prompt anyone")
	}
	if channelApprovalReachable(nil) != nil {
		t.Error("no channels must give a nil reachability, which the broker treats as unreachable")
	}
}

func TestTerminalApprovalReachableOnlyWhenTheBridgeExists(t *testing.T) {
	if terminalApprovalReachable(nil) != nil {
		t.Error("a chat without an approval bridge must be unreachable")
	}
	reachable := terminalApprovalReachable(terminal.NewApprovalBridge())
	if reachable == nil || !reachable("any-session") {
		t.Error("a TTY chat with the approval bridge must be reachable for every session")
	}
}
