package plugin

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

const connectorManifest = `api_version: aura.plugin/v1
id: mailer
version: 1.0.0
name: Mailer
description: Mail access.
capabilities: [network]
tools:
  - name: search
    description: Search mail.
    tier: read
  - name: send
    description: Send mail.
    tier: effect
egress: [api.mail.example, uploads.mail.example:8443]
secrets: []
auth:
  kind: none
backend:
  kind: connector
  command: ./bin/connector
  args: [--quiet]
  startup_timeout: 5s
  request_timeout: 20s
  max_message_size: 512KiB
`

func installConnector(t *testing.T, env *testEnv, manifest string) *InstallResult {
	t.Helper()
	result, err := env.service.Install(t.Context(), writeConnectorBundle(t, manifest, 0o700))
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	return result
}

func writeConnectorBundle(t *testing.T, manifest string, mode os.FileMode) string {
	t.Helper()
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, ManifestFile), manifest, 0o600)
	writeFile(t, filepath.Join(dir, "bin", "connector"), "#!/bin/sh\nexit 0\n", mode)
	return dir
}

func connectorOAuthManifest(t *testing.T) string {
	t.Helper()
	text := replaceOnce(t, connectorManifest, "secrets: []", "secrets:\n  - name: client_id\n    ref: env://MAILER_CLIENT_ID")
	text = replaceOnce(t, text, "egress: [api.mail.example, uploads.mail.example:8443]", "egress: [auth.mail.example, api.mail.example]")
	return replaceOnce(t, text, "  kind: none", `  kind: oauth2
  authorize_url: https://auth.mail.example/authorize
  token_url: https://auth.mail.example/token
  scopes: [mail.read]
  redirect_uri: http://127.0.0.1:8765/callback
  client_id_secret: client_id
  hosts: [api.mail.example]`)
}

func connectorStaticManifest(t *testing.T) string {
	t.Helper()
	text := replaceOnce(t, connectorManifest, "secrets: []", "secrets:\n  - name: token\n    ref: env://MAILER_TOKEN")
	return replaceOnce(t, text, "  kind: none", "  kind: static_secret\n  secret: token\n  hosts: [api.mail.example]")
}

func TestParseManifestAcceptsAConnectorBackend(t *testing.T) {
	cases := []struct {
		name string
		text func(t *testing.T) string
	}{
		{"no auth", func(*testing.T) string { return connectorManifest }},
		{"oauth2", connectorOAuthManifest},
		{"static secret", connectorStaticManifest},
		{"auth host written with the default port", func(t *testing.T) string {
			return replaceOnce(t, connectorStaticManifest(t), "hosts: [api.mail.example]", "hosts: [api.mail.example:443]")
		}},
		{"auth host with its own port", func(t *testing.T) string {
			return replaceOnce(t, connectorStaticManifest(t), "hosts: [api.mail.example]", "hosts: [uploads.mail.example:8443, api.mail.example]")
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			manifest, err := ParseManifest([]byte(tc.text(t)))
			if err != nil {
				t.Fatalf("ParseManifest: %v", err)
			}
			server := manifest.ServerConfig("/data/plugins/mailer/1.0.0")
			if server.Transport != "stdio" || server.Command != "/data/plugins/mailer/1.0.0/bin/connector" || server.URL != "" {
				t.Fatalf("server = %+v", server)
			}
			if server.MaxMessageSize != 512<<10 || time.Duration(server.RequestTimeout) != 20*time.Second || time.Duration(server.StartupTimeout) != 5*time.Second {
				t.Fatalf("server limits = %+v", server)
			}
		})
	}
}

func TestParseManifestRejectsABadConnectorBackend(t *testing.T) {
	cases := []struct {
		name string
		text func(t *testing.T) string
	}{
		{"no command", func(t *testing.T) string {
			return replaceOnce(t, connectorManifest, "  command: ./bin/connector\n", "")
		}},
		{"absolute command", func(t *testing.T) string {
			return replaceOnce(t, connectorManifest, "command: ./bin/connector", "command: /bin/sh")
		}},
		{"escaping command", func(t *testing.T) string {
			return replaceOnce(t, connectorManifest, "command: ./bin/connector", "command: ../connector")
		}},
		{"transport set", func(t *testing.T) string {
			return replaceOnce(t, connectorManifest, "  kind: connector\n", "  kind: connector\n  transport: stdio\n")
		}},
		{"url set", func(t *testing.T) string {
			return replaceOnce(t, connectorManifest, "  kind: connector\n", "  kind: connector\n  url: https://api.mail.example/mcp\n")
		}},
		{"connect timeout set", func(t *testing.T) string {
			return replaceOnce(t, connectorManifest, "  startup_timeout: 5s\n", "  connect_timeout: 5s\n")
		}},
		{"bad request timeout", func(t *testing.T) string {
			return replaceOnce(t, connectorManifest, "request_timeout: 20s", "request_timeout: soon")
		}},
		{"unknown backend field", func(t *testing.T) string { return connectorManifest + "  rpc: v1\n" }},
		{"oauth2 without auth hosts", func(t *testing.T) string {
			return replaceOnce(t, connectorOAuthManifest(t), "\n  hosts: [api.mail.example]", "")
		}},
		{"oauth2 with empty auth hosts", func(t *testing.T) string {
			return replaceOnce(t, connectorOAuthManifest(t), "hosts: [api.mail.example]", "hosts: []")
		}},
		{"static secret without auth hosts", func(t *testing.T) string {
			return replaceOnce(t, connectorStaticManifest(t), "\n  hosts: [api.mail.example]", "")
		}},
		{"auth host not listed in egress", func(t *testing.T) string {
			return replaceOnce(t, connectorStaticManifest(t), "hosts: [api.mail.example]", "hosts: [other.mail.example]")
		}},
		{"auth host on another port than the egress entry", func(t *testing.T) string {
			return replaceOnce(t, connectorStaticManifest(t), "hosts: [api.mail.example]", "hosts: [api.mail.example:8443]")
		}},
		{"auth host listed twice after normalization", func(t *testing.T) string {
			return replaceOnce(t, connectorStaticManifest(t), "hosts: [api.mail.example]", "hosts: [api.mail.example, api.mail.example:443]")
		}},
		{"auth host with a wildcard", func(t *testing.T) string {
			return replaceOnce(t, connectorStaticManifest(t), "hosts: [api.mail.example]", "hosts: ['*.mail.example']")
		}},
		{"auth host in uppercase", func(t *testing.T) string {
			return replaceOnce(t, connectorStaticManifest(t), "hosts: [api.mail.example]", "hosts: [API.mail.example]")
		}},
		{"auth hosts without auth", func(t *testing.T) string {
			return replaceOnce(t, connectorManifest, "  kind: none", "  kind: none\n  hosts: [api.mail.example]")
		}},
		{"oauth2 token host not listed", func(t *testing.T) string {
			return replaceOnce(t, connectorOAuthManifest(t), "egress: [auth.mail.example, api.mail.example]", "egress: [api.mail.example]")
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ParseManifest([]byte(tc.text(t))); !hasCode(err, ErrManifestInvalid) {
				t.Fatalf("ParseManifest error = %v, want %s", err, ErrManifestInvalid)
			}
		})
	}
}

func TestInstallRequiresTheConnectorExecutableInsideTheBundle(t *testing.T) {
	cases := []struct {
		name   string
		bundle func(t *testing.T) string
		want   ErrorCode
	}{
		{"executable present", func(t *testing.T) string { return writeConnectorBundle(t, connectorManifest, 0o700) }, ""},
		{"file is not executable", func(t *testing.T) string { return writeConnectorBundle(t, connectorManifest, 0o600) }, ErrManifestInvalid},
		{"file missing", func(t *testing.T) string {
			dir := t.TempDir()
			writeFile(t, filepath.Join(dir, ManifestFile), connectorManifest, 0o600)
			return dir
		}, ErrManifestInvalid},
		{"command names a directory", func(t *testing.T) string {
			dir := t.TempDir()
			writeFile(t, filepath.Join(dir, ManifestFile), replaceOnce(t, connectorManifest, "command: ./bin/connector", "command: ./bin"), 0o600)
			writeFile(t, filepath.Join(dir, "bin", "connector"), "x", 0o700)
			return dir
		}, ErrManifestInvalid},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newTestEnv(t)
			result, err := env.service.Install(t.Context(), tc.bundle(t))
			if tc.want == "" {
				if err != nil || result.State != StateQuarantined {
					t.Fatalf("Install = %+v, %v; want a quarantined plugin", result, err)
				}
				return
			}
			if !hasCode(err, tc.want) {
				t.Fatalf("Install error = %v, want %s", err, tc.want)
			}
		})
	}
}

func TestAConnectorPluginReviewsAndApprovesLikeAnyPlugin(t *testing.T) {
	env := newTestEnv(t)
	result := installConnector(t, env, connectorManifest)
	review, err := env.service.Review(t.Context(), "mailer")
	if err != nil {
		t.Fatalf("Review: %v", err)
	}
	if review.Backend.Kind != BackendConnector || review.Backend.Command != "./bin/connector" || len(review.Tools) != 2 || !slices.Equal(review.Egress, []string{"api.mail.example", "uploads.mail.example:8443"}) {
		t.Fatalf("review = %+v", review)
	}
	if _, err := env.service.Approve(t.Context(), "mailer", result.Digest[:MinDigestPrefix]); err != nil {
		t.Fatalf("Approve: %v", err)
	}
	plan, err := env.service.Plan(t.Context())
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if len(plan.Held) != 0 || len(plan.Active) != 1 {
		t.Fatalf("plan = %+v", plan)
	}
	active := plan.Active[0]
	if active.Kind != BackendConnector || active.Root != filepath.Join(env.dir, "mailer", "1.0.0") || active.Server.Command != filepath.Join(active.Root, "bin", "connector") {
		t.Fatalf("active = %+v", active)
	}
	if !slices.Equal(active.Egress, []string{"api.mail.example", "uploads.mail.example:8443"}) {
		t.Fatalf("egress = %v", active.Egress)
	}
}

func TestPlanOfAConnectorLeavesAWidenedEgressHostOut(t *testing.T) {
	env := newTestEnv(t)
	env.approve("mailer", installConnector(t, env, connectorManifest))
	widened := replaceOnce(t, connectorManifest, "version: 1.0.0", "version: 1.1.0")
	widened = replaceOnce(t, widened, "uploads.mail.example:8443]", "uploads.mail.example:8443, cdn.mail.example]")
	update := installConnector(t, env, widened)
	if !update.Diff.Widens() || update.Diff.Quarantines() {
		t.Fatalf("update = %+v", update)
	}
	plan, err := env.service.Plan(t.Context())
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if len(plan.Active) != 1 || slices.Contains(plan.Active[0].Egress, "cdn.mail.example") {
		t.Fatalf("plan = %+v", plan)
	}
}

func TestAuthHostsBelongOnlyToAConnectorWithCredentials(t *testing.T) {
	mcp := replaceOnce(t, baseManifest, "  kind: none", "  kind: none\n  hosts: [api.example.com]")
	if _, err := ParseManifest([]byte(mcp)); !hasCode(err, ErrManifestInvalid) {
		t.Fatalf("mcp manifest with auth hosts: %v, want %s", err, ErrManifestInvalid)
	}
}

func TestChangingAuthHostsQuarantinesAndInvalidatesTheToken(t *testing.T) {
	approved := mustSurface(t, connectorStaticManifest(t), "b")
	widened := mustSurface(t, replaceOnce(t, connectorStaticManifest(t), "hosts: [api.mail.example]", "hosts: [api.mail.example, uploads.mail.example:8443]"), "b")
	diff := Compare(&approved, &widened)
	if !diff.Quarantines() || !slices.ContainsFunc(diff.Restart, func(change Change) bool { return change.Surface == SurfaceAuth }) {
		t.Fatalf("diff = %+v, want an auth restart", diff)
	}
	if approved.Auth == widened.Auth {
		t.Fatal("the auth fingerprint ignores the auth hosts")
	}
	first, second := mustSurface(t, replaceOnce(t, connectorStaticManifest(t), "hosts: [api.mail.example]", "hosts: [uploads.mail.example:8443, api.mail.example]"), "b"), widened
	if first.Auth != second.Auth {
		t.Fatal("the auth fingerprint depends on the order of the auth hosts")
	}
}

func TestAConnectorPluginCarriesItsAuthHostsIntoTheActivePlan(t *testing.T) {
	env := newTestEnv(t)
	manifest := replaceOnce(t, connectorStaticManifest(t), "hosts: [api.mail.example]", "hosts: [uploads.mail.example:8443, api.mail.example]")
	result := installConnector(t, env, manifest)
	if _, err := env.service.Approve(t.Context(), "mailer", result.Digest[:MinDigestPrefix]); err != nil {
		t.Fatalf("Approve: %v", err)
	}
	env.secrets.set("env://MAILER_TOKEN", "token-value")
	plan, err := env.service.Plan(t.Context())
	if err != nil || len(plan.Active) != 1 {
		t.Fatalf("Plan = %+v, %v", plan, err)
	}
	if want := []string{"api.mail.example", "uploads.mail.example:8443"}; !slices.Equal(plan.Active[0].AuthHosts, want) {
		t.Fatalf("auth hosts = %v, want %v", plan.Active[0].AuthHosts, want)
	}
}
