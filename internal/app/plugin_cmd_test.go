package app

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/anggasct/aura/internal/modules/plugin"
)

const cliPluginManifest = `api_version: aura.plugin/v1
id: demo
version: 1.0.0
name: Demo
description: A demo plugin.
capabilities: []
tools:
  - name: forecast
    description: Get a forecast.
    tier: read
egress: []
secrets: []
auth:
  kind: none
backend:
  kind: mcp
  transport: stdio
  command: ./server
`

func writeCLIPluginBundle(t *testing.T, manifest string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "plugin.yaml"), []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "server"), []byte("#!/bin/sh\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	return dir
}

func runPluginCommand(t *testing.T, cfg string, args ...string) (string, error) {
	t.Helper()
	cmd := newPluginCmd(&globalFlags{configPath: cfg})
	out := &bytes.Buffer{}
	cmd.SetOut(out)
	cmd.SetErr(out)
	cmd.SetArgs(args)
	err := cmd.ExecuteContext(t.Context())
	return out.String(), err
}

func pluginDigestFromReview(t *testing.T, review string) string {
	t.Helper()
	match := regexp.MustCompile(`Digest: ([0-9a-f]{64})`).FindStringSubmatch(review)
	if match == nil {
		t.Fatalf("review has no digest: %q", review)
	}
	return match[1]
}

func TestPluginCommandsInstallReviewApproveLifecycle(t *testing.T) {
	cfg := writeChildrenCLIConfig(t)
	if out, err := runPluginCommand(t, cfg, "list"); err != nil || strings.TrimSpace(out) != "no plugins installed" {
		t.Fatalf("empty list = %q, %v", out, err)
	}
	source := writeCLIPluginBundle(t, cliPluginManifest)
	out, err := runPluginCommand(t, cfg, "install", source)
	if err != nil || !strings.Contains(out, "installed demo 1.0.0 as quarantined") {
		t.Fatalf("install = %q, %v", out, err)
	}
	if out, err := runPluginCommand(t, cfg, "install", source); err != nil || !strings.Contains(out, "no change") {
		t.Fatalf("reinstall = %q, %v", out, err)
	}
	review, err := runPluginCommand(t, cfg, "review", "demo")
	if err != nil {
		t.Fatalf("review: %v", err)
	}
	for _, want := range []string{"State: quarantined", "demo.forecast [read]: Get a forecast.", "Backend: mcp"} {
		if !strings.Contains(review, want) {
			t.Fatalf("review lacks %q: %q", want, review)
		}
	}
	digest := pluginDigestFromReview(t, review)
	if _, err := runPluginCommand(t, cfg, "approve", "demo"); err == nil {
		t.Fatal("approve without a digest succeeded")
	}
	if _, err := runPluginCommand(t, cfg, "approve", "demo", "--digest", strings.Repeat("0", 16)); err == nil {
		t.Fatal("approve with a wrong digest succeeded")
	}
	if out, err := runPluginCommand(t, cfg, "list"); err != nil || !strings.Contains(out, "demo\tquarantined\t1.0.0\t-\t") {
		t.Fatalf("list before approval = %q, %v", out, err)
	}
	if out, err := runPluginCommand(t, cfg, "approve", "demo", "--digest", digest[:16]); err != nil || !strings.Contains(out, "approved demo 1.0.0") || !strings.Contains(out, "demo.forecast") {
		t.Fatalf("approve = %q, %v", out, err)
	}
	if accessEventCount(t, cfg, accessActionPluginApprove) != 1 {
		t.Fatal("approve left no owner-action event")
	}
	listed, err := runPluginCommand(t, cfg, "list", "--json")
	if err != nil {
		t.Fatalf("list json: %v", err)
	}
	var parsed pluginListOutput
	if err := json.Unmarshal([]byte(listed), &parsed); err != nil || len(parsed.Plugins) != 1 || parsed.Plugins[0].State != "active" || parsed.Plugins[0].ApprovedVersion != "1.0.0" {
		t.Fatalf("json list = %q, %v", listed, err)
	}
	if out, err := runPluginCommand(t, cfg, "disable", "demo"); err != nil || !strings.Contains(out, "disabled demo") {
		t.Fatalf("disable = %q, %v", out, err)
	}
	if accessEventCount(t, cfg, accessActionPluginDisable) != 1 {
		t.Fatal("disable left no owner-action event")
	}
	if out, _ := runPluginCommand(t, cfg, "list"); !strings.Contains(out, "demo\tdisabled\t") {
		t.Fatalf("list after disable = %q", out)
	}
}

func TestPluginRemoveDryRunKeepsEverythingUntilApply(t *testing.T) {
	cfg := writeChildrenCLIConfig(t)
	if _, err := runPluginCommand(t, cfg, "install", writeCLIPluginBundle(t, cliPluginManifest)); err != nil {
		t.Fatalf("install: %v", err)
	}
	bundle := filepath.Join(filepath.Dir(cfg), "plugins", "demo")
	if _, err := os.Stat(bundle); err != nil {
		t.Fatalf("bundle missing after install: %v", err)
	}
	out, err := runPluginCommand(t, cfg, "remove", "demo")
	if err != nil || !strings.Contains(out, "would remove demo") {
		t.Fatalf("dry run = %q, %v", out, err)
	}
	if _, err := os.Stat(bundle); err != nil {
		t.Fatalf("dry run removed the bundle: %v", err)
	}
	if out, _ := runPluginCommand(t, cfg, "list"); !strings.Contains(out, "demo\t") {
		t.Fatalf("dry run removed the registry row: %q", out)
	}
	if accessEventCount(t, cfg, accessActionPluginRemove) != 0 {
		t.Fatal("dry run recorded an owner action")
	}
	if out, err := runPluginCommand(t, cfg, "remove", "demo", "--apply"); err != nil || !strings.Contains(out, "removed demo") {
		t.Fatalf("apply = %q, %v", out, err)
	}
	if _, err := os.Stat(bundle); !os.IsNotExist(err) {
		t.Fatalf("bundle still present: %v", err)
	}
	if out, _ := runPluginCommand(t, cfg, "list"); strings.TrimSpace(out) != "no plugins installed" {
		t.Fatalf("list after remove = %q", out)
	}
	if accessEventCount(t, cfg, accessActionPluginRemove) != 1 {
		t.Fatal("remove left no owner-action event")
	}
	if _, err := runPluginCommand(t, cfg, "remove", "demo", "--apply"); err == nil {
		t.Fatal("removing an unknown plugin succeeded")
	}
}

func TestPluginInstallRejectsBadBundlesWithoutCopying(t *testing.T) {
	cases := []struct {
		name  string
		setup func(t *testing.T) string
	}{
		{"malformed manifest", func(t *testing.T) string { return writeCLIPluginBundle(t, "not: [valid") }},
		{"unknown field", func(t *testing.T) string { return writeCLIPluginBundle(t, cliPluginManifest+"surprise: true\n") }},
		{"symlink in a subdirectory", func(t *testing.T) string {
			dir := writeCLIPluginBundle(t, cliPluginManifest)
			if err := os.Mkdir(filepath.Join(dir, "lib"), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink("/etc/passwd", filepath.Join(dir, "lib", "link")); err != nil {
				t.Fatal(err)
			}
			return dir
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := writeChildrenCLIConfig(t)
			if _, err := runPluginCommand(t, cfg, "install", tc.setup(t)); err == nil {
				t.Fatal("install succeeded")
			}
			entries, err := os.ReadDir(filepath.Join(filepath.Dir(cfg), "plugins"))
			if err == nil && len(entries) > 0 {
				t.Fatalf("install left %d entries behind", len(entries))
			}
			if out, _ := runPluginCommand(t, cfg, "list"); strings.TrimSpace(out) != "no plugins installed" {
				t.Fatalf("rejected install registered a plugin: %q", out)
			}
		})
	}
}

func TestReviewPrintsTheHostsThatReceiveTheCredential(t *testing.T) {
	cases := []struct {
		name string
		auth plugin.Auth
		want string
	}{
		{"static secret", plugin.Auth{Kind: plugin.AuthStaticSecret, Secret: "token", Hosts: []string{"b.mail.example:8443", "api.mail.example"}}, "Auth: static_secret (secret token, credential sent to api.mail.example, b.mail.example:8443)"},
		{"oauth2", plugin.Auth{Kind: plugin.AuthOAuth2, AuthorizeURL: "https://auth.mail.example/a", TokenURL: "https://auth.mail.example/t", Scopes: []string{"mail.read"}, RedirectURI: "https://aura.example/cb", ClientIDSecret: "client_id", Hosts: []string{"api.mail.example"}}, "credential sent to api.mail.example)"},
		{"none", plugin.Auth{Kind: plugin.AuthNone}, "Auth: none"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			text := strings.Join(reviewLines(&plugin.Review{ID: "mailer", Auth: tc.auth}), "\n")
			if !strings.Contains(text, tc.want) {
				t.Fatalf("review output = %q, want it to contain %q", text, tc.want)
			}
			if tc.auth.Kind == plugin.AuthNone && strings.Contains(text, "credential sent") {
				t.Fatalf("review output = %q", text)
			}
		})
	}
}

func TestReviewPrintsTheApprovalDisplayAndTheOAuthLoginExtras(t *testing.T) {
	oauth := plugin.Auth{
		Kind: plugin.AuthOAuth2, AuthorizeURL: "https://auth.mail.example/a", TokenURL: "https://auth.mail.example/t",
		Scopes: []string{"mail.read"}, RedirectURI: "http://127.0.0.1/cb", ClientIDSecret: "client_id", Hosts: []string{"api.mail.example"},
		AuthorizeParams: map[string]string{"prompt": "consent", "access_type": "offline"}, Issuer: "https://auth.mail.example", AccessOnly: true,
	}
	review := &plugin.Review{
		ID:   "mailer",
		Auth: oauth,
		Tools: []plugin.ReviewTool{
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
