package cli

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

	"github.com/anggasct/aura/internal/config"
	"github.com/anggasct/aura/internal/modules/mcp"
	"github.com/anggasct/aura/internal/modules/plugin"
	"github.com/anggasct/aura/internal/modules/plugin/auth"
	"github.com/anggasct/aura/internal/modules/plugin/auth/authtest"
	"github.com/anggasct/aura/internal/platform/approval"
	"github.com/anggasct/aura/internal/platform/health"
	"github.com/anggasct/aura/internal/platform/principal"
	runtimeadk "github.com/anggasct/aura/internal/platform/turn/adk"
	"github.com/anggasct/aura/internal/store"
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
	secretsDir, err := pluginDirectory(&h.cfg, pluginSecretsDirName)
	if err != nil {
		t.Fatalf("pluginDirectory: %v", err)
	}
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
	secretsDir, err := pluginDirectory(&f.h.cfg, pluginSecretsDirName)
	if err != nil {
		t.Fatalf("pluginDirectory: %v", err)
	}
	registry, err := auth.NewRegistry(&auth.Options{
		Dir: secretsDir, Secrets: mcpSecretResolver{}, HTTPClient: pluginHTTPClient(mcpHTTPClient(nil)),
		Redactor: f.h.executor.Broker(), Now: f.clock.now,
	})
	if err != nil {
		t.Fatalf("auth.NewRegistry: %v", err)
	}
	pluginsDir, err := pluginDirectory(&f.h.cfg, pluginDirName)
	if err != nil {
		t.Fatalf("pluginDirectory: %v", err)
	}
	service, err := plugin.NewService(&plugin.Options{
		Registry: &storePluginRegistry{plugins: store.NewPluginStore(f.h.db)}, Auth: registry, Dir: pluginsDir,
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

func (f *e2eFixture) start(t *testing.T) (*pluginRuntime, *mcp.Manager) {
	t.Helper()
	plan, err := f.service.Plan(t.Context())
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	runtime := &pluginRuntime{service: f.service, plan: plan, installed: 1}
	manager, err := mcp.NewManager(&mcp.ManagerOptions{
		Config:         &config.MCP{},
		Plugins:        runtime.mcpPlugins(),
		Broker:         f.h.executor.Broker(),
		HTTPClient:     &http.Client{},
		EndpointPolicy: mcpEndpointPolicy{},
		Logger:         slog.New(slog.NewTextHandler(f.logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
	})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	t.Cleanup(func() { _ = manager.Close(context.WithoutCancel(t.Context())) })
	_ = manager.Start(t.Context())
	return runtime, manager
}

func (f *e2eFixture) search(t *testing.T, id, message string) (json.RawMessage, error) {
	t.Helper()
	args, err := json.Marshal(echoInput{Message: message})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	f.h.mu.Lock()
	f.h.sequence++
	sequence := f.h.sequence
	f.h.mu.Unlock()
	return f.h.executor.Execute(t.Context(), &runtimeadk.BuiltinToolRequest{
		RequestID: id, TurnID: "turn-" + id, SessionID: "session-1", PrincipalID: "owner-1", PrincipalClass: principal.ClassOwner,
		ToolName: "plugin_mailer_search", ToolVersion: "v1", Arguments: args,
		Trust: string(approval.TrustDerivedUntrusted), IdempotencyKey: "key-" + id,
		EventSequence: sequence, EventInvocation: "inv-" + id, EventBranch: "main", EventAuthor: "agent",
	})
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
	runtime, manager := f.start(t)
	if len(runtime.plan.Active) != 0 || len(runtime.plan.Held) != 1 || runtime.plan.Held[0].Code != plugin.ErrAuthRequired {
		t.Fatalf("plan = %+v", runtime.plan)
	}
	if len(manager.PluginStatuses()) != 0 {
		t.Fatalf("a plugin without a token reached the mcp manager: %+v", manager.PluginStatuses())
	}
	check, ok, err := runtime.healthCheck(&f.h.cfg, f.h.db, nil)
	if err != nil || !ok {
		t.Fatalf("healthCheck = %v, %v", ok, err)
	}
	findings := check.Checker.Check(t.Context())
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
	runtime, manager := f.start(t)
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
	check, ok, err := runtime.healthCheck(&f.h.cfg, f.h.db, &mcpRuntime{manager: manager})
	if err != nil || !ok {
		t.Fatalf("healthCheck = %v, %v", ok, err)
	}
	findings := check.Checker.Check(t.Context())
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
	if len(restarted.plan.Active) != 0 || len(restarted.plan.Held) != 1 || restarted.plan.Held[0].Code != plugin.ErrAuthFailed {
		t.Fatalf("restarted plan = %+v", restarted.plan)
	}
	if got := findingCodes(mustFindings(t, restarted, f)); got["mailer"] != string(plugin.ErrAuthFailed) {
		t.Fatalf("restarted findings = %v", got)
	}
}

func mustFindings(t *testing.T, runtime *pluginRuntime, f *e2eFixture) []health.Finding {
	t.Helper()
	check, ok, err := runtime.healthCheck(&f.h.cfg, f.h.db, nil)
	if err != nil || !ok {
		t.Fatalf("healthCheck = %v, %v", ok, err)
	}
	return check.Checker.Check(t.Context())
}

func TestTransientRefreshFailureFailsOnlyThatCall(t *testing.T) {
	f := newE2EFixture(t)
	f.as.ExpireIn(120)
	f.login(t)
	runtime, manager := f.start(t)
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
	if got := runtime.plan.Active[0].AuthStatus(t.Context()); got != "" {
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
