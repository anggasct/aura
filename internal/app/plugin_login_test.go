package app

import (
	"bytes"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/anggasct/aura/internal/modules/plugin/auth/authtest"
)

const cliOAuthManifest = `api_version: aura.plugin/v1
id: mailer
version: 1.0.0
name: Mailer
description: Mail access.
capabilities: []
tools:
  - name: search
    description: Search mail.
    tier: read
egress: [AS_HOST, api.mail.example]
secrets:
  - name: client_id
    ref: env://AURA_TEST_OAUTH_CLIENT_ID
  - name: client_secret
    ref: env://AURA_TEST_OAUTH_CLIENT_SECRET
auth:
  kind: oauth2
  authorize_url: AS_AUTHORIZE
  token_url: AS_TOKEN
  scopes: [mail.read, mail.send]
  redirect_uri: https://aura.example/oauth/callback
  client_id_secret: client_id
  client_secret_secret: client_secret
backend:
  kind: mcp
  transport: streamable_http
  url: https://api.mail.example/mcp
`

type approvingInput struct {
	out  *bytes.Buffer
	as   *authtest.Server
	done bool
}

var authorizeLine = regexp.MustCompile(`(?m)^(https?://\S+/authorize\?\S+)$`)

func (a *approvingInput) Read(p []byte) (int, error) {
	if a.done {
		return 0, io.EOF
	}
	a.done = true
	match := authorizeLine.FindStringSubmatch(a.out.String())
	if match == nil {
		return 0, io.EOF
	}
	return copy(p, a.as.Approve(match[1])+"\n"), nil
}

func oauthManifestFor(as *authtest.Server) string {
	parsed, err := url.Parse(as.URL())
	if err != nil {
		panic(err)
	}
	text := strings.ReplaceAll(cliOAuthManifest, "AS_HOST", parsed.Host)
	text = strings.ReplaceAll(text, "AS_AUTHORIZE", as.AuthorizeURL())
	return strings.ReplaceAll(text, "AS_TOKEN", as.TokenURL())
}

func runPluginCommandIn(t *testing.T, cfg string, in io.Reader, out *bytes.Buffer, args ...string) error {
	t.Helper()
	cmd := newPluginCmd(&globalFlags{configPath: cfg})
	cmd.SetOut(out)
	cmd.SetErr(out)
	cmd.SetIn(in)
	cmd.SetArgs(args)
	return cmd.ExecuteContext(t.Context())
}

func installOAuthPlugin(t *testing.T, cfg string, as *authtest.Server, approve bool) {
	t.Helper()
	t.Setenv("AURA_TEST_OAUTH_CLIENT_ID", authtest.ClientID)
	t.Setenv("AURA_TEST_OAUTH_CLIENT_SECRET", authtest.Secret)
	if _, err := runPluginCommand(t, cfg, "install", writeCLIPluginBundle(t, oauthManifestFor(as))); err != nil {
		t.Fatalf("install: %v", err)
	}
	if !approve {
		return
	}
	review, err := runPluginCommand(t, cfg, "review", "mailer")
	if err != nil {
		t.Fatalf("review: %v", err)
	}
	if _, err := runPluginCommand(t, cfg, "approve", "mailer", "--digest", pluginDigestFromReview(t, review)[:16]); err != nil {
		t.Fatalf("approve: %v", err)
	}
}

func TestPluginReviewShowsTheOAuth2EndpointsAndScopes(t *testing.T) {
	cfg := writeChildrenCLIConfig(t)
	as := authtest.New(t)
	installOAuthPlugin(t, cfg, as, false)
	review, err := runPluginCommand(t, cfg, "review", "mailer")
	if err != nil {
		t.Fatalf("review: %v", err)
	}
	for _, want := range []string{
		"authorize " + as.AuthorizeURL(), "token " + as.TokenURL(), "scopes mail.read mail.send",
		"redirect https://aura.example/oauth/callback", "client id secret client_id", "client secret secret client_secret",
	} {
		if !strings.Contains(review, want) {
			t.Fatalf("review lacks %q: %q", want, review)
		}
	}
	if strings.Contains(review, authtest.ClientID) || strings.Contains(review, authtest.Secret) {
		t.Fatalf("review shows a credential value: %q", review)
	}
}

func TestPluginLoginAndLogoutLifecycle(t *testing.T) {
	cfg := writeChildrenCLIConfig(t)
	as := authtest.New(t)
	installOAuthPlugin(t, cfg, as, true)
	out := &bytes.Buffer{}
	if err := runPluginCommandIn(t, cfg, &approvingInput{out: out, as: as}, out, "login", "mailer", "--paste"); err != nil {
		t.Fatalf("login: %v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "logged in mailer") {
		t.Fatalf("login output = %q", out.String())
	}
	for _, leaked := range []string{as.AccessToken(1), as.RefreshToken(1), authtest.Secret} {
		if strings.Contains(out.String(), leaked) {
			t.Fatalf("login output leaks %q", leaked)
		}
	}
	namespace := filepath.Join(filepath.Dir(cfg), "plugin-secrets", "mailer")
	tokenFile := filepath.Join(namespace, "token.json")
	for path, want := range map[string]os.FileMode{filepath.Dir(namespace): 0o700, namespace: 0o700, tokenFile: 0o600} {
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != want {
			t.Fatalf("stat %s = %v, %v; want mode %o", filepath.Base(path), info, err, want)
		}
	}
	logout := &bytes.Buffer{}
	if err := runPluginCommandIn(t, cfg, strings.NewReader(""), logout, "logout", "mailer"); err != nil || !strings.Contains(logout.String(), "logged out mailer") {
		t.Fatalf("logout = %q, %v", logout.String(), err)
	}
	if accessEventCount(t, cfg, accessActionPluginLogin) != 1 || accessEventCount(t, cfg, accessActionPluginLogout) != 1 {
		t.Fatalf("login events = %d logout events = %d, want one each", accessEventCount(t, cfg, accessActionPluginLogin), accessEventCount(t, cfg, accessActionPluginLogout))
	}
	if _, err := os.Stat(namespace); !os.IsNotExist(err) {
		t.Fatalf("namespace after logout: %v", err)
	}
}

func TestPluginLoginRefusesAPluginThatIsNotActive(t *testing.T) {
	cfg := writeChildrenCLIConfig(t)
	as := authtest.New(t)
	installOAuthPlugin(t, cfg, as, false)
	out := &bytes.Buffer{}
	err := runPluginCommandIn(t, cfg, strings.NewReader(""), out, "login", "mailer", "--paste")
	if err == nil || !strings.Contains(err.Error(), "plugin_surface_pending") {
		t.Fatalf("login error = %v", err)
	}
	if strings.Contains(out.String(), "/authorize") {
		t.Fatalf("a refused login printed an authorize url: %q", out.String())
	}
	if _, statErr := os.Stat(filepath.Join(filepath.Dir(cfg), "plugin-secrets", "mailer")); !os.IsNotExist(statErr) {
		t.Fatalf("a refused login stored something: %v", statErr)
	}
}

func TestPluginLoginRejectsAWrongStateAndStoresNothing(t *testing.T) {
	cfg := writeChildrenCLIConfig(t)
	as := authtest.New(t)
	installOAuthPlugin(t, cfg, as, true)
	out := &bytes.Buffer{}
	forged := strings.NewReader("https://aura.example/oauth/callback?code=code-1&state=forged\n")
	err := runPluginCommandIn(t, cfg, forged, out, "login", "mailer", "--paste")
	if err == nil || !strings.Contains(err.Error(), "plugin_login_failed") {
		t.Fatalf("login error = %v", err)
	}
	if as.Exchanges() != 0 {
		t.Fatal("a forged callback reached the token endpoint")
	}
	if _, statErr := os.Stat(filepath.Join(filepath.Dir(cfg), "plugin-secrets", "mailer")); !os.IsNotExist(statErr) {
		t.Fatalf("a failed login stored something: %v", statErr)
	}
}

func TestPluginRemoveApplyErasesTheTokenNamespace(t *testing.T) {
	cfg := writeChildrenCLIConfig(t)
	as := authtest.New(t)
	installOAuthPlugin(t, cfg, as, true)
	out := &bytes.Buffer{}
	if err := runPluginCommandIn(t, cfg, &approvingInput{out: out, as: as}, out, "login", "mailer", "--paste"); err != nil {
		t.Fatalf("login: %v", err)
	}
	namespace := filepath.Join(filepath.Dir(cfg), "plugin-secrets", "mailer")
	if _, err := runPluginCommand(t, cfg, "remove", "mailer"); err != nil {
		t.Fatalf("remove dry run: %v", err)
	}
	if _, err := os.Stat(namespace); err != nil {
		t.Fatalf("a dry run removed the token: %v", err)
	}
	if _, err := runPluginCommand(t, cfg, "remove", "mailer", "--apply"); err != nil {
		t.Fatalf("remove --apply: %v", err)
	}
	if _, err := os.Stat(namespace); !os.IsNotExist(err) {
		t.Fatalf("namespace after remove --apply: %v", err)
	}
}

func TestPluginLoginOnAPluginWithoutAuthNeedsNoLogin(t *testing.T) {
	cfg := writeChildrenCLIConfig(t)
	if _, err := runPluginCommand(t, cfg, "install", writeCLIPluginBundle(t, cliPluginManifest)); err != nil {
		t.Fatalf("install: %v", err)
	}
	review, err := runPluginCommand(t, cfg, "review", "demo")
	if err != nil {
		t.Fatalf("review: %v", err)
	}
	if _, err := runPluginCommand(t, cfg, "approve", "demo", "--digest", pluginDigestFromReview(t, review)[:16]); err != nil {
		t.Fatalf("approve: %v", err)
	}
	err = runPluginCommandIn(t, cfg, strings.NewReader(""), &bytes.Buffer{}, "login", "demo", "--paste")
	if err == nil || !strings.Contains(err.Error(), "plugin_invalid_argument") {
		t.Fatalf("login error = %v", err)
	}
}
