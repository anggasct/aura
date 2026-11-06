package plugin

import (
	"context"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/anggasct/aura/internal/health"
	"github.com/anggasct/aura/internal/plugin/auth"
	"github.com/anggasct/aura/internal/plugin/auth/authtest"
)

const oauthManifest = `api_version: aura.plugin/v1
id: mailer
version: 1.0.0
name: Mailer
description: Mail access.
capabilities: [network]
tools:
  - name: search
    description: Search mail.
    tier: read
egress: [auth.mail.example, api.mail.example]
secrets:
  - name: client_id
    ref: env://MAILER_CLIENT_ID
  - name: client_secret
    ref: env://MAILER_CLIENT_SECRET
auth:
  kind: oauth2
  authorize_url: https://auth.mail.example/authorize
  token_url: https://auth.mail.example/token
  scopes: [mail.read]
  redirect_uri: http://127.0.0.1:8765/callback
  client_id_secret: client_id
  client_secret_secret: client_secret
backend:
  kind: mcp
  transport: streamable_http
  url: https://api.mail.example/mcp
`

const (
	clientIDRef     = "env://MAILER_CLIENT_ID"
	clientSecretRef = "env://MAILER_CLIENT_SECRET"
)

func TestParseManifestAcceptsOAuth2(t *testing.T) {
	cases := []struct {
		name string
		text func(t *testing.T) string
	}{
		{"confidential client", func(*testing.T) string { return oauthManifest }},
		{"public client", func(t *testing.T) string {
			text := replaceOnce(t, oauthManifest, "  - name: client_secret\n    ref: env://MAILER_CLIENT_SECRET\n", "")
			return replaceOnce(t, text, "  client_secret_secret: client_secret\n", "")
		}},
		{"https redirect", func(t *testing.T) string {
			return replaceOnce(t, oauthManifest, "http://127.0.0.1:8765/callback", "https://aura.example/oauth/callback")
		}},
		{"loopback endpoints", func(t *testing.T) string {
			text := replaceOnce(t, oauthManifest, "https://auth.mail.example/authorize", "http://127.0.0.1:9001/authorize")
			text = replaceOnce(t, text, "https://auth.mail.example/token", "http://127.0.0.1:9001/token")
			return replaceOnce(t, text, "egress: [auth.mail.example, api.mail.example]", "egress: [127.0.0.1:9001, api.mail.example]")
		}},
		{"several scopes", func(t *testing.T) string {
			return replaceOnce(t, oauthManifest, "scopes: [mail.read]", "scopes: [mail.read, 'https://www.example/auth/mail.send', offline_access]")
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			manifest, err := ParseManifest([]byte(tc.text(t)))
			if err != nil {
				t.Fatalf("ParseManifest: %v", err)
			}
			spec := manifest.AuthSpec("fp")
			if spec.Kind != AuthOAuth2 || spec.ClientIDRef != clientIDRef || spec.TokenURL == "" || spec.AuthorizeURL == "" || spec.RedirectURI == "" || len(spec.Scopes) == 0 {
				t.Fatalf("auth spec = %+v", spec)
			}
			if server := manifest.ServerConfig("/plugins/mailer/1.0.0"); server.Auth != nil {
				t.Fatalf("server auth = %+v, want the strategy to own authentication", server.Auth)
			}
		})
	}
}

func TestParseManifestRejectsInvalidOAuth2(t *testing.T) {
	cases := []struct {
		name string
		edit func(t *testing.T) string
	}{
		{"authorize url over http", func(t *testing.T) string {
			return replaceOnce(t, oauthManifest, "https://auth.mail.example/authorize", "http://auth.mail.example/authorize")
		}},
		{"authorize url with credentials", func(t *testing.T) string {
			return replaceOnce(t, oauthManifest, "https://auth.mail.example/authorize", "https://user:pw@auth.mail.example/authorize")
		}},
		{"authorize url with fragment", func(t *testing.T) string {
			return replaceOnce(t, oauthManifest, "https://auth.mail.example/authorize", "https://auth.mail.example/authorize#frag")
		}},
		{"authorize url missing", func(t *testing.T) string {
			return replaceOnce(t, oauthManifest, "  authorize_url: https://auth.mail.example/authorize\n", "")
		}},
		{"token url over http", func(t *testing.T) string {
			return replaceOnce(t, oauthManifest, "https://auth.mail.example/token", "http://auth.mail.example/token")
		}},
		{"token url host outside egress", func(t *testing.T) string {
			return replaceOnce(t, oauthManifest, "https://auth.mail.example/token", "https://elsewhere.example/token")
		}},
		{"token url missing", func(t *testing.T) string {
			return replaceOnce(t, oauthManifest, "  token_url: https://auth.mail.example/token\n", "")
		}},
		{"no scopes", func(t *testing.T) string { return replaceOnce(t, oauthManifest, "  scopes: [mail.read]\n", "") }},
		{"empty scopes", func(t *testing.T) string { return replaceOnce(t, oauthManifest, "scopes: [mail.read]", "scopes: []") }},
		{"scope with a space", func(t *testing.T) string {
			return replaceOnce(t, oauthManifest, "scopes: [mail.read]", `scopes: ["mail.read mail.send"]`)
		}},
		{"scope with a quote", func(t *testing.T) string {
			return replaceOnce(t, oauthManifest, "scopes: [mail.read]", `scopes: ['mail"read']`)
		}},
		{"scope with a control character", func(t *testing.T) string {
			return replaceOnce(t, oauthManifest, "scopes: [mail.read]", `scopes: ["mail\nread"]`)
		}},
		{"scope with non ascii", func(t *testing.T) string {
			return replaceOnce(t, oauthManifest, "scopes: [mail.read]", `scopes: ["mail.réad"]`)
		}},
		{"duplicate scope", func(t *testing.T) string {
			return replaceOnce(t, oauthManifest, "scopes: [mail.read]", "scopes: [mail.read, mail.read]")
		}},
		{"too many scopes", func(t *testing.T) string {
			var scopes []string
			for i := range 33 {
				scopes = append(scopes, "s"+strings.Repeat("a", i))
			}
			return replaceOnce(t, oauthManifest, "scopes: [mail.read]", "scopes: ["+strings.Join(scopes, ",")+"]")
		}},
		{"redirect over http on a named host", func(t *testing.T) string {
			return replaceOnce(t, oauthManifest, "http://127.0.0.1:8765/callback", "http://localhost:8765/callback")
		}},
		{"redirect over http on another address", func(t *testing.T) string {
			return replaceOnce(t, oauthManifest, "http://127.0.0.1:8765/callback", "http://10.0.0.5:8765/callback")
		}},
		{"redirect with port zero", func(t *testing.T) string {
			return replaceOnce(t, oauthManifest, "http://127.0.0.1:8765/callback", "http://127.0.0.1:0/callback")
		}},
		{"redirect with port out of range", func(t *testing.T) string {
			return replaceOnce(t, oauthManifest, "http://127.0.0.1:8765/callback", "http://127.0.0.1:70000/callback")
		}},
		{"redirect without a path", func(t *testing.T) string {
			return replaceOnce(t, oauthManifest, "http://127.0.0.1:8765/callback", "http://127.0.0.1:8765")
		}},
		{"redirect with a query", func(t *testing.T) string {
			return replaceOnce(t, oauthManifest, "http://127.0.0.1:8765/callback", "http://127.0.0.1:8765/callback?x=1")
		}},
		{"redirect with a fragment", func(t *testing.T) string {
			return replaceOnce(t, oauthManifest, "http://127.0.0.1:8765/callback", "https://aura.example/callback#x")
		}},
		{"redirect with another scheme", func(t *testing.T) string {
			return replaceOnce(t, oauthManifest, "http://127.0.0.1:8765/callback", "ftp://aura.example/callback")
		}},
		{"redirect missing", func(t *testing.T) string {
			return replaceOnce(t, oauthManifest, "  redirect_uri: http://127.0.0.1:8765/callback\n", "")
		}},
		{"client id missing", func(t *testing.T) string { return replaceOnce(t, oauthManifest, "  client_id_secret: client_id\n", "") }},
		{"client id undeclared", func(t *testing.T) string {
			return replaceOnce(t, oauthManifest, "client_id_secret: client_id", "client_id_secret: other")
		}},
		{"client secret undeclared", func(t *testing.T) string {
			return replaceOnce(t, oauthManifest, "client_secret_secret: client_secret", "client_secret_secret: other")
		}},
		{"client secret equals client id", func(t *testing.T) string {
			return replaceOnce(t, oauthManifest, "client_secret_secret: client_secret", "client_secret_secret: client_id")
		}},
		{"unreferenced secret", func(t *testing.T) string {
			return replaceOnce(t, oauthManifest, "  client_secret_secret: client_secret\n", "")
		}},
		{"three secrets", func(t *testing.T) string {
			return replaceOnce(t, oauthManifest, "auth:\n", "  - name: extra\n    ref: env://EXTRA\nauth:\n")
		}},
		{"stdio transport", func(t *testing.T) string {
			return replaceOnce(t, oauthManifest, "  transport: streamable_http\n  url: https://api.mail.example/mcp", "  transport: stdio\n  command: ./server")
		}},
		{"static secret field", func(t *testing.T) string {
			return replaceOnce(t, oauthManifest, "  scopes:", "  secret: client_id\n  scopes:")
		}},
		{"header field", func(t *testing.T) string {
			return replaceOnce(t, oauthManifest, "  scopes:", "  header: X-Key\n  scopes:")
		}},
		{"oauth2 field on none", func(t *testing.T) string {
			return replaceOnce(t, baseManifest, "kind: none", "kind: none\n  scopes: [mail.read]")
		}},
		{"oauth2 field on static secret", func(t *testing.T) string {
			text := replaceOnce(t, oauthManifest, "  - name: client_secret\n    ref: env://MAILER_CLIENT_SECRET\n", "")
			text = replaceOnce(t, text, "kind: oauth2", "kind: static_secret\n  secret: client_id")
			return replaceOnce(t, text, "  client_id_secret: client_id\n  client_secret_secret: client_secret\n", "  client_id_secret: client_id\n")
		}},
		{"secret holding a value", func(t *testing.T) string {
			return replaceOnce(t, oauthManifest, "ref: env://MAILER_CLIENT_SECRET", "ref: sk-live-0123456789abcdef")
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseManifest([]byte(tc.edit(t)))
			if !hasCode(err, ErrManifestInvalid) {
				t.Fatalf("ParseManifest error = %v, want %s", err, ErrManifestInvalid)
			}
			if err != nil && strings.Contains(err.Error(), "sk-live-0123456789abcdef") {
				t.Fatalf("error echoes a secret-looking value: %v", err)
			}
		})
	}
}

func TestAnyAuthBlockChangeQuarantinesAndInvalidatesTheToken(t *testing.T) {
	cases := []struct {
		name string
		edit func(t *testing.T, text string) string
	}{
		{"scope added", func(t *testing.T, text string) string {
			return replaceOnce(t, text, "scopes: [mail.read]", "scopes: [mail.read, mail.send]")
		}},
		{"scope replaced", func(t *testing.T, text string) string {
			return replaceOnce(t, text, "scopes: [mail.read]", "scopes: [mail.send]")
		}},
		{"authorize url changed", func(t *testing.T, text string) string {
			return replaceOnce(t, text, "https://auth.mail.example/authorize", "https://auth.mail.example/v2/authorize")
		}},
		{"token url changed", func(t *testing.T, text string) string {
			return replaceOnce(t, text, "https://auth.mail.example/token", "https://auth.mail.example/v2/token")
		}},
		{"redirect uri changed", func(t *testing.T, text string) string {
			return replaceOnce(t, text, "8765/callback", "8766/callback")
		}},
		{"client id secret renamed", func(t *testing.T, text string) string {
			text = replaceOnce(t, text, "  - name: client_id\n", "  - name: app_id\n")
			return replaceOnce(t, text, "client_id_secret: client_id", "client_id_secret: app_id")
		}},
		{"client secret dropped", func(t *testing.T, text string) string {
			text = replaceOnce(t, text, "  - name: client_secret\n    ref: env://MAILER_CLIENT_SECRET\n", "")
			return replaceOnce(t, text, "  client_secret_secret: client_secret\n", "")
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newTestEnv(t)
			env.approve("mailer", env.install(oauthManifest, nil))
			first := authSurface(t, env, "mailer", "1.0.0")
			env.install(tc.edit(t, bump(t, oauthManifest, "1.1.0")), nil)
			record, err := env.registry.Get(t.Context(), "mailer")
			if err != nil || record.State != StateQuarantined || record.ApprovedVersion != "1.0.0" {
				t.Fatalf("record = %+v, %v; want quarantined", record, err)
			}
			review, err := env.service.Review(t.Context(), "mailer")
			if err != nil || review.Diff == nil || !review.Diff.Quarantines() {
				t.Fatalf("review = %+v, %v", review, err)
			}
			if second := authSurface(t, env, "mailer", "1.1.0"); second == first {
				t.Fatal("the auth fingerprint did not change")
			}
		})
	}
}

func TestRemovingAScopeQuarantinesToo(t *testing.T) {
	env := newTestEnv(t)
	wide := replaceOnce(t, oauthManifest, "scopes: [mail.read]", "scopes: [mail.read, mail.send]")
	env.approve("mailer", env.install(wide, nil))
	env.install(bump(t, oauthManifest, "1.1.0"), nil)
	record, err := env.registry.Get(t.Context(), "mailer")
	if err != nil || record.State != StateQuarantined {
		t.Fatalf("record = %+v, %v; want quarantined", record, err)
	}
}

func authSurface(t *testing.T, env *testEnv, id, version string) string {
	t.Helper()
	row, err := env.registry.Version(t.Context(), id, version)
	if err != nil {
		t.Fatalf("Version: %v", err)
	}
	manifest, bundle, err := decodeStored(row.ManifestJSON)
	if err != nil {
		t.Fatalf("decodeStored: %v", err)
	}
	surface, err := SurfaceOf(manifest, bundle)
	if err != nil {
		t.Fatalf("SurfaceOf: %v", err)
	}
	return surface.Auth
}

func TestScopeOrderDoesNotChangeTheAuthFingerprint(t *testing.T) {
	first := replaceOnce(t, oauthManifest, "scopes: [mail.read]", "scopes: [mail.read, mail.send]")
	second := replaceOnce(t, oauthManifest, "scopes: [mail.read]", "scopes: [mail.send, mail.read]")
	parsedFirst, err := ParseManifest([]byte(first))
	if err != nil {
		t.Fatalf("ParseManifest: %v", err)
	}
	parsedSecond, err := ParseManifest([]byte(second))
	if err != nil {
		t.Fatalf("ParseManifest: %v", err)
	}
	a, err := SurfaceOf(parsedFirst, "bundle")
	if err != nil {
		t.Fatalf("SurfaceOf: %v", err)
	}
	b, err := SurfaceOf(parsedSecond, "bundle")
	if err != nil {
		t.Fatalf("SurfaceOf: %v", err)
	}
	if a.Auth != b.Auth {
		t.Fatal("reordering scopes changed the auth fingerprint")
	}
}

func TestOlderAuthBlocksKeepTheirCanonicalForm(t *testing.T) {
	manifest, err := ParseManifest([]byte(baseManifest))
	if err != nil {
		t.Fatalf("ParseManifest: %v", err)
	}
	canonical, err := manifest.Canonical()
	if err != nil {
		t.Fatalf("Canonical: %v", err)
	}
	if !strings.Contains(string(canonical), `"auth":{"kind":"none","secret":"","header":""}`) {
		t.Fatalf("canonical manifest = %s", canonical)
	}
}

func TestReviewShowsTheOAuth2Endpoints(t *testing.T) {
	env := newTestEnv(t)
	env.install(oauthManifest, nil)
	review, err := env.service.Review(t.Context(), "mailer")
	if err != nil {
		t.Fatalf("Review: %v", err)
	}
	want := Auth{
		Kind: AuthOAuth2, AuthorizeURL: "https://auth.mail.example/authorize", TokenURL: "https://auth.mail.example/token",
		Scopes: []string{"mail.read"}, RedirectURI: "http://127.0.0.1:8765/callback", ClientIDSecret: "client_id", ClientSecretSecret: "client_secret",
	}
	got := review.Auth
	if got.Kind != want.Kind || got.AuthorizeURL != want.AuthorizeURL || got.TokenURL != want.TokenURL || got.RedirectURI != want.RedirectURI ||
		got.ClientIDSecret != want.ClientIDSecret || got.ClientSecretSecret != want.ClientSecretSecret || len(got.Scopes) != 1 || got.Scopes[0] != "mail.read" {
		t.Fatalf("review auth = %+v", got)
	}
}

func TestRequiredPendingDemandsTheTokenEndpointGrant(t *testing.T) {
	manifest, err := ParseManifest([]byte(oauthManifest))
	if err != nil {
		t.Fatalf("ParseManifest: %v", err)
	}
	granted := map[GrantKey]string{}
	for _, capability := range manifest.Capabilities {
		granted[GrantKey{SurfaceCapability, capability}] = ValueFingerprint(SurfaceCapability, capability)
	}
	for i := range manifest.Secrets {
		granted[GrantKey{SurfaceSecret, manifest.Secrets[i].Name}] = SecretFingerprint(&manifest.Secrets[i])
	}
	granted[GrantKey{SurfaceEgress, "api.mail.example"}] = ValueFingerprint(SurfaceEgress, "api.mail.example")
	if got := requiredPending(manifest, granted); got != "egress for the token url" {
		t.Fatalf("requiredPending = %q", got)
	}
	granted[GrantKey{SurfaceEgress, "auth.mail.example"}] = ValueFingerprint(SurfaceEgress, "auth.mail.example")
	if got := requiredPending(manifest, granted); got != "" {
		t.Fatalf("requiredPending = %q", got)
	}
}

type oauthFixture struct {
	env  *testEnv
	as   *authtest.Server
	text string
}

func newOAuthFixture(t *testing.T) *oauthFixture {
	t.Helper()
	as := authtest.New(t)
	parsed, err := url.Parse(as.URL())
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	text := oauthManifest
	text = replaceOnce(t, text, "https://auth.mail.example/authorize", as.AuthorizeURL())
	text = replaceOnce(t, text, "https://auth.mail.example/token", as.TokenURL())
	text = replaceOnce(t, text, "auth.mail.example, api.mail.example", parsed.Host+", api.mail.example")
	text = replaceOnce(t, text, "http://127.0.0.1:8765/callback", "https://aura.example/callback")
	env := newTestEnv(t)
	env.secrets.set(clientIDRef, authtest.ClientID)
	env.secrets.set(clientSecretRef, authtest.Secret)
	return &oauthFixture{env: env, as: as, text: text}
}

func (f *oauthFixture) installApproved(t *testing.T) {
	t.Helper()
	f.env.approve("mailer", f.env.install(f.text, nil))
}

func (f *oauthFixture) login(t *testing.T) {
	t.Helper()
	input := &strings.Builder{}
	reader := &lazyReader{text: input}
	opts := &auth.LoginOptions{Paste: true, Input: reader, Prompt: func(authorizeURL string) {
		input.WriteString(f.as.Approve(authorizeURL) + "\n")
	}}
	if err := f.env.service.Login(t.Context(), "mailer", opts); err != nil {
		t.Fatalf("Login: %v", err)
	}
}

type lazyReader struct {
	text *strings.Builder
	read bool
}

func (l *lazyReader) Read(p []byte) (int, error) {
	if l.read {
		return 0, os.ErrClosed
	}
	l.read = true
	return copy(p, l.text.String()), nil
}

func (f *oauthFixture) planned(t *testing.T) *Plan {
	t.Helper()
	plan, err := f.env.service.Plan(t.Context())
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	return plan
}

func TestActiveOAuth2PluginWithoutATokenStaysOff(t *testing.T) {
	f := newOAuthFixture(t)
	f.installApproved(t)
	plan := f.planned(t)
	if len(plan.Active) != 0 || len(plan.Held) != 1 || plan.Held[0].Code != ErrAuthRequired {
		t.Fatalf("plan = %+v", plan)
	}
	if strings.Contains(plan.Held[0].Detail, authtest.ClientID) {
		t.Fatalf("held detail leaks a credential: %q", plan.Held[0].Detail)
	}
}

func TestActiveOAuth2PluginWithATokenRunsWithItsStrategy(t *testing.T) {
	f := newOAuthFixture(t)
	f.installApproved(t)
	f.login(t)
	plan := f.planned(t)
	if len(plan.Held) != 0 || len(plan.Active) != 1 {
		t.Fatalf("plan = %+v", plan)
	}
	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://api.mail.example/mcp", http.NoBody)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	if err := plan.Active[0].Auth.Authorize(t.Context(), request); err != nil {
		t.Fatalf("Authorize: %v", err)
	}
	if request.Header.Get("Authorization") != "Bearer "+f.as.AccessToken(1) {
		t.Fatalf("Authorization = %q", request.Header.Get("Authorization"))
	}
	if !f.env.redactor.has(f.as.AccessToken(1)) || !f.env.redactor.has(authtest.Secret) {
		t.Fatal("the token and the client secret were not registered for redaction")
	}
}

func TestPlanReportsAPermanentlyRejectedTokenAfterARestart(t *testing.T) {
	f := newOAuthFixture(t)
	f.as.ExpireIn(1)
	f.installApproved(t)
	f.login(t)
	plan := f.planned(t)
	f.as.QueueInvalidGrant()
	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://api.mail.example/mcp", http.NoBody)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	if code, _ := auth.CodeOf(plan.Active[0].Auth.Authorize(t.Context(), request)); code != auth.ErrFailed {
		t.Fatalf("Authorize code = %q", code)
	}
	if got := plan.Active[0].AuthStatus(t.Context()); got != ErrAuthFailed {
		t.Fatalf("running status = %q, want %q", got, ErrAuthFailed)
	}
	restarted := f.planned(t)
	if len(restarted.Active) != 0 || len(restarted.Held) != 1 || restarted.Held[0].Code != ErrAuthFailed {
		t.Fatalf("restarted plan = %+v", restarted)
	}
	if strings.Contains(restarted.Held[0].Detail, authtest.PlantedBody) {
		t.Fatalf("held detail leaks provider text: %q", restarted.Held[0].Detail)
	}
	checker := newCheckerForTest(t, f.env, &CheckerOptions{Held: restarted.Held})
	findings := checker.Check(t.Context())
	if len(findings) != 1 || findings[0].Code != string(ErrAuthFailed) || findings[0].Status != health.StatusDegraded || findings[0].Remediation != RemediationLogin {
		t.Fatalf("findings = %+v", findings)
	}
	if strings.Contains(findings[0].Detail, authtest.PlantedBody) {
		t.Fatalf("finding leaks provider text: %q", findings[0].Detail)
	}
}

func TestLoginRefusesPluginsThatAreNotActive(t *testing.T) {
	cases := []struct {
		name  string
		setup func(t *testing.T, f *oauthFixture)
		want  ErrorCode
	}{
		{"quarantined", func(t *testing.T, f *oauthFixture) { f.env.install(f.text, nil) }, ErrSurfacePending},
		{"disabled", func(t *testing.T, f *oauthFixture) {
			f.installApproved(t)
			if err := f.env.service.Disable(t.Context(), "mailer"); err != nil {
				t.Fatalf("Disable: %v", err)
			}
		}, ErrSurfacePending},
		{"tampered", func(t *testing.T, f *oauthFixture) {
			f.installApproved(t)
			if err := os.WriteFile(filepath.Join(f.env.dir, "mailer", "1.0.0", "extra"), []byte("x"), 0o600); err != nil {
				t.Fatalf("write: %v", err)
			}
		}, ErrTampered},
		{"unknown", func(*testing.T, *oauthFixture) {}, ErrNotFound},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newOAuthFixture(t)
			tc.setup(t, f)
			opts := &auth.LoginOptions{Paste: true, Input: strings.NewReader(""), Prompt: func(string) { t.Error("the login prompted for a plugin that is not active") }}
			err := f.env.service.Login(t.Context(), "mailer", opts)
			if !hasCode(err, tc.want) {
				t.Fatalf("Login error = %v, want %s", err, tc.want)
			}
			if _, statErr := os.Stat(filepath.Join(f.env.authDir, "mailer")); !os.IsNotExist(statErr) {
				t.Fatalf("a refused login created storage: %v", statErr)
			}
			if f.as.Exchanges() != 0 {
				t.Fatal("a refused login reached the token endpoint")
			}
		})
	}
}

func TestLoginRefusesPluginsThatNeedNoLogin(t *testing.T) {
	env := newTestEnv(t)
	env.approve("weather", env.install(baseManifest, serverFiles()))
	opts := &auth.LoginOptions{Paste: true, Input: strings.NewReader(""), Prompt: func(string) {}}
	if err := env.service.Login(t.Context(), "weather", opts); !hasCode(err, ErrInvalidArgument) {
		t.Fatalf("Login error = %v", err)
	}
}

func TestLoginRejectsBadArguments(t *testing.T) {
	env := newTestEnv(t)
	opts := &auth.LoginOptions{Prompt: func(string) {}}
	if err := env.service.Login(t.Context(), "../x", opts); !hasCode(err, ErrInvalidArgument) {
		t.Fatalf("Login error = %v", err)
	}
	if err := env.service.Login(t.Context(), "mailer", nil); !hasCode(err, ErrInvalidArgument) {
		t.Fatalf("Login error = %v", err)
	}
	if err := env.service.Logout(t.Context(), "../x"); !hasCode(err, ErrInvalidArgument) {
		t.Fatalf("Logout error = %v", err)
	}
	if err := env.service.Logout(t.Context(), "mailer"); !hasCode(err, ErrNotFound) {
		t.Fatalf("Logout error = %v", err)
	}
}

func TestLogoutAndRemoveEraseTheTokenNamespace(t *testing.T) {
	cases := []struct {
		name string
		act  func(t *testing.T, f *oauthFixture)
	}{
		{"logout", func(t *testing.T, f *oauthFixture) {
			if err := f.env.service.Logout(t.Context(), "mailer"); err != nil {
				t.Fatalf("Logout: %v", err)
			}
		}},
		{"remove apply", func(t *testing.T, f *oauthFixture) {
			plan, err := f.env.service.Remove(t.Context(), "mailer", true)
			if err != nil || !plan.Applied {
				t.Fatalf("Remove: %+v, %v", plan, err)
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newOAuthFixture(t)
			f.installApproved(t)
			f.login(t)
			namespace := filepath.Join(f.env.authDir, "mailer")
			if _, err := os.Stat(filepath.Join(namespace, "token.json")); err != nil {
				t.Fatalf("token file missing after login: %v", err)
			}
			tc.act(t, f)
			if _, err := os.Stat(namespace); !os.IsNotExist(err) {
				t.Fatalf("namespace after %s: %v", tc.name, err)
			}
			if plan := f.planned(t); len(plan.Active) != 0 && tc.name == "logout" {
				t.Fatalf("plan after logout = %+v", plan)
			}
		})
	}
}

func TestRemoveWithoutApplyKeepsTheToken(t *testing.T) {
	f := newOAuthFixture(t)
	f.installApproved(t)
	f.login(t)
	plan, err := f.env.service.Remove(t.Context(), "mailer", false)
	if err != nil || plan.Applied {
		t.Fatalf("Remove: %+v, %v", plan, err)
	}
	if _, err := os.Stat(filepath.Join(f.env.authDir, "mailer", "token.json")); err != nil {
		t.Fatalf("token file after a dry run: %v", err)
	}
}

func TestStaticSecretPluginsRunBehindTheSameInterface(t *testing.T) {
	remote := `api_version: aura.plugin/v1
id: notes
version: 1.0.0
name: Notes
description: Notes.
tools:
  - name: search
    description: Search.
    tier: read
egress: [notes.example.com]
secrets:
  - name: token
    ref: env://NOTES_TOKEN
auth:
  kind: static_secret
  secret: token
  header: X-Notes-Key
backend:
  kind: mcp
  transport: streamable_http
  url: https://notes.example.com/mcp
`
	env := newTestEnv(t)
	env.approve("notes", env.install(remote, nil))
	plan, err := env.service.Plan(t.Context())
	if err != nil || len(plan.Active) != 0 || len(plan.Held) != 1 || plan.Held[0].Code != ErrAuthRequired {
		t.Fatalf("plan = %+v, %v; want the plugin held until its secret resolves", plan, err)
	}
	env.secrets.set("env://NOTES_TOKEN", "notes-secret-value")
	plan, err = env.service.Plan(t.Context())
	if err != nil || len(plan.Active) != 1 {
		t.Fatalf("plan = %+v, %v", plan, err)
	}
	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://notes.example.com/mcp", http.NoBody)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	if err := plan.Active[0].Auth.Authorize(t.Context(), request); err != nil {
		t.Fatalf("Authorize: %v", err)
	}
	if request.Header.Get("X-Notes-Key") != "notes-secret-value" {
		t.Fatalf("headers = %v", request.Header)
	}
	if !env.redactor.has("notes-secret-value") {
		t.Fatal("the static secret was not registered for redaction")
	}
}

func TestCheckerReportsARunningPluginWhoseAuthorizationWasLost(t *testing.T) {
	env := newTestEnv(t)
	checker := newCheckerForTest(t, env, &CheckerOptions{
		Runtime: func(context.Context) []RuntimeStatus {
			return []RuntimeStatus{{ID: "mailer", Running: true, Auth: ErrAuthFailed}, {ID: "calendar", Running: true}}
		},
	})
	findings := checker.Check(t.Context())
	if len(findings) != 1 || findings[0].ID != "plugin_auth_failed:mailer" || findings[0].Remediation != RemediationLogin || findings[0].Status != health.StatusDegraded {
		t.Fatalf("findings = %+v", findings)
	}
}
