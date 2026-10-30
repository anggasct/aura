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
	if len(discord.Accounts) != 0 {
		t.Errorf("accounts = %+v, want none by default", discord.Accounts)
	}
	if discord.MinEditInterval != Duration(2*time.Second) {
		t.Errorf("min_edit_interval = %v", discord.MinEditInterval)
	}
	if discord.MaxAttachmentBytes != 20971520 {
		t.Errorf("max_attachment_bytes = %d", discord.MaxAttachmentBytes)
	}
	if discord.ThreadAutoArchiveMinutes != 1440 {
		t.Errorf("thread_auto_archive_minutes = %d", discord.ThreadAutoArchiveMinutes)
	}
	if len(res.Config.Bindings) != 0 {
		t.Errorf("bindings = %+v, want none by default", res.Config.Bindings)
	}
}

func TestLoad_DiscordAccountsValid(t *testing.T) {
	path := writeDiscordConfig(t, `version: 1
channels:
  discord:
    enabled: true
    accounts:
      - name: home
        bot_token_ref: env://DISCORD_HOME_TOKEN
        persona: main
        allowed_user_ids:
          - "123456789012345678"
        allowed_guild_ids:
          - "987654321098765432"
      - name: kuma
        bot_token_ref: file:///run/secrets/kuma
        allowed_user_ids:
          - "123456789012345678"
        accept_dms: false
`)
	res, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	discord := res.Config.Channels.Discord
	if !discord.Enabled || len(discord.Accounts) != 2 {
		t.Fatalf("discord = %+v", discord)
	}
	home, kuma := discord.Accounts[0], discord.Accounts[1]
	if home.Name != "home" || home.Persona != "main" || home.BotTokenRef != "env://DISCORD_HOME_TOKEN" {
		t.Errorf("home = %+v", home)
	}
	if !home.AcceptDMs {
		t.Error("accept_dms must default to true per account")
	}
	if kuma.AcceptDMs {
		t.Error("explicit accept_dms false was not kept")
	}
	if kuma.Persona != "" {
		t.Errorf("kuma persona = %q, want empty", kuma.Persona)
	}
	if discord.MinEditInterval != Duration(2*time.Second) {
		t.Errorf("min_edit_interval default not applied: %v", discord.MinEditInterval)
	}
}

func TestLoad_DiscordEnvOverride(t *testing.T) {
	t.Setenv("AURA_CHANNELS_DISCORD_MIN_EDIT_INTERVAL", "5s")
	path := writeDiscordConfig(t, "version: 1\n")
	res, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := res.Config.Channels.Discord.MinEditInterval; got != Duration(5*time.Second) {
		t.Errorf("min_edit_interval = %v, want 5s", got)
	}
}

func TestLoad_DiscordSingleBlockShapeChanged(t *testing.T) {
	cases := map[string]string{
		"instance":            "    instance: owner\n",
		"bot_token_ref":       "    bot_token_ref: env://AURA_DISCORD_BOT_TOKEN\n",
		"allowed_user_ids":    "    allowed_user_ids:\n      - \"123456789012345678\"\n",
		"allowed_guild_ids":   "    allowed_guild_ids: []\n",
		"allowed_channel_ids": "    allowed_channel_ids: []\n",
		"accept_dms":          "    accept_dms: true\n",
	}
	for name, line := range cases {
		t.Run(name, func(t *testing.T) {
			path := writeDiscordConfig(t, "version: 1\nchannels:\n  discord:\n    enabled: true\n"+line)
			_, err := Load(path)
			code, ok := CodeOf(err)
			if !ok || code != ErrorCodeConfigShapeChanged {
				t.Fatalf("Load error = %v, want %s", err, ErrorCodeConfigShapeChanged)
			}
		})
	}
}

func TestLoad_DiscordInvalid(t *testing.T) {
	account := func(extra string) string {
		return "version: 1\nchannels:\n  discord:\n    enabled: true\n    accounts:\n      - name: home\n        bot_token_ref: env://DISCORD_HOME_TOKEN\n        allowed_user_ids:\n          - \"123456789012345678\"\n" + extra
	}
	cases := map[string]string{
		"enabled without accounts": `version: 1
channels:
  discord:
    enabled: true
`,
		"enabled account without users": `version: 1
channels:
  discord:
    enabled: true
    accounts:
      - name: home
        bot_token_ref: env://DISCORD_HOME_TOKEN
`,
		"account without token ref": `version: 1
channels:
  discord:
    accounts:
      - name: home
`,
		"bad token ref scheme": `version: 1
channels:
  discord:
    accounts:
      - name: home
        bot_token_ref: DISCORD_HOME_TOKEN
`,
		"account name with separator": `version: 1
channels:
  discord:
    accounts:
      - name: home/x
        bot_token_ref: env://DISCORD_HOME_TOKEN
`,
		"account without name": `version: 1
channels:
  discord:
    accounts:
      - bot_token_ref: env://DISCORD_HOME_TOKEN
`,
		"duplicate account names": account("      - name: home\n        bot_token_ref: env://DISCORD_OTHER_TOKEN\n        allowed_user_ids:\n          - \"123456789012345678\"\n"),
		"shared token ref":        account("      - name: kuma\n        bot_token_ref: env://DISCORD_HOME_TOKEN\n        allowed_user_ids:\n          - \"123456789012345678\"\n"),
		"non-numeric user id": `version: 1
channels:
  discord:
    accounts:
      - name: home
        bot_token_ref: env://DISCORD_HOME_TOKEN
        allowed_user_ids:
          - "not-a-snowflake"
`,
		"duplicate user ids": `version: 1
channels:
  discord:
    accounts:
      - name: home
        bot_token_ref: env://DISCORD_HOME_TOKEN
        allowed_user_ids:
          - "123456789012345678"
          - "123456789012345678"
`,
		"unquoted user id": `version: 1
channels:
  discord:
    accounts:
      - name: home
        bot_token_ref: env://DISCORD_HOME_TOKEN
        allowed_user_ids:
          - 123456789012345678
`,
		"accounts not a sequence": `version: 1
channels:
  discord:
    accounts:
      name: home
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
		"non-positive auto-archive": `version: 1
channels:
  discord:
    thread_auto_archive_minutes: 0
`,
		"wrong enabled type": `version: 1
channels:
  discord:
    enabled: "yes"
`,
		"wrong accept_dms type": account("        accept_dms: \"no\"\n"),
		"unknown account key":   account("        instance: owner\n"),
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

const bindingAccounts = `version: 1
channels:
  discord:
    accounts:
      - name: home
        bot_token_ref: env://DISCORD_HOME_TOKEN
        persona: main
      - name: kuma
        bot_token_ref: env://DISCORD_KUMA_TOKEN
`

func TestLoad_BindingsValid(t *testing.T) {
	path := writeDiscordConfig(t, bindingAccounts+`bindings:
  - persona: engineer
    channel: discord
    account: home
    channel_id: "123"
  - persona: reviewer
    channel: discord
    account: home
    channel_id: "123"
    user_id: "456"
  - persona: engineer
    channel: discord
    account: kuma
    channel_id: "123"
`)
	res, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(res.Config.Bindings) != 3 {
		t.Fatalf("bindings = %+v", res.Config.Bindings)
	}
	second := res.Config.Bindings[1]
	if second.Persona != "reviewer" || second.Account != "home" || second.ChannelID != "123" || second.UserID != "456" {
		t.Errorf("bindings[1] = %+v", second)
	}
}

func TestLoad_BindingsInvalid(t *testing.T) {
	cases := map[string]string{
		"unknown account": `bindings:
  - persona: engineer
    channel: discord
    account: work
    channel_id: "123"
`,
		"missing account": `bindings:
  - persona: engineer
    channel: discord
    channel_id: "123"
`,
		"unsupported channel": `bindings:
  - persona: engineer
    channel: telegram
    account: home
    channel_id: "123"
`,
		"no match field": `bindings:
  - persona: engineer
    channel: discord
    account: home
`,
		"empty persona": `bindings:
  - persona: ""
    channel: discord
    account: home
    channel_id: "123"
`,
		"non-numeric id": `bindings:
  - persona: engineer
    channel: discord
    account: home
    guild_id: "abc"
`,
		"duplicate match fields": `bindings:
  - persona: engineer
    channel: discord
    account: home
    channel_id: "123"
  - persona: reviewer
    channel: discord
    account: home
    channel_id: "123"
`,
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			path := writeDiscordConfig(t, bindingAccounts+content)
			_, err := Load(path)
			code, ok := CodeOf(err)
			if !ok || code != ErrorCodeBindingInvalid {
				t.Fatalf("Load error = %v, want %s", err, ErrorCodeBindingInvalid)
			}
		})
	}
}

func TestLoad_BindingsShapeInvalid(t *testing.T) {
	cases := map[string]string{
		"unquoted id": `bindings:
  - persona: engineer
    channel: discord
    account: home
    channel_id: 123
`,
		"not a sequence": `bindings:
  persona: engineer
`,
		"unknown key": `bindings:
  - persona: engineer
    channel: discord
    account: home
    role_id: "123"
`,
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			path := writeDiscordConfig(t, bindingAccounts+content)
			if _, err := Load(path); err == nil {
				t.Fatal("invalid bindings shape accepted")
			}
		})
	}
}
