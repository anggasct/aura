package config

import (
	"strings"
	"testing"
	"time"
)

const emailOwner = "owner:\n  identities:\n    - email:owner@example.com\n"

const emailAccountYAML = `      - name: work
        persona: main
        address: aura@example.com
        authserv_id: mx.example.com
        imap:
          host: imap.example.com
          username: aura@example.com
          password_ref: env://AURA_EMAIL_PASSWORD
`

func emailConfig(owner, channel, account string) string {
	return "version: 1\n" + owner + "channels:\n  email:\n" + channel + "    accounts:\n" + account
}

func TestLoad_EmailDefaults(t *testing.T) {
	res, err := Load(writeDiscordConfig(t, "version: 1\n"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	email := res.Config.Channels.Email
	if email.Enabled || len(email.Accounts) != 0 {
		t.Fatalf("email = %+v, want disabled with no accounts", email)
	}
	if email.MaxAttachmentBytes != 20971520 {
		t.Errorf("max_attachment_bytes = %d, want 20971520", email.MaxAttachmentBytes)
	}
}

func TestLoad_EmailAccountApplied(t *testing.T) {
	res, err := Load(writeDiscordConfig(t, emailConfig(emailOwner, "    enabled: true\n    max_attachment_bytes: 1048576\n", emailAccountYAML+"        allowed_senders:\n          - friend@example.com\n")))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	email := res.Config.Channels.Email
	if !email.Enabled || len(email.Accounts) != 1 || email.MaxAttachmentBytes != 1048576 {
		t.Fatalf("email = %+v", email)
	}
	account := email.Accounts[0]
	if account.Name != "work" || account.Persona != "main" || account.Address != "aura@example.com" || account.AuthservID != "mx.example.com" {
		t.Errorf("account = %+v", account)
	}
	if len(account.AllowedSenders) != 1 || account.AllowedSenders[0] != "friend@example.com" {
		t.Errorf("allowed_senders = %v", account.AllowedSenders)
	}
	imap := account.IMAP
	if imap.Host != "imap.example.com" || imap.Username != "aura@example.com" || imap.PasswordRef != "env://AURA_EMAIL_PASSWORD" {
		t.Errorf("imap = %+v", imap)
	}
	if imap.Port != 993 || imap.Folder != "INBOX" || imap.PollInterval != Duration(time.Minute) {
		t.Errorf("imap defaults = port %d folder %q poll %v, want 993 INBOX 1m", imap.Port, imap.Folder, imap.PollInterval)
	}
}

func TestLoad_EmailIMAPOverrides(t *testing.T) {
	account := emailAccountYAML + "          port: 1993\n          folder: Aura\n          poll_interval: 30s\n"
	res, err := Load(writeDiscordConfig(t, emailConfig(emailOwner, "", account)))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	imap := res.Config.Channels.Email.Accounts[0].IMAP
	if imap.Port != 1993 || imap.Folder != "Aura" || imap.PollInterval != Duration(30*time.Second) {
		t.Errorf("imap = %+v", imap)
	}
}

func TestLoad_EmailMaxAttachmentBytes(t *testing.T) {
	cases := []struct {
		name    string
		value   string
		want    int64
		wantErr string
	}{
		{name: "lower bound", value: "1", want: 1},
		{name: "upper bound", value: "20971520", want: 20971520},
		{name: "above the ceiling", value: "20971521", wantErr: "max_attachment_bytes must be between 1 and 20971520"},
		{name: "zero", value: "0", wantErr: "max_attachment_bytes must be between 1 and 20971520"},
		{name: "negative", value: "-5", wantErr: "max_attachment_bytes must be between 1 and 20971520"},
		{name: "not an integer", value: "\"20MB\"", wantErr: "max_attachment_bytes must be an integer"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res, err := Load(writeDiscordConfig(t, "version: 1\nchannels:\n  email:\n    max_attachment_bytes: "+tc.value+"\n"))
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("Load error = %v, want it to mention %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if got := res.Config.Channels.Email.MaxAttachmentBytes; got != tc.want {
				t.Errorf("max_attachment_bytes = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestLoad_EmailInvalid(t *testing.T) {
	replace := func(old, replacement string) string {
		if !strings.Contains(emailAccountYAML, old) {
			t.Fatalf("fixture does not contain %q", old)
		}
		return emailConfig(emailOwner, "", strings.Replace(emailAccountYAML, old, replacement, 1))
	}
	withIMAP := func(extra string) string {
		return emailConfig(emailOwner, "", emailAccountYAML+extra)
	}
	cases := map[string]struct {
		yaml string
		want string
	}{
		"enabled without accounts": {
			yaml: "version: 1\n" + emailOwner + "channels:\n  email:\n    enabled: true\n",
			want: "requires at least one channels.email.accounts entry",
		},
		"enabled without an owner or allowlist": {
			yaml: emailConfig("", "    enabled: true\n", emailAccountYAML),
			want: "requires an email:<address> entry in owner.identities or an allowed_senders entry on every account",
		},
		"enabled with an owner of another channel": {
			yaml: emailConfig("owner:\n  identities:\n    - discord:123456789012345678\n", "    enabled: true\n", emailAccountYAML),
			want: "requires an email:<address> entry in owner.identities",
		},
		"inline password": {
			yaml: replace("env://AURA_EMAIL_PASSWORD", "hunter2"),
			want: "password_ref must use env:// or file://",
		},
		"plaintext url password": {
			yaml: replace("env://AURA_EMAIL_PASSWORD", "https://user:hunter2@example.com"),
			want: "password_ref must use env:// or file://",
		},
		"empty reference target": {
			yaml: replace("env://AURA_EMAIL_PASSWORD", "env://"),
			want: "password_ref must use env:// or file://",
		},
		"missing password": {
			yaml: replace("          password_ref: env://AURA_EMAIL_PASSWORD\n", ""),
			want: "password_ref must not be empty",
		},
		"missing persona": {
			yaml: replace("        persona: main\n", ""),
			want: "persona must not be empty",
		},
		"bad account name": {
			yaml: replace("name: work", "name: Work/1"),
			want: "name \"Work/1\" must match",
		},
		"duplicate account name": {
			yaml: withIMAP(strings.Replace(emailAccountYAML, "persona: main", "persona: other", 1)),
			want: "is configured more than once",
		},
		"uppercase address": {
			yaml: replace("address: aura@example.com", "address: Aura@example.com"),
			want: "address must be a lowercase mailbox address",
		},
		"address without domain": {
			yaml: replace("address: aura@example.com", "address: aura"),
			want: "address must be a lowercase mailbox address",
		},
		"missing authserv id": {
			yaml: replace("        authserv_id: mx.example.com\n", ""),
			want: "authserv_id must be the receiving server's host name",
		},
		"authserv id with a space": {
			yaml: replace("authserv_id: mx.example.com", "authserv_id: \"mx.example.com (trusted)\""),
			want: "authserv_id must be the receiving server's host name",
		},
		"host with a scheme": {
			yaml: replace("host: imap.example.com", "host: imaps://imap.example.com"),
			want: "imap.host must be a host name",
		},
		"host with a port": {
			yaml: replace("host: imap.example.com", "host: imap.example.com:143"),
			want: "imap.host must be a host name",
		},
		"missing host": {
			yaml: replace("          host: imap.example.com\n", ""),
			want: "imap.host must be a host name",
		},
		"missing username": {
			yaml: replace("          username: aura@example.com\n", ""),
			want: "imap.username must be a single line",
		},
		"port below range": {
			yaml: withIMAP("          port: 0\n"),
			want: "imap.port must be between 1 and 65535",
		},
		"port above range": {
			yaml: withIMAP("          port: 65536\n"),
			want: "imap.port must be between 1 and 65535",
		},
		"port as a string": {
			yaml: withIMAP("          port: \"993\"\n"),
			want: "imap.port must be an integer",
		},
		"empty folder": {
			yaml: withIMAP("          folder: \"\"\n"),
			want: "imap.folder must be a single line",
		},
		"folder with a newline": {
			yaml: withIMAP("          folder: \"INBOX\\nA1 LOGOUT\"\n"),
			want: "imap.folder must be a single line",
		},
		"poll interval below the floor": {
			yaml: withIMAP("          poll_interval: 5s\n"),
			want: "imap.poll_interval must be between 10s and 1h0m0s",
		},
		"poll interval above the ceiling": {
			yaml: withIMAP("          poll_interval: 2h\n"),
			want: "imap.poll_interval must be between 10s and 1h0m0s",
		},
		"zero poll interval": {
			yaml: withIMAP("          poll_interval: 0s\n"),
			want: "imap.poll_interval must be between 10s and 1h0m0s",
		},
		"numeric poll interval": {
			yaml: withIMAP("          poll_interval: 60\n"),
			want: "imap.poll_interval must be a duration string",
		},
		"uppercase allowed sender": {
			yaml: withIMAP("        allowed_senders:\n          - Friend@example.com\n"),
			want: "allowed_senders entries must be lowercase mailbox addresses",
		},
		"duplicate allowed sender": {
			yaml: withIMAP("        allowed_senders:\n          - friend@example.com\n          - friend@example.com\n"),
			want: "allowed_senders lists an address more than once",
		},
		"unquoted allowed sender": {
			yaml: withIMAP("        allowed_senders:\n          - 7\n"),
			want: "entries must be quoted strings",
		},
		"allowed senders not a sequence": {
			yaml: withIMAP("        allowed_senders: friend@example.com\n"),
			want: "allowed_senders must be a sequence",
		},
		"smtp is not accepted yet": {
			yaml: withIMAP("        smtp:\n          host: smtp.example.com\n"),
			want: "unknown key",
		},
		"tls switch is not accepted": {
			yaml: withIMAP("          tls: false\n"),
			want: "unknown key",
		},
		"verification switch is not accepted": {
			yaml: withIMAP("          insecure_skip_verify: true\n"),
			want: "unknown key",
		},
		"unknown account key": {
			yaml: withIMAP("        transport: plain\n"),
			want: "unknown key",
		},
		"enabled not a boolean": {
			yaml: emailConfig(emailOwner, "    enabled: \"yes\"\n", emailAccountYAML),
			want: "channels.email.enabled must be a boolean",
		},
		"account name not a string": {
			yaml: replace("name: work", "name: 7"),
			want: "name must be a string",
		},
		"imap not a mapping": {
			yaml: withIMAP("        imap: plain\n"),
			want: "imap must be a mapping",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := Load(writeDiscordConfig(t, tc.yaml))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Load error = %v, want it to mention %q", err, tc.want)
			}
			if strings.Contains(err.Error(), "hunter2") {
				t.Errorf("error %q leaks the inline password", err)
			}
		})
	}
}

func TestLoad_EmailInvalidCodes(t *testing.T) {
	_, err := Load(writeDiscordConfig(t, emailConfig(emailOwner, "", strings.Replace(emailAccountYAML, "env://AURA_EMAIL_PASSWORD", "hunter2", 1))))
	if code, ok := CodeOf(err); !ok || code != ErrorCodeConfigInvalid {
		t.Fatalf("code = %q, %v; want %q", code, ok, ErrorCodeConfigInvalid)
	}
}

func TestLoad_EmailBindingsAreNotSupported(t *testing.T) {
	yaml := emailConfig(emailOwner, "", emailAccountYAML) + "bindings:\n  - persona: main\n    channel: email\n    account: work\n    user_id: \"7\"\n"
	_, err := Load(writeDiscordConfig(t, yaml))
	if err == nil || !strings.Contains(err.Error(), "channel \"email\" is not supported") {
		t.Fatalf("Load error = %v, want the email binding rejected", err)
	}
}

func TestLoad_EmailOwnerIdentityRejectsInvalidAddress(t *testing.T) {
	_, err := Load(writeDiscordConfig(t, "version: 1\nowner:\n  identities:\n    - email:Owner@example.com\n"))
	if err == nil {
		t.Fatal("Load accepted an uppercase email owner identity")
	}
}
