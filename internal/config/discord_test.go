package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeDiscordConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	return path
}

func TestLoad_DiscordDefaults(t *testing.T) {
	path := writeDiscordConfig(t, "version: 1\n")
	res, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	discord := res.Config.Channels.Discord
	if discord.Enabled {
		t.Error("discord must be disabled by default")
	}
	if discord.Instance != "owner" {
		t.Errorf("instance = %q", discord.Instance)
	}
	if discord.BotTokenRef != "env://AURA_DISCORD_BOT_TOKEN" {
		t.Errorf("bot_token_ref = %q", discord.BotTokenRef)
	}
	if !discord.AcceptDMs {
		t.Error("accept_dms must default to true")
	}
	if discord.MinEditInterval != Duration(2*time.Second) {
		t.Errorf("min_edit_interval = %v", discord.MinEditInterval)
	}
	if discord.MaxAttachmentBytes != 20971520 {
		t.Errorf("max_attachment_bytes = %d", discord.MaxAttachmentBytes)
	}
}

func TestLoad_DiscordEnabledValid(t *testing.T) {
	path := writeDiscordConfig(t, `version: 1
channels:
  discord:
    enabled: true
    instance: owner
    bot_token_ref: env://AURA_DISCORD_BOT_TOKEN
    allowed_user_ids:
      - "123456789012345678"
    allowed_guild_ids:
      - "987654321098765432"
    accept_dms: false
`)
	res, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	discord := res.Config.Channels.Discord
	if !discord.Enabled || discord.AcceptDMs {
		t.Fatalf("discord = %+v", discord)
	}
	if len(discord.AllowedUserIDs) != 1 || discord.AllowedUserIDs[0] != "123456789012345678" {
		t.Errorf("allowed_user_ids = %v", discord.AllowedUserIDs)
	}
	if discord.MinEditInterval != Duration(2*time.Second) {
		t.Errorf("min_edit_interval default not applied: %v", discord.MinEditInterval)
	}
}

func TestLoad_DiscordEnvOverride(t *testing.T) {
	t.Setenv("AURA_CHANNELS_DISCORD_INSTANCE", "second")
	t.Setenv("AURA_CHANNELS_DISCORD_ACCEPT_DMS", "false")
	path := writeDiscordConfig(t, "version: 1\n")
	res, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	discord := res.Config.Channels.Discord
	if discord.Instance != "second" {
		t.Errorf("instance = %q, want second", discord.Instance)
	}
	if discord.AcceptDMs {
		t.Error("accept_dms env override not applied")
	}
}
func TestLoad_DiscordInvalid(t *testing.T) {
	cases := map[string]string{
		"enabled without users": `version: 1
channels:
  discord:
    enabled: true
`,
		"enabled without token ref": `version: 1
channels:
  discord:
    enabled: true
    bot_token_ref: ""
    allowed_user_ids:
      - "123456789012345678"
`,
		"bad token ref scheme": `version: 1
channels:
  discord:
    bot_token_ref: AURA_DISCORD_BOT_TOKEN
`,
		"non-numeric user id": `version: 1
channels:
  discord:
    allowed_user_ids:
      - "not-a-snowflake"
`,
		"duplicate user ids": `version: 1
channels:
  discord:
    allowed_user_ids:
      - "123456789012345678"
      - "123456789012345678"
`,
		"unquoted user id": `version: 1
channels:
  discord:
    allowed_user_ids:
      - 123456789012345678
`,
		"non-positive edit interval": `version: 1
channels:
  discord:
    min_edit_interval: 0s
`,
		"non-positive attachment cap": `version: 1
channels:
  discord:
    max_attachment_bytes: 0
`,
		"wrong enabled type": `version: 1
channels:
  discord:
    enabled: "yes"
`,
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			path := writeDiscordConfig(t, content)
			if _, err := Load(path); err == nil {
				t.Fatal("invalid discord config accepted")
			}
		})
	}
}
