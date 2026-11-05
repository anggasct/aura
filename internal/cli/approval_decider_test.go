package cli

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/channel/approval"
	"github.com/anggasct/aura/internal/toolbroker"
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

func TestChannelApprovalDeciderDeniesHTTPSessions(t *testing.T) {
	accepting := &fakeApprovalChannel{sessions: map[string]bool{"main/discord:owner/dm:1": true}, accept: true}
	decider := channelApprovalDecider([]approvalChannel{accepting})
	sessions := []string{
		"lobby/http/abcdefghijkl/abcdefghijklmnopqrstuvwxyz",
		"main/http/abcdefghijkl/a2b3c4d5e6f7g2h3i4j5k6l7m2",
	}
	for _, session := range sessions {
		accepted, err := decider(t.Context(), brokerPrompt(session))
		if accepted || err != nil {
			t.Errorf("decider(%s) = %v, %v; want a clean denial", session, accepted, err)
		}
	}
	if len(accepting.prompts) != 0 {
		t.Errorf("an http session reached a channel owner prompt: %+v", accepting.prompts)
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
