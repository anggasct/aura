package config

import (
	"strings"
	"testing"
	"time"
)

const telegramOwner = "owner:\n  identities:\n    - telegram:424242\n"

func TestLoad_TelegramDefaults(t *testing.T) {
	res, err := Load(writeDiscordConfig(t, "version: 1\n"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	telegram := res.Config.Channels.Telegram
	if telegram.Enabled || len(telegram.Accounts) != 0 {
		t.Fatalf("telegram = %+v, want disabled with no accounts", telegram)
	}
	if telegram.Polling.Timeout != Duration(30*time.Second) || telegram.Polling.MaxBackoff != Duration(time.Minute) {
		t.Errorf("polling = %+v, want 30s timeout and 1m backoff", telegram.Polling)
	}
	if telegram.MinEditInterval != Duration(2*time.Second) {
		t.Errorf("min_edit_interval = %v, want 2s", telegram.MinEditInterval)
	}
}

func TestLoad_TelegramMinEditIntervalOverride(t *testing.T) {
	res, err := Load(writeDiscordConfig(t, "version: 1\nchannels:\n  telegram:\n    min_edit_interval: 5s\n"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := res.Config.Channels.Telegram.MinEditInterval; got != Duration(5*time.Second) {
		t.Errorf("min_edit_interval = %v, want 5s", got)
	}
}

func TestLoad_TelegramAccountsValid(t *testing.T) {
	res, err := Load(writeDiscordConfig(t, `version: 1
`+telegramOwner+`channels:
  telegram:
    enabled: true
    accounts:
      - name: pocket
        bot_token_ref: env://AURA_TELEGRAM_BOT_TOKEN
        persona: main
    polling:
      timeout: 20s
bindings:
  - persona: main
    channel: telegram
    account: pocket
    user_id: "424242"
`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	telegram := res.Config.Channels.Telegram
	if !telegram.Enabled || len(telegram.Accounts) != 1 {
		t.Fatalf("telegram = %+v", telegram)
	}
	if account := telegram.Accounts[0]; account.Name != "pocket" || account.Persona != "main" || account.BotTokenRef != "env://AURA_TELEGRAM_BOT_TOKEN" {
		t.Errorf("account = %+v", account)
	}
	if telegram.Polling.Timeout != Duration(20*time.Second) {
		t.Errorf("timeout = %v, want 20s", telegram.Polling.Timeout)
	}
	if telegram.Polling.MaxBackoff != Duration(time.Minute) {
		t.Errorf("max_backoff default not applied: %v", telegram.Polling.MaxBackoff)
	}
	if len(res.Config.Bindings) != 1 || res.Config.Bindings[0].Channel != BindingChannelTelegram {
		t.Errorf("bindings = %+v", res.Config.Bindings)
	}
}

func TestLoad_TelegramInvalid(t *testing.T) {
	account := func(extra string) string {
		return "version: 1\n" + telegramOwner + "channels:\n  telegram:\n    enabled: true\n    accounts:\n      - name: pocket\n        bot_token_ref: env://AURA_TELEGRAM_BOT_TOKEN\n" + extra
	}
	cases := map[string]struct {
		yaml string
		want string
	}{
		"enabled without accounts": {
			yaml: "version: 1\n" + telegramOwner + "channels:\n  telegram:\n    enabled: true\n",
			want: "requires at least one channels.telegram.accounts entry",
		},
		"enabled without a telegram owner": {
			yaml: "version: 1\nowner:\n  identities:\n    - discord:123456789012345678\nchannels:\n  telegram:\n    enabled: true\n    accounts:\n      - name: pocket\n        bot_token_ref: env://AURA_TELEGRAM_BOT_TOKEN\n",
			want: "requires a telegram:<user_id> entry in owner.identities",
		},
		"inline token": {
			yaml: "version: 1\nchannels:\n  telegram:\n    accounts:\n      - name: pocket\n        bot_token_ref: \"123456:ABC\"\n",
			want: "bot_token_ref must use env:// or file://",
		},
		"missing token ref": {
			yaml: "version: 1\nchannels:\n  telegram:\n    accounts:\n      - name: pocket\n",
			want: "bot_token_ref must not be empty",
		},
		"bad account name": {
			yaml: "version: 1\nchannels:\n  telegram:\n    accounts:\n      - name: Pocket/1\n        bot_token_ref: env://AURA_TELEGRAM_BOT_TOKEN\n",
			want: "name \"Pocket/1\" must match",
		},
		"duplicate account names": {
			yaml: account("      - name: pocket\n        bot_token_ref: env://AURA_TELEGRAM_OTHER\n"),
			want: "is configured more than once",
		},
		"shared token ref": {
			yaml: account("      - name: spare\n        bot_token_ref: env://AURA_TELEGRAM_BOT_TOKEN\n"),
			want: "is shared with another account",
		},
		"token ref shared with discord": {
			yaml: "version: 1\nchannels:\n  discord:\n    accounts:\n      - name: home\n        bot_token_ref: env://SHARED\n  telegram:\n    accounts:\n      - name: pocket\n        bot_token_ref: env://SHARED\n",
			want: "is shared with another account",
		},
		"fractional poll timeout": {
			yaml: account("    polling:\n      timeout: 1500ms\n"),
			want: "polling.timeout must be a whole number of seconds",
		},
		"poll timeout above the bound": {
			yaml: account("    polling:\n      timeout: 2m\n"),
			want: "polling.timeout must be a whole number of seconds",
		},
		"backoff below a second": {
			yaml: account("    polling:\n      max_backoff: 10ms\n"),
			want: "polling.max_backoff must be between",
		},
		"numeric poll timeout": {
			yaml: account("    polling:\n      timeout: 30\n"),
			want: "must be a duration string",
		},
		"zero edit interval": {
			yaml: account("    min_edit_interval: 0s\n"),
			want: "min_edit_interval must be between 1s and 1m0s",
		},
		"edit interval below a second": {
			yaml: account("    min_edit_interval: 500ms\n"),
			want: "min_edit_interval must be between 1s and 1m0s",
		},
		"edit interval above a minute": {
			yaml: account("    min_edit_interval: 2m\n"),
			want: "min_edit_interval must be between 1s and 1m0s",
		},
		"numeric edit interval": {
			yaml: account("    min_edit_interval: 2\n"),
			want: "min_edit_interval must be a duration string",
		},
		"enabled without owner or allowlist": {
			yaml: "version: 1\nchannels:\n  telegram:\n    enabled: true\n    accounts:\n      - name: pocket\n        bot_token_ref: env://AURA_TELEGRAM_BOT_TOKEN\n",
			want: "or an allowed_user_ids entry on every account",
		},
		"allowlist missing on one of two accounts": {
			yaml: "version: 1\nchannels:\n  telegram:\n    enabled: true\n    accounts:\n      - name: pocket\n        bot_token_ref: env://AURA_TELEGRAM_BOT_TOKEN\n        allowed_user_ids:\n          - \"7\"\n      - name: spare\n        bot_token_ref: env://AURA_TELEGRAM_SPARE\n",
			want: "or an allowed_user_ids entry on every account",
		},
		"unquoted allowed user id": {
			yaml: account("        allowed_user_ids:\n          - 7\n"),
			want: "entries must be quoted strings",
		},
		"allowed user ids not a sequence": {
			yaml: account("        allowed_user_ids: \"7\"\n"),
			want: "allowed_user_ids must be a sequence",
		},
		"non numeric allowed user id": {
			yaml: account("        allowed_user_ids:\n          - \"alice\"\n"),
			want: "is not a positive decimal telegram user id",
		},
		"zero allowed user id": {
			yaml: account("        allowed_user_ids:\n          - \"0\"\n"),
			want: "is not a positive decimal telegram user id",
		},
		"negative allowed user id": {
			yaml: account("        allowed_user_ids:\n          - \"-7\"\n"),
			want: "is not a positive decimal telegram user id",
		},
		"empty allowed user id": {
			yaml: account("        allowed_user_ids:\n          - \"\"\n"),
			want: "must not contain empty ids",
		},
		"duplicate allowed user id": {
			yaml: account("        allowed_user_ids:\n          - \"7\"\n          - \"7\"\n"),
			want: "is configured more than once",
		},
		"accept private chats not a boolean": {
			yaml: account("        accept_private_chats: \"yes\"\n"),
			want: "accept_private_chats must be a boolean",
		},
		"unknown account key": {
			yaml: account("        mode: polling\n"),
			want: "unknown key",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := Load(writeDiscordConfig(t, tc.yaml))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Load error = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}

func TestLoad_TelegramBindingsInvalid(t *testing.T) {
	base := "version: 1\nchannels:\n  telegram:\n    accounts:\n      - name: pocket\n        bot_token_ref: env://AURA_TELEGRAM_BOT_TOKEN\nbindings:\n  - persona: main\n    channel: telegram\n"
	cases := map[string]struct {
		yaml string
		want string
	}{
		"unknown account":    {yaml: base + "    account: other\n    user_id: \"42\"\n", want: "not a configured channels.telegram.accounts name"},
		"discord account":    {yaml: "version: 1\nchannels:\n  discord:\n    accounts:\n      - name: home\n        bot_token_ref: env://D\nbindings:\n  - persona: main\n    channel: telegram\n    account: home\n    user_id: \"42\"\n", want: "not a configured channels.telegram.accounts name"},
		"missing user":       {yaml: base + "    account: pocket\n", want: "must set user_id"},
		"negative user":      {yaml: base + "    account: pocket\n    user_id: \"-42\"\n", want: "not a positive decimal telegram user id"},
		"discord-only field": {yaml: base + "    account: pocket\n    user_id: \"42\"\n    guild_id: \"1\"\n", want: "by user_id only"},
		"unknown channel":    {yaml: "version: 1\nbindings:\n  - persona: main\n    channel: slack\n    account: x\n    user_id: \"42\"\n", want: "is not supported"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := Load(writeDiscordConfig(t, tc.yaml))
			if code, ok := CodeOf(err); !ok || code != ErrorCodeBindingInvalid && code != ErrorCodeConfigInvalid {
				t.Fatalf("Load error = %v, want a binding or config error", err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Load error = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}

func TestLoad_TelegramAccessKeys(t *testing.T) {
	res, err := Load(writeDiscordConfig(t, `version: 1
`+telegramOwner+`channels:
  telegram:
    enabled: true
    accounts:
      - name: pocket
        bot_token_ref: env://AURA_TELEGRAM_BOT_TOKEN
        allowed_user_ids:
          - "7"
          - "8"
        accept_private_chats: false
      - name: spare
        bot_token_ref: env://AURA_TELEGRAM_SPARE
`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	accounts := res.Config.Channels.Telegram.Accounts
	if got := accounts[0]; got.AcceptPrivateChats || len(got.AllowedUserIDs) != 2 || got.AllowedUserIDs[0] != "7" || got.AllowedUserIDs[1] != "8" {
		t.Errorf("explicit account = %+v, want two allowed ids and private chats off", got)
	}
	if got := accounts[1]; !got.AcceptPrivateChats || len(got.AllowedUserIDs) != 0 {
		t.Errorf("default account = %+v, want private chats on and no allowlist", got)
	}
}

func TestLoad_TelegramAllowlistReplacesOwnerRequirement(t *testing.T) {
	res, err := Load(writeDiscordConfig(t, `version: 1
channels:
  telegram:
    enabled: true
    accounts:
      - name: pocket
        bot_token_ref: env://AURA_TELEGRAM_BOT_TOKEN
        allowed_user_ids:
          - "7"
`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !res.Config.Channels.Telegram.Enabled {
		t.Fatal("telegram with an allowlist and no owner identity did not load as enabled")
	}
}

func TestLoad_TelegramOwnerOnlyAccountKeepsPrivateChatsOn(t *testing.T) {
	res, err := Load(writeDiscordConfig(t, "version: 1\n"+telegramOwner+"channels:\n  telegram:\n    enabled: true\n    accounts:\n      - name: pocket\n        bot_token_ref: env://AURA_TELEGRAM_BOT_TOKEN\n"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	account := res.Config.Channels.Telegram.Accounts[0]
	if !account.AcceptPrivateChats || len(account.AllowedUserIDs) != 0 {
		t.Errorf("account = %+v, want the step 2 shape: private chats on, no allowlist", account)
	}
}
