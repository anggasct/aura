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
	if telegram.MaxAttachmentBytes != 20971520 {
		t.Errorf("max_attachment_bytes = %d, want 20971520", telegram.MaxAttachmentBytes)
	}
}

func TestLoad_TelegramMaxAttachmentBytes(t *testing.T) {
	cases := []struct {
		name    string
		value   string
		want    int64
		wantErr string
		invalid bool
	}{
		{name: "lower bound", value: "1", want: 1},
		{name: "typical", value: "1048576", want: 1048576},
		{name: "upper bound", value: "20971520", want: 20971520},
		{name: "above the bot api limit", value: "20971521", wantErr: "max_attachment_bytes must be between 1 and 20971520", invalid: true},
		{name: "zero", value: "0", wantErr: "max_attachment_bytes must be between 1 and 20971520", invalid: true},
		{name: "negative", value: "-5", wantErr: "max_attachment_bytes must be between 1 and 20971520", invalid: true},
		{name: "not an integer", value: "\"20MB\"", wantErr: "max_attachment_bytes must be an integer"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res, err := Load(writeDiscordConfig(t, "version: 1\nchannels:\n  telegram:\n    max_attachment_bytes: "+tc.value+"\n"))
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("Load error = %v, want it to mention %q", err, tc.wantErr)
				}
				if code, ok := CodeOf(err); tc.invalid && (!ok || code != ErrorCodeConfigInvalid) {
					t.Errorf("error code = %q, want %q", code, ErrorCodeConfigInvalid)
				}
				return
			}
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if got := res.Config.Channels.Telegram.MaxAttachmentBytes; got != tc.want {
				t.Errorf("max_attachment_bytes = %d, want %d", got, tc.want)
			}
		})
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
		"unquoted allowed chat id": {
			yaml: account("        allowed_chat_ids:\n          - -100\n"),
			want: "entries must be quoted strings",
		},
		"allowed chat ids not a sequence": {
			yaml: account("        allowed_chat_ids: \"-100\"\n"),
			want: "allowed_chat_ids must be a sequence",
		},
		"positive allowed chat id": {
			yaml: account("        allowed_chat_ids:\n          - \"42\"\n"),
			want: "is not a negative decimal telegram group chat id",
		},
		"zero allowed chat id": {
			yaml: account("        allowed_chat_ids:\n          - \"0\"\n"),
			want: "is not a negative decimal telegram group chat id",
		},
		"non numeric allowed chat id": {
			yaml: account("        allowed_chat_ids:\n          - \"@group\"\n"),
			want: "is not a negative decimal telegram group chat id",
		},
		"empty allowed chat id": {
			yaml: account("        allowed_chat_ids:\n          - \"\"\n"),
			want: "must not contain empty ids",
		},
		"duplicate allowed chat id": {
			yaml: account("        allowed_chat_ids:\n          - \"-100\"\n          - \"-100\"\n"),
			want: "is configured more than once",
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
			yaml: account("        transport: webhook\n"),
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
		"unknown account":     {yaml: base + "    account: other\n    user_id: \"42\"\n", want: "not a configured channels.telegram.accounts name"},
		"discord account":     {yaml: "version: 1\nchannels:\n  discord:\n    accounts:\n      - name: home\n        bot_token_ref: env://D\nbindings:\n  - persona: main\n    channel: telegram\n    account: home\n    user_id: \"42\"\n", want: "not a configured channels.telegram.accounts name"},
		"missing match":       {yaml: base + "    account: pocket\n", want: "must set chat_id or user_id"},
		"negative user":       {yaml: base + "    account: pocket\n    user_id: \"-42\"\n", want: "not a positive decimal telegram user id"},
		"discord-only field":  {yaml: base + "    account: pocket\n    user_id: \"42\"\n    guild_id: \"1\"\n", want: "by chat_id, thread_id and user_id only"},
		"channel id field":    {yaml: base + "    account: pocket\n    chat_id: \"-100\"\n    channel_id: \"1\"\n", want: "by chat_id, thread_id and user_id only"},
		"thread without chat": {yaml: base + "    account: pocket\n    user_id: \"42\"\n    thread_id: \"5\"\n", want: "thread_id requires chat_id"},
		"positive chat":       {yaml: base + "    account: pocket\n    chat_id: \"42\"\n", want: "not a negative decimal telegram group chat id"},
		"non numeric chat":    {yaml: base + "    account: pocket\n    chat_id: \"-abc\"\n", want: "not a negative decimal telegram group chat id"},
		"zero chat":           {yaml: base + "    account: pocket\n    chat_id: \"0\"\n", want: "not a negative decimal telegram group chat id"},
		"padded chat":         {yaml: base + "    account: pocket\n    chat_id: \"-0100\"\n", want: "not a negative decimal telegram group chat id"},
		"non numeric thread":  {yaml: base + "    account: pocket\n    chat_id: \"-100\"\n    thread_id: \"general\"\n", want: "not a positive decimal telegram topic id"},
		"negative thread":     {yaml: base + "    account: pocket\n    chat_id: \"-100\"\n    thread_id: \"-5\"\n", want: "not a positive decimal telegram topic id"},
		"duplicate chat":      {yaml: base + "    account: pocket\n    chat_id: \"-100\"\n  - persona: main\n    channel: telegram\n    account: pocket\n    chat_id: \"-100\"\n", want: "same match fields"},
		"unknown channel":     {yaml: "version: 1\nbindings:\n  - persona: main\n    channel: slack\n    account: x\n    user_id: \"42\"\n", want: "is not supported"},
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

func TestLoad_TelegramGroupKeys(t *testing.T) {
	res, err := Load(writeDiscordConfig(t, `version: 1
`+telegramOwner+`channels:
  telegram:
    enabled: true
    accounts:
      - name: pocket
        bot_token_ref: env://AURA_TELEGRAM_BOT_TOKEN
        allowed_chat_ids:
          - "-1001234567890"
          - "-42"
      - name: spare
        bot_token_ref: env://AURA_TELEGRAM_SPARE
bindings:
  - persona: main
    channel: telegram
    account: pocket
    chat_id: "-1001234567890"
  - persona: main
    channel: telegram
    account: pocket
    chat_id: "-1001234567890"
    thread_id: "5"
  - persona: main
    channel: telegram
    account: pocket
    chat_id: "-1001234567890"
    user_id: "7"
  - persona: main
    channel: telegram
    account: pocket
    user_id: "7"
`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	accounts := res.Config.Channels.Telegram.Accounts
	if got := accounts[0].AllowedChatIDs; len(got) != 2 || got[0] != "-1001234567890" || got[1] != "-42" {
		t.Errorf("allowed chat ids = %v", got)
	}
	if got := accounts[1].AllowedChatIDs; len(got) != 0 {
		t.Errorf("account without the key serves groups: %v", got)
	}
	if len(res.Config.Bindings) != 4 {
		t.Errorf("bindings = %d, want 4", len(res.Config.Bindings))
	}
}

func TestLoad_ChatIDBindingIsTelegramOnly(t *testing.T) {
	path := writeDiscordConfig(t, bindingAccounts+`bindings:
  - persona: engineer
    channel: discord
    account: home
    channel_id: "123"
    chat_id: "-100"
`)
	_, err := Load(path)
	if code, ok := CodeOf(err); !ok || code != ErrorCodeBindingInvalid || !strings.Contains(err.Error(), "chat_id is only valid on telegram bindings") {
		t.Fatalf("Load error = %v, want %s naming chat_id", err, ErrorCodeBindingInvalid)
	}
}

func TestLoad_TelegramChatBindingMustBeQuoted(t *testing.T) {
	_, err := Load(writeDiscordConfig(t, "version: 1\nchannels:\n  telegram:\n    accounts:\n      - name: pocket\n        bot_token_ref: env://AURA_TELEGRAM_BOT_TOKEN\nbindings:\n  - persona: main\n    channel: telegram\n    account: pocket\n    chat_id: -100\n"))
	if err == nil || !strings.Contains(err.Error(), "bindings[0].chat_id must be a quoted string") {
		t.Fatalf("Load error = %v, want a quoted-string shape error", err)
	}
}

func TestLoad_TelegramWebhookDefaults(t *testing.T) {
	res, err := Load(writeDiscordConfig(t, "version: 1\n"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	webhook := res.Config.Channels.Telegram.Webhook
	want := TelegramWebhook{ListenAddress: "127.0.0.1:8283", MaxBodyBytes: 1048576, MaxConnections: 4}
	if webhook != want {
		t.Errorf("webhook = %+v, want %+v", webhook, want)
	}
}

func TestLoad_TelegramAccountModeDefaultsToPolling(t *testing.T) {
	res, err := Load(writeDiscordConfig(t, "version: 1\n"+telegramOwner+`channels:
  telegram:
    enabled: true
    accounts:
      - name: pocket
        bot_token_ref: env://AURA_TELEGRAM_BOT_TOKEN
`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	account := res.Config.Channels.Telegram.Accounts[0]
	if account.Mode != TelegramModePolling || account.WebhookMode() || res.Config.Channels.Telegram.UsesWebhook() {
		t.Errorf("account = %+v, want polling", account)
	}
}

func TestLoad_TelegramWebhookValid(t *testing.T) {
	res, err := Load(writeDiscordConfig(t, "version: 1\n"+telegramOwner+`channels:
  telegram:
    enabled: true
    accounts:
      - name: pocket
        bot_token_ref: env://AURA_TELEGRAM_BOT_TOKEN
        mode: webhook
        webhook_secret_ref: file:///run/secrets/telegram-hook
      - name: spare
        bot_token_ref: env://AURA_TELEGRAM_SPARE
    webhook:
      listen_address: 0.0.0.0:9000
      public_base_url: https://aura.example.com/edge
      max_body_bytes: 2048
      max_connections: 10
`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	telegram := res.Config.Channels.Telegram
	if !telegram.UsesWebhook() || !telegram.Accounts[0].WebhookMode() || telegram.Accounts[1].WebhookMode() {
		t.Fatalf("accounts = %+v, want only pocket in webhook mode", telegram.Accounts)
	}
	if telegram.Accounts[0].WebhookSecretRef != "file:///run/secrets/telegram-hook" {
		t.Errorf("webhook_secret_ref = %q", telegram.Accounts[0].WebhookSecretRef)
	}
	want := TelegramWebhook{ListenAddress: "0.0.0.0:9000", PublicBaseURL: "https://aura.example.com/edge", MaxBodyBytes: 2048, MaxConnections: 10}
	if telegram.Webhook != want {
		t.Errorf("webhook = %+v, want %+v", telegram.Webhook, want)
	}
}

func TestLoad_TelegramWebhookInvalid(t *testing.T) {
	hook := func(accountExtra, webhookBlock string) string {
		yaml := "version: 1\n" + telegramOwner + "channels:\n  telegram:\n    accounts:\n      - name: pocket\n        bot_token_ref: env://AURA_TELEGRAM_BOT_TOKEN\n" + accountExtra
		if webhookBlock != "" {
			yaml += "    webhook:\n" + webhookBlock
		}
		return yaml
	}
	const webhookAccount = "        mode: webhook\n        webhook_secret_ref: env://AURA_TELEGRAM_WEBHOOK_SECRET\n"
	const publicURL = "      public_base_url: https://aura.example.com\n"
	cases := map[string]struct {
		yaml  string
		want  string
		shape bool
	}{
		"unknown mode":                     {yaml: hook("        mode: carrier\n", ""), want: `mode "carrier" must be polling or webhook`},
		"empty mode":                       {yaml: hook("        mode: \"\"\n", ""), want: "mode \"\" must be polling or webhook"},
		"non string mode":                  {yaml: hook("        mode: 3\n", ""), want: "mode must be a string", shape: true},
		"webhook without secret ref":       {yaml: hook("        mode: webhook\n", publicURL), want: "webhook_secret_ref must not be empty in webhook mode"},
		"inline secret":                    {yaml: hook("        mode: webhook\n        webhook_secret_ref: plain\n", publicURL), want: "webhook_secret_ref must use env:// or file://"},
		"secret ref equals own token":      {yaml: hook("        mode: webhook\n        webhook_secret_ref: env://AURA_TELEGRAM_BOT_TOKEN\n", publicURL), want: "must differ from every bot token reference"},
		"secret ref equals other token":    {yaml: hook(webhookAccount+"      - name: spare\n        bot_token_ref: env://AURA_TELEGRAM_WEBHOOK_SECRET\n", publicURL), want: "must differ from every bot token reference"},
		"secret ref equals discord token":  {yaml: "version: 1\nchannels:\n  discord:\n    accounts:\n      - name: home\n        bot_token_ref: env://AURA_TELEGRAM_WEBHOOK_SECRET\n  telegram:\n    accounts:\n      - name: pocket\n        bot_token_ref: env://AURA_TELEGRAM_BOT_TOKEN\n" + webhookAccount + "    webhook:\n" + publicURL, want: "must differ from every bot token reference"},
		"secret ref on a polling account":  {yaml: hook("        webhook_secret_ref: env://AURA_TELEGRAM_WEBHOOK_SECRET\n", ""), want: "webhook_secret_ref is only valid when mode is webhook"},
		"missing public base url":          {yaml: hook(webhookAccount, ""), want: "public_base_url is required when an account uses webhook mode"},
		"http public base url":             {yaml: hook(webhookAccount, "      public_base_url: http://aura.example.com\n"), want: "public_base_url must use https"},
		"loopback http public base url":    {yaml: hook(webhookAccount, "      public_base_url: http://127.0.0.1:8283\n"), want: "public_base_url must use https"},
		"public base url with query":       {yaml: hook(webhookAccount, "      public_base_url: https://aura.example.com/?a=b\n"), want: "must not carry a query or fragment"},
		"public base url with empty query": {yaml: hook(webhookAccount, "      public_base_url: https://aura.example.com/?\n"), want: "must not carry a query or fragment"},
		"public base url with fragment":    {yaml: hook(webhookAccount, "      public_base_url: https://aura.example.com/#top\n"), want: "must not carry a query or fragment"},
		"public base url with userinfo":    {yaml: hook(webhookAccount, "      public_base_url: https://user:pass@aura.example.com\n"), want: "user info is not allowed"},
		"public base url without host":     {yaml: hook(webhookAccount, "      public_base_url: https://\n"), want: "missing host"},
		"public base url on polling only":  {yaml: hook("", "      public_base_url: http://aura.example.com\n"), want: "public_base_url must use https"},
		"listen address without port":      {yaml: hook("", "      listen_address: 127.0.0.1\n"), want: "listen_address must be host:port"},
		"listen address port zero":         {yaml: hook("", "      listen_address: 127.0.0.1:0\n"), want: "listen_address port must be between 1 and 65535"},
		"listen address port too large":    {yaml: hook("", "      listen_address: 127.0.0.1:70000\n"), want: "listen_address port must be between 1 and 65535"},
		"listen address without host":      {yaml: hook("", "      listen_address: \":8283\"\n"), want: "listen_address must include a host"},
		"body bound zero":                  {yaml: hook("", "      max_body_bytes: 0\n"), want: "max_body_bytes must be between 1 and 10485760"},
		"body bound negative":              {yaml: hook("", "      max_body_bytes: -1\n"), want: "max_body_bytes must be between 1 and 10485760"},
		"body bound above the ceiling":     {yaml: hook("", "      max_body_bytes: 10485761\n"), want: "max_body_bytes must be between 1 and 10485760"},
		"connections zero":                 {yaml: hook("", "      max_connections: 0\n"), want: "max_connections must be between 1 and 100"},
		"connections above the ceiling":    {yaml: hook("", "      max_connections: 101\n"), want: "max_connections must be between 1 and 100"},
		"connections not an integer":       {yaml: hook("", "      max_connections: many\n"), want: "max_connections must be an integer", shape: true},
		"body bound not an integer":        {yaml: hook("", "      max_body_bytes: \"1MB\"\n"), want: "max_body_bytes must be an integer", shape: true},
		"listen address not a string":      {yaml: hook("", "      listen_address: 8283\n"), want: "listen_address must be a string", shape: true},
		"webhook block not a mapping":      {yaml: "version: 1\nchannels:\n  telegram:\n    webhook: yes\n", want: "channels.telegram.webhook must be a mapping", shape: true},
		"unknown webhook key":              {yaml: hook("", "      route: /x\n"), want: "unknown key", shape: true},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := Load(writeDiscordConfig(t, tc.yaml))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Load error = %v, want it to mention %q", err, tc.want)
			}
			if code, ok := CodeOf(err); !tc.shape && (!ok || code != ErrorCodeConfigInvalid) {
				t.Errorf("error code = %q, want %q", code, ErrorCodeConfigInvalid)
			}
		})
	}
}

func TestMarshal_TelegramWebhookSecretStaysAReference(t *testing.T) {
	const secretValue = "hook-secret-probe-81c4"
	t.Setenv("AURA_TELEGRAM_WEBHOOK_SECRET", secretValue)
	res, err := Load(writeDiscordConfig(t, "version: 1\n"+telegramOwner+`channels:
  telegram:
    enabled: true
    accounts:
      - name: pocket
        bot_token_ref: env://AURA_TELEGRAM_BOT_TOKEN
        mode: webhook
        webhook_secret_ref: env://AURA_TELEGRAM_WEBHOOK_SECRET
    webhook:
      public_base_url: https://aura.example.com
`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	dump, err := Marshal(res.Config)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if strings.Contains(string(dump), secretValue) {
		t.Errorf("config dump carries the resolved webhook secret")
	}
	if !strings.Contains(string(dump), "webhook_secret_ref: env://AURA_TELEGRAM_WEBHOOK_SECRET") {
		t.Errorf("config dump does not carry the secret reference:\n%s", dump)
	}
}
