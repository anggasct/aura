package plugin

import (
	"bytes"
	"strings"
	"testing"

	"github.com/anggasct/aura/internal/platform/command"
	"github.com/anggasct/aura/internal/platform/fault"
)

func TestPluginCommandsRefusesNilOpener(t *testing.T) {
	cases := [][]string{
		{"list"},
		{"install", "/tmp/demo"},
		{"review", "demo"},
		{"approve", "demo", "--digest", "0123456789ab"},
		{"disable", "demo"},
		{"remove", "demo"},
		{"login", "demo"},
		{"logout", "demo"},
	}
	for _, args := range cases {
		t.Run(args[0], func(t *testing.T) {
			cmds := Commands(nil)
			if len(cmds) != 1 {
				t.Fatalf("Commands(nil) returned %d commands, want 1", len(cmds))
			}
			root := cmds[0]
			root.SilenceUsage = true
			root.SilenceErrors = true
			root.SetOut(&bytes.Buffer{})
			root.SetErr(&bytes.Buffer{})
			root.SetArgs(args)
			err := root.ExecuteContext(t.Context())
			if !fault.HasCode(err, ErrInvalidArgument) {
				t.Fatalf("Execute(%v) = %v, want %s", args, err, ErrInvalidArgument)
			}
		})
	}
}

func TestPluginCommandsUsageErrorsReturnUsageError(t *testing.T) {
	cases := [][]string{
		{"list", "extra"},
		{"install"},
		{"review"},
		{"approve"},
		{"disable"},
		{"remove"},
		{"login"},
		{"logout"},
	}
	for _, args := range cases {
		t.Run(strings.Join(args, "_"), func(t *testing.T) {
			root := Commands(nil)[0]
			root.SilenceUsage = true
			root.SilenceErrors = true
			root.SetOut(&bytes.Buffer{})
			root.SetErr(&bytes.Buffer{})
			root.SetArgs(args)
			err := root.ExecuteContext(t.Context())
			if !command.IsUsageError(err) {
				t.Fatalf("Execute(%v) = %v, want usage error", args, err)
			}
		})
	}
}

func TestReviewPrintsTheHostsThatReceiveTheCredential(t *testing.T) {
	cases := []struct {
		name string
		auth Auth
		want string
	}{
		{"static secret", Auth{Kind: AuthStaticSecret, Secret: "token", Hosts: []string{"b.mail.example:8443", "api.mail.example"}}, "Auth: static_secret (secret token, credential sent to api.mail.example, b.mail.example:8443)"},
		{"oauth2", Auth{Kind: AuthOAuth2, AuthorizeURL: "https://auth.mail.example/a", TokenURL: "https://auth.mail.example/t", Scopes: []string{"mail.read"}, RedirectURI: "https://aura.example/cb", ClientIDSecret: "client_id", Hosts: []string{"api.mail.example"}}, "credential sent to api.mail.example)"},
		{"none", Auth{Kind: AuthNone}, "Auth: none"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			text := strings.Join(reviewLines(&Review{ID: "mailer", Auth: tc.auth}), "\n")
			if !strings.Contains(text, tc.want) {
				t.Fatalf("review output = %q, want it to contain %q", text, tc.want)
			}
			if tc.auth.Kind == AuthNone && strings.Contains(text, "credential sent") {
				t.Fatalf("review output = %q", text)
			}
		})
	}
}

func TestReviewPrintsTheApprovalDisplayAndTheOAuthLoginExtras(t *testing.T) {
	oauth := Auth{
		Kind: AuthOAuth2, AuthorizeURL: "https://auth.mail.example/a", TokenURL: "https://auth.mail.example/t",
		Scopes: []string{"mail.read"}, RedirectURI: "http://127.0.0.1/cb", ClientIDSecret: "client_id", Hosts: []string{"api.mail.example"},
		AuthorizeParams: map[string]string{"prompt": "consent", "access_type": "offline"}, Issuer: "https://auth.mail.example", AccessOnly: true,
	}
	review := &Review{
		ID:   "mailer",
		Auth: oauth,
		Tools: []ReviewTool{
			{ID: "plugin_mailer_send", Tier: "effect", Description: "Send mail.", Display: []string{"to", "subject", "body"}},
			{ID: "plugin_mailer_search", Tier: "read", Description: "Search mail."},
		},
	}
	text := strings.Join(reviewLines(review), "\n")
	for _, want := range []string{
		"approval shows, in order: to, subject, body",
		"authorize params access_type=offline prompt=consent",
		"issuer https://auth.mail.example",
		"access only (no refresh token required)",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("review output = %q, want it to contain %q", text, want)
		}
	}
	if strings.Count(text, "approval shows") != 1 {
		t.Fatalf("review output = %q, want the display line only for the tool that sets one", text)
	}
}
