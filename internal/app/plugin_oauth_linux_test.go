package app

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/modules/mcp"
	"github.com/anggasct/aura/internal/modules/plugin"
	"github.com/anggasct/aura/internal/modules/plugin/auth"
	"github.com/anggasct/aura/internal/modules/plugin/auth/authtest"
	"github.com/anggasct/aura/internal/platform/approval"
	"github.com/anggasct/aura/internal/platform/health"
	"github.com/anggasct/aura/internal/platform/principal"
	runtimeadk "github.com/anggasct/aura/internal/platform/turn/adk"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

type echoInput struct {
	Message string `json:"message"`
}

type echoOutput struct {
	Reply string `json:"reply"`
}

type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

type testClock struct {
	mu     sync.Mutex
	offset time.Duration
}

func (c *testClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return time.Now().Add(c.offset)
}

func (c *testClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.offset += d
}

type resourceServer struct {
	server      *httptest.Server
	bearers     sync.Map
	wrongBearer atomic.Int64
	unpersisted atomic.Int64
	requests    atomic.Int64
}

func newResourceServer(t *testing.T, as *authtest.Server, tokenFile func() string) *resourceServer {
	t.Helper()
	resource := &resourceServer{}
	mcpServer := sdk.NewServer(&sdk.Implementation{Name: "mail", Version: "1.0.0"}, nil)
	sdk.AddTool(mcpServer, &sdk.Tool{Name: "search", Description: "searches mail"}, func(_ context.Context, _ *sdk.CallToolRequest, in echoInput) (*sdk.CallToolResult, echoOutput, error) {
		return nil, echoOutput{Reply: in.Message}, nil
	})
	handler := sdk.NewStreamableHTTPHandler(func(*http.Request) *sdk.Server { return mcpServer }, nil)
	mux := http.NewServeMux()
	mux.HandleFunc("/mcp", func(w http.ResponseWriter, r *http.Request) {
		resource.requests.Add(1)
		bearer := r.Header.Get("Authorization")
		resource.bearers.Store(bearer, true)
		if bearer != "Bearer "+as.AccessToken(as.Issued()) {
			resource.wrongBearer.Add(1)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if data, err := os.ReadFile(tokenFile()); err != nil || !strings.Contains(string(data), strings.TrimPrefix(bearer, "Bearer ")) || !strings.Contains(string(data), as.RefreshToken(as.Issued())) {
			resource.unpersisted.Add(1)
		}
		handler.ServeHTTP(w, r)
	})
	resource.server = httptest.NewServer(mux)
	t.Cleanup(resource.server.Close)
	return resource
}

func (r *resourceServer) saw(bearer string) bool {
	_, ok := r.bearers.Load(bearer)
	return ok
}

const e2eOAuthManifest = `api_version: aura.plugin/v1
id: mailer
version: 1.0.0
name: Mailer
description: Mail access.
capabilities: []
tools:
  - name: search
    description: Search mail.
    tier: read
egress: [AS_HOST, RS_HOST]
secrets:
  - name: client_id
    ref: env://AURA_TEST_OAUTH_CLIENT_ID
  - name: client_secret
    ref: env://AURA_TEST_OAUTH_CLIENT_SECRET
auth:
  kind: oauth2
  authorize_url: AS_AUTHORIZE
  token_url: AS_TOKEN
  scopes: [mail.read]
  redirect_uri: https://aura.example/oauth/callback
  client_id_secret: client_id
  client_secret_secret: client_secret
backend:
  kind: mcp
  transport: streamable_http
  url: RS_URL/mcp
`

type e2eFixture struct {
	h        *mcpHarness
	as       *authtest.Server
	resource *resourceServer
	clock    *testClock
	logs     *lockedBuffer
	service  *plugin.Service
}

func newE2EFixture(t *testing.T) *e2eFixture {
	t.Helper()
	t.Setenv("AURA_TEST_OAUTH_CLIENT_ID", authtest.ClientID)
	t.Setenv("AURA_TEST_OAUTH_CLIENT_SECRET", authtest.Secret)
	h := newMCPHarness(t, nil)
	as := authtest.New(t)
	dataRoot, err := resolveDataRoot(&h.cfg)
	if err != nil {
		t.Fatalf("resolveDataRoot: %v", err)
	}
	secretsDir := filepath.Join(dataRoot, "plugin-secrets")
	resource := newResourceServer(t, as, func() string { return filepath.Join(secretsDir, "mailer", "token.json") })
	asURL, err := url.Parse(as.URL())
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	rsURL, err := url.Parse(resource.server.URL)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	manifest := strings.NewReplacer("AS_HOST", asURL.Host, "RS_HOST", rsURL.Host, "AS_AUTHORIZE", as.AuthorizeURL(), "AS_TOKEN", as.TokenURL(), "RS_URL", resource.server.URL).Replace(e2eOAuthManifest)
	fixture := &e2eFixture{h: h, as: as, resource: resource, clock: &testClock{}, logs: &lockedBuffer{}}
	fixture.service = fixture.newService(t)
	installed, err := fixture.service.Install(t.Context(), writeCLIPluginBundle(t, manifest))
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	if _, err := fixture.service.Approve(t.Context(), installed.ID, installed.Digest[:16]); err != nil {
		t.Fatalf("Approve: %v", err)
	}
	return fixture
}

func (f *e2eFixture) newService(t *testing.T) *plugin.Service {
	t.Helper()
	dataRoot, err := resolveDataRoot(&f.h.cfg)
	if err != nil {
		t.Fatalf("resolveDataRoot: %v", err)
	}
	registry, err := auth.NewRegistry(&auth.Options{
		Dir: filepath.Join(dataRoot, "plugin-secrets"), Secrets: auth.NewSecretResolver(), HTTPClient: auth.NewHTTPClient(nil),
		Redactor: f.h.executor.Broker(), Now: f.clock.now,
	})
	if err != nil {
		t.Fatalf("auth.NewRegistry: %v", err)
	}
	store, err := plugin.NewSQLiteStore(f.h.db)
	if err != nil {
		t.Fatalf("plugin.NewSQLiteStore: %v", err)
	}
	service, err := plugin.NewService(&plugin.Options{
		Registry: store, Auth: registry, Dir: filepath.Join(dataRoot, "plugins"),
	})
	if err != nil {
		t.Fatalf("plugin.NewService: %v", err)
	}
	return service
}

func (f *e2eFixture) login(t *testing.T) {
	t.Helper()
	out := &bytes.Buffer{}
	input := &approvingInput{out: out, as: f.as}
	opts := &auth.LoginOptions{Paste: true, Input: input, Prompt: func(authorizeURL string) {
		out.WriteString(authorizeURL + "\n")
	}}
	if err := f.service.Login(t.Context(), "mailer", opts); err != nil {
		t.Fatalf("Login: %v", err)
	}
}

func (f *e2eFixture) start(t *testing.T) (*plugin.Plan, *mcp.Module) {
	t.Helper()
	plan, err := f.service.Plan(t.Context())
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	mcpPlugins := mcpPluginsFromBackends(plan.Active)
	if len(mcpPlugins) == 0 {
		return plan, nil
	}
	mod, err := mcp.New(t.Context(), &mcp.ModuleDeps{
		Config:     &mcp.Config{},
		Plugins:    mcpPlugins,
		Broker:     f.h.executor.Broker(),
		HTTPClient: &http.Client{},
		Logger:     slog.New(slog.NewTextHandler(f.logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
	})
	if err != nil {
		t.Fatalf("mcp.New: %v", err)
	}
	t.Cleanup(func() { _ = mod.Close(context.WithoutCancel(t.Context())) })
	return plan, mod
}

func (f *e2eFixture) checkFindings(t *testing.T, plan *plugin.Plan, mcpMod *mcp.Module) []health.Finding {
	t.Helper()
	store, err := plugin.NewSQLiteStore(f.h.db)
	if err != nil {
		t.Fatalf("plugin.NewSQLiteStore: %v", err)
	}
	var source func(context.Context) []plugin.RuntimeStatus
	if mcpMod != nil {
		source = func(ctx context.Context) []plugin.RuntimeStatus {
			statuses := mcpPluginStatuses(mcpMod)
			for i := range statuses {
				for j := range plan.Active {
					if plan.Active[j].ID == statuses[i].ID {
						statuses[i].Auth = plan.Active[j].AuthStatus(ctx)
					}
				}
			}
			return statuses
		}
	}
	checker, err := plugin.NewChecker(&plugin.CheckerOptions{
		Registry: store,
		Desired:  f.h.cfg.Plugins,
		Held:     plan.Held,
		Runtime:  source,
	})
	if err != nil {
		t.Fatalf("NewChecker: %v", err)
	}
	return checker.Check(t.Context())
}

func (f *e2eFixture) search(t *testing.T, id, message string) (json.RawMessage, error) {
	t.Helper()
	args, err := json.Marshal(echoInput{Message: message})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	res, err := f.h.executor.Execute(t.Context(), &runtimeadk.BuiltinToolRequest{
		RequestID: id, TurnID: "turn-" + id, SessionID: "session-1", PrincipalID: "owner-1", PrincipalClass: principal.ClassOwner,
		ToolName: "plugin_mailer_search", ToolVersion: "v1", Arguments: args,
		Trust: string(approval.TrustDerivedUntrusted), IdempotencyKey: "key-" + id,
		EventInvocation: "inv-" + id, EventBranch: "main", EventAuthor: "agent",
	})
	return res.Output, err
}

func requireNoPlanted(t *testing.T, label, text string, extra ...string) {
	t.Helper()
	for _, planted := range append([]string{authtest.PlantedBody}, extra...) {
		if strings.Contains(text, planted) {
			t.Fatalf("%s leaks %q: %s", label, planted, text)
		}
	}
}

func TestActiveOAuth2PluginWithoutATokenStaysOffWithAuthRequired(t *testing.T) {
	f := newE2EFixture(t)
	plan, manager := f.start(t)
	if len(plan.Active) != 0 || len(plan.Held) != 1 || plan.Held[0].Code != plugin.ErrAuthRequired {
		t.Fatalf("plan = %+v", plan)
	}
	if manager != nil && len(manager.PluginStatuses()) != 0 {
		t.Fatalf("a plugin without a token reached the mcp manager: %+v", manager.PluginStatuses())
	}
	findings := f.checkFindings(t, plan, nil)
	if len(findings) != 1 || findings[0].Code != string(plugin.ErrAuthRequired) || findings[0].Remediation != plugin.RemediationLogin {
		t.Fatalf("findings = %+v", findings)
	}
	if f.resource.requests.Load() != 0 {
		t.Fatal("the resource server was contacted without a token")
	}
}

func TestOAuth2PluginCallsCarryTheBearerAndRefreshIsPersistedBeforeUse(t *testing.T) {
	f := newE2EFixture(t)
	f.as.ExpireIn(120)
	f.login(t)
	f.as.ExpireIn(3600)
	f.clock.advance(100 * time.Second)
	_, manager := f.start(t)
	if statuses := manager.PluginStatuses(); len(statuses) != 1 || statuses[0].Code != "" {
		t.Fatalf("statuses = %+v", statuses)
	}
	if f.as.Refreshes() != 1 {
		t.Fatalf("refreshes = %d, want the expired login token refreshed once", f.as.Refreshes())
	}
	output, err := f.search(t, "first", "hello")
	if err != nil || !strings.Contains(string(output), "hello") {
		t.Fatalf("search = %s, %v", output, err)
	}
	if f.resource.unpersisted.Load() != 0 || f.resource.wrongBearer.Load() != 0 {
		t.Fatalf("unpersisted = %d, wrong bearer = %d", f.resource.unpersisted.Load(), f.resource.wrongBearer.Load())
	}
	if !f.resource.saw("Bearer " + f.as.AccessToken(2)) {
		t.Fatal("the resource server never saw the rotated bearer")
	}
	if f.resource.saw("Bearer " + f.as.AccessToken(1)) {
		t.Fatal("the expired bearer was sent")
	}
}

func TestOAuth2PluginOutputNeverCarriesTokensOrClientCredentials(t *testing.T) {
	f := newE2EFixture(t)
	f.login(t)
	f.start(t)
	for i, secretValue := range []string{f.as.AccessToken(1), f.as.RefreshToken(1), authtest.ClientID, authtest.Secret} {
		output, err := f.search(t, "echo-"+strconv.Itoa(i), "value "+secretValue)
		if err != nil {
			t.Fatalf("search: %v", err)
		}
		if strings.Contains(string(output), secretValue) || !strings.Contains(string(output), "[REDACTED]") {
			t.Fatalf("output = %s, want %q redacted", output, secretValue)
		}
	}
	requireNoPlanted(t, "logs", f.logs.String(), f.as.AccessToken(1), f.as.RefreshToken(1), authtest.Secret)
}

func TestPermanentRefreshFailureFailsClosedAndSurvivesARestart(t *testing.T) {
	f := newE2EFixture(t)
	f.as.ExpireIn(120)
	f.login(t)
	plan, manager := f.start(t)
	if statuses := manager.PluginStatuses(); len(statuses) != 1 || statuses[0].Code != "" {
		t.Fatalf("statuses = %+v", statuses)
	}
	if _, err := f.search(t, "before", "ok"); err != nil {
		t.Fatalf("search before the failure: %v", err)
	}
	f.as.ExpireIn(3600)
	f.clock.advance(100 * time.Second)
	f.as.QueueInvalidGrant()
	refreshes := f.as.Refreshes()
	_, err := f.search(t, "after", "ok")
	if err == nil {
		t.Fatal("a call succeeded after the refresh token was rejected")
	}
	requireNoPlanted(t, "tool error", err.Error(), f.as.RefreshToken(1))
	if _, err := f.search(t, "again", "ok"); err == nil {
		t.Fatal("a call succeeded for a plugin whose authorization failed")
	}
	if f.as.Refreshes() != refreshes+1 {
		t.Fatalf("refreshes = %d, want the failed plugin to stop calling the token endpoint", f.as.Refreshes())
	}
	findings := f.checkFindings(t, plan, manager)
	codes := findingCodes(findings)
	if codes["mailer"] != string(plugin.ErrAuthFailed) {
		t.Fatalf("findings = %+v", findings)
	}
	rendered, err := json.Marshal(findings)
	if err != nil {
		t.Fatalf("marshal findings: %v", err)
	}
	requireNoPlanted(t, "health findings", string(rendered), f.as.RefreshToken(1))
	requireNoPlanted(t, "logs", f.logs.String(), f.as.RefreshToken(1), f.as.AccessToken(1))
	restarted, _ := f.start(t)
	if len(restarted.Active) != 0 || len(restarted.Held) != 1 || restarted.Held[0].Code != plugin.ErrAuthFailed {
		t.Fatalf("restarted plan = %+v", restarted)
	}
	if got := findingCodes(f.checkFindings(t, restarted, nil)); got["mailer"] != string(plugin.ErrAuthFailed) {
		t.Fatalf("restarted findings = %v", got)
	}
}

func TestTransientRefreshFailureFailsOnlyThatCall(t *testing.T) {
	f := newE2EFixture(t)
	f.as.ExpireIn(120)
	f.login(t)
	plan, manager := f.start(t)
	if _, err := f.search(t, "before", "ok"); err != nil {
		t.Fatalf("search before the outage: %v", err)
	}
	f.as.ExpireIn(3600)
	f.clock.advance(100 * time.Second)
	f.as.QueueOutage()
	_, err := f.search(t, "during", "ok")
	if err == nil {
		t.Fatal("a call succeeded during the token endpoint outage")
	}
	requireNoPlanted(t, "tool error", err.Error())
	if got := plan.Active[0].AuthStatus(t.Context()); got != "" {
		t.Fatalf("auth status after an outage = %q, want none", got)
	}
	if output, err := f.search(t, "after", "recovered"); err != nil || !strings.Contains(string(output), "recovered") {
		t.Fatalf("search after the outage = %s, %v", output, err)
	}
	statuses := manager.PluginStatuses()
	if len(statuses) != 1 || statuses[0].Code != "" {
		t.Fatalf("statuses = %+v", statuses)
	}
	requireNoPlanted(t, "logs", f.logs.String())
}
