package discord

import (
	"encoding/json"
	"testing"

	"github.com/anggasct/aura/internal/config"
)

func testDiscordConfig() *config.Discord {
	return &config.Discord{
		Enabled:            true,
		Instance:           "owner",
		BotTokenRef:        "env://AURA_DISCORD_BOT_TOKEN",
		AllowedUserIDs:     []string{"111"},
		AllowedGuildIDs:    []string{"222"},
		AllowedChannelIDs:  []string{"333"},
		AcceptDMs:          true,
		MinEditInterval:    2000000000,
		MaxAttachmentBytes: 20971520,
	}
}

func testMessage() *messagePayload {
	return &messagePayload{
		ID:        "1001",
		ChannelID: "333",
		GuildID:   "222",
		Author:    userPayload{ID: "111"},
		Content:   "hello aura",
		Mentions:  []userPayload{{ID: "999"}},
	}
}

func TestNormalizeMessage_AdmissionMatrix(t *testing.T) {
	selfID := "999"
	cases := map[string]struct {
		mutate    func(*config.Discord, *messagePayload)
		admit     bool
		conv      string
		extID     string
		principal string
	}{
		"guild mention admitted": {
			mutate:    func(*config.Discord, *messagePayload) {},
			admit:     true,
			conv:      "channel:333",
			extID:     "message:1001",
			principal: "111",
		},
		"dm admitted": {
			mutate: func(cfg *config.Discord, msg *messagePayload) {
				msg.GuildID = ""
				msg.Mentions = nil
			},
			admit:     true,
			conv:      "dm:111",
			extID:     "message:1001",
			principal: "111",
		},
		"non-allowlisted author denied": {
			mutate: func(_ *config.Discord, msg *messagePayload) { msg.Author.ID = "666" },
		},
		"bot author denied": {
			mutate: func(_ *config.Discord, msg *messagePayload) { msg.Author.Bot = true },
		},
		"own message denied": {
			mutate: func(_ *config.Discord, msg *messagePayload) { msg.Author.ID = selfID },
		},
		"empty content ignored": {
			mutate: func(_ *config.Discord, msg *messagePayload) { msg.Content = "" },
		},
		"guild message without mention denied": {
			mutate: func(_ *config.Discord, msg *messagePayload) { msg.Mentions = nil },
		},
		"guild message outside channel scope denied": {
			mutate: func(_ *config.Discord, msg *messagePayload) { msg.ChannelID = "444" },
		},
		"guild message outside guild scope denied": {
			mutate: func(_ *config.Discord, msg *messagePayload) { msg.GuildID = "555" },
		},
		"dm refused when dms closed": {
			mutate: func(cfg *config.Discord, msg *messagePayload) {
				cfg.AcceptDMs = false
				msg.GuildID = ""
				msg.Mentions = nil
			},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			cfg := testDiscordConfig()
			msg := testMessage()
			tc.mutate(cfg, msg)
			env, admitted, err := normalizeMessage(cfg, selfID, cfg.Instance, msg)
			if err != nil {
				t.Fatalf("normalizeMessage: %v", err)
			}
			if admitted != tc.admit {
				t.Fatalf("admitted = %v, want %v", admitted, tc.admit)
			}
			if !tc.admit {
				return
			}
			if env.Source != "discord:owner" {
				t.Errorf("source = %q", env.Source)
			}
			if env.ConversationID != tc.conv {
				t.Errorf("conversation = %q, want %q", env.ConversationID, tc.conv)
			}
			if env.ExternalID != tc.extID {
				t.Errorf("external id = %q, want %q", env.ExternalID, tc.extID)
			}
			if env.PrincipalID != tc.principal {
				t.Errorf("principal = %q, want %q", env.PrincipalID, tc.principal)
			}
			if len(env.Parts) != 1 || env.Parts[0].Text != "hello aura" {
				t.Errorf("parts = %+v", env.Parts)
			}
			var reference replyReference
			if err := json.Unmarshal(env.ReplyContext, &reference); err != nil {
				t.Fatalf("reply context: %v", err)
			}
			if reference.MessageID != "1001" {
				t.Errorf("reply message id = %q", reference.MessageID)
			}
			if env.ReceivedAt.IsZero() {
				t.Error("received_at is zero")
			}
		})
	}
}

func TestNormalizeMessage_RejectsMalformed(t *testing.T) {
	cfg := testDiscordConfig()
	for name, msg := range map[string]*messagePayload{
		"missing id":      {ChannelID: "333", Author: userPayload{ID: "111"}, Content: "hi"},
		"missing channel": {ID: "1001", Author: userPayload{ID: "111"}, Content: "hi"},
		"missing author":  {ID: "1001", ChannelID: "333", Content: "hi"},
	} {
		t.Run(name, func(t *testing.T) {
			_, _, err := normalizeMessage(cfg, "999", "owner", msg)
			if code, ok := CodeOf(err); !ok || code != ErrorCodeProtocolInvalid {
				t.Errorf("code = %v, %v; want protocol_invalid", code, ok)
			}
		})
	}
	if _, _, err := normalizeMessage(nil, "999", "owner", testMessage()); err == nil {
		t.Error("nil config accepted")
	}
}
