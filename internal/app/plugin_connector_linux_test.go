package app

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/anggasct/aura/internal/modules/mcp"
	"github.com/anggasct/aura/internal/modules/plugin"
	"github.com/anggasct/aura/internal/modules/plugin/auth"
	"github.com/anggasct/aura/internal/modules/plugin/auth/authtest"
	"github.com/anggasct/aura/internal/modules/plugin/connector"
	"github.com/anggasct/aura/internal/platform/approval"
	"github.com/anggasct/aura/internal/platform/principal"
	"github.com/anggasct/aura/internal/platform/toolbroker"
	runtimeadk "github.com/anggasct/aura/internal/platform/turn/adk"
)

const connectorNoAuthManifest = `api_version: aura.plugin/v1
id: mailer
version: 1.0.0
name: Mailer
description: Mail access.
capabilities: []
tools:
  - name: search
    description: Search mail.
    tier: read
  - name: send
    description: Send mail.
    tier: effect
egress: [API_HOST]
secrets: []
auth:
  kind: none
backend:
  kind: connector
  command: ./bin/connector
  startup_timeout: 30s
  request_timeout: 30s
`

const connectorOAuthManifest = `api_version: aura.plugin/v1
id: mailer
version: 1.0.0
name: Mailer
description: Mail access.
capabilities: []
tools:
  - name: search
    description: Search mail.
    tier: read
  - name: send
    description: Send mail.
    tier: effect
egress: [AS_HOST, API_HOST]
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
  hosts: [API_HOST]
backend:
  kind: connector
  command: ./bin/connector
  startup_timeout: 30s
  request_timeout: 30s
`

func buildTestConnector(t *testing.T) string {
	t.Helper()
	out := filepath.Join(t.TempDir(), "connector")
	goBinary, err := exec.LookPath("go")
	if err != nil {
		t.Fatalf("find the go toolchain: %v", err)
	}
	build := exec.CommandContext(t.Context(), goBinary, "build", "-o", out, "github.com/anggasct/aura/internal/modules/plugin/connector/testdata/testconnector")
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build the test connector: %v\n%s", err, output)
	}
	return out
}

func goTestConnector(t *testing.T) []byte {
	t.Helper()
	binary, err := os.ReadFile(buildTestConnector(t))
	if err != nil {
		t.Fatalf("read the test connector: %v", err)
	}
	return binary
}

func writeConnectorPluginBundle(t *testing.T, manifest string, executable []byte) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "plugin.yaml"), []byte(manifest), 0o600); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	if err := os.Mkdir(filepath.Join(dir, "bin"), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "bin", "connector"), executable, 0o700); err != nil {
		t.Fatalf("write connector: %v", err)
	}
	return dir
}

type processSession struct {
	cmd   *exec.Cmd
	stdin io.WriteCloser
	lines chan []byte
	done  chan struct{}

	mu   sync.Mutex
	sent [][]byte
	once sync.Once
}

func (s *processSession) Request(ctx context.Context, payload []byte) ([]byte, error) {
	s.mu.Lock()
	s.sent = append(s.sent, bytes.Clone(payload))
	s.mu.Unlock()
	if _, err := s.stdin.Write(append(bytes.Clone(payload), '\n')); err != nil {
		return nil, errors.New("connector process is gone")
	}
	select {
	case line, open := <-s.lines:
		if !open {
			return nil, errors.New("connector process exited")
		}
		return line, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (s *processSession) Close(context.Context) error {
	s.once.Do(func() {
		close(s.done)
		_ = s.stdin.Close()
		_ = s.cmd.Process.Kill()
		_ = s.cmd.Wait()
	})
	return nil
}

func (s *processSession) payloads() [][]byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.sent)
}

type processStarter struct {
	mu       sync.Mutex
	failure  error
	sessions []*processSession
	requests []mcp.ContainedSessionRequest
}

func (p *processStarter) StartSession(ctx context.Context, req *mcp.ContainedSessionRequest) (mcp.ContainedSession, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.requests = append(p.requests, *req)
	if p.failure != nil {
		return nil, p.failure
	}
	cmd := exec.CommandContext(ctx, req.Executable, req.Arguments...)
	cmd.Dir = req.WorkingDir
	cmd.Env = []string{}
	for key, value := range req.Environment {
		cmd.Env = append(cmd.Env, key+"="+value)
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	session := &processSession{cmd: cmd, stdin: stdin, lines: make(chan []byte), done: make(chan struct{})}
	go func() {
		defer close(session.lines)
		reader := bufio.NewReaderSize(stdout, 4<<20)
		for {
			line, err := reader.ReadBytes('\n')
			if err != nil {
				return
			}
			select {
			case session.lines <- bytes.TrimSuffix(line, []byte("\n")):
			case <-session.done:
				return
			}
		}
	}()
	p.sessions = append(p.sessions, session)
	return session, nil
}

func (p *processStarter) allPayloads() [][]byte {
	p.mu.Lock()
	sessions := slices.Clone(p.sessions)
	p.mu.Unlock()
	var all [][]byte
	for _, session := range sessions {
		all = append(all, session.payloads()...)
	}
	return all
}

type recordedCall struct {
	Method        string
	Path          string
	Query         string
	Authorization string
	Cookie        string
}

type connectorAPI struct {
	server  *httptest.Server
	mu      sync.Mutex
	calls   []recordedCall
	handler func(w http.ResponseWriter, r *http.Request)
}

func newConnectorAPI(t *testing.T) *connectorAPI {
	t.Helper()
	api := &connectorAPI{handler: func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"messages":["m1"]}`))
	}}
	api.server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		api.mu.Lock()
		api.calls = append(api.calls, recordedCall{
			Method: r.Method, Path: r.URL.Path, Query: r.URL.RawQuery, Authorization: r.Header.Get("Authorization"), Cookie: r.Header.Get("Cookie"),
		})
		handler := api.handler
		api.mu.Unlock()
		handler(w, r)
	}))
	t.Cleanup(api.server.Close)
	return api
}

func (a *connectorAPI) host() string {
	return strings.TrimPrefix(a.server.URL, "https://")
}

func (a *connectorAPI) recorded() []recordedCall {
	a.mu.Lock()
	defer a.mu.Unlock()
	return slices.Clone(a.calls)
}

type connectorFixture struct {
	h       *mcpHarness
	api     *connectorAPI
	service *plugin.Service
	runtime *pluginRuntime
	manager *connector.Manager
	starter *processStarter
}

func newConnectorFixture(t *testing.T, manifest string, prepare func(f *connectorFixture)) *connectorFixture {
	t.Helper()
	return newConnectorFixtureWith(t, manifest, goTestConnector(t), prepare)
}

func newConnectorFixtureWith(t *testing.T, manifest string, executable []byte, prepare func(f *connectorFixture)) *connectorFixture {
	t.Helper()
	f := &connectorFixture{h: newMCPHarness(t, nil), api: newConnectorAPI(t), starter: &processStarter{}}
	service, err := newPluginService(&f.h.cfg, f.h.db, f.h.executor.Broker())
	if err != nil {
		t.Fatalf("newPluginService: %v", err)
	}
	f.service = service
	installed, err := service.Install(t.Context(), writeConnectorPluginBundle(t, strings.ReplaceAll(manifest, "API_HOST", f.api.host()), executable))
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	if _, err := service.Approve(t.Context(), installed.ID, installed.Digest[:16]); err != nil {
		t.Fatalf("Approve: %v", err)
	}
	if prepare != nil {
		prepare(f)
	}
	return f
}

func (f *connectorFixture) start(t *testing.T) {
	t.Helper()
	plan, err := f.service.Plan(t.Context())
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	f.runtime = &pluginRuntime{service: f.service, plan: plan, installed: 1}
	f.manager, err = startConnectorPlugins(t.Context(), &connectorRuntimeOptions{
		Broker: f.h.executor.Broker(), Logger: slog.New(slog.DiscardHandler), Starter: f.starter,
		HTTPClient: f.api.server.Client(), Plugins: f.runtime.connectorPlugins(),
	})
	if err != nil {
		t.Fatalf("startConnectorPlugins: %v", err)
	}
	f.runtime.connectors = f.manager
	t.Cleanup(func() { _ = f.manager.Close(context.WithoutCancel(t.Context())) })
}

func (f *connectorFixture) call(t *testing.T, tool, id string, class principal.Class, arguments map[string]any) (json.RawMessage, error) {
	t.Helper()
	raw, err := json.Marshal(arguments)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return f.h.executor.Execute(t.Context(), &runtimeadk.BuiltinToolRequest{
		RequestID: id, TurnID: "turn-" + id, SessionID: "session-1", PrincipalID: "owner-1", PrincipalClass: class,
		ToolName: "plugin_mailer_" + tool, ToolVersion: "v1", Arguments: raw,
		Trust: string(approval.TrustDerivedUntrusted), IdempotencyKey: "key-" + id,
		EventInvocation: "inv-" + id, EventBranch: "main", EventAuthor: "agent",
	})
}

func countEffectsIn(t *testing.T, db *sql.DB, state string) int {
	t.Helper()
	var count int
	if err := db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM effect_intent WHERE state = ?`, state).Scan(&count); err != nil {
		t.Fatalf("count intents: %v", err)
	}
	return count
}

func outputOf(t *testing.T, output json.RawMessage) map[string]any {
	t.Helper()
	var decoded map[string]any
	if err := json.Unmarshal(output, &decoded); err != nil {
		t.Fatalf("output %s: %v", output, err)
	}
	return decoded
}

func connectorCode(err error, want string) bool {
	return err != nil && strings.Contains(err.Error(), want)
}

func TestConnectorPluginRunsEndToEndThroughTheBrokerWithoutAuth(t *testing.T) {
	f := newConnectorFixture(t, connectorNoAuthManifest, nil)
	f.start(t)
	names := f.h.toolNames()
	if !slices.Contains(names, "plugin_mailer_search") || !slices.Contains(names, "plugin_mailer_send") || slices.Contains(names, "plugin_mailer_export_all") {
		t.Fatalf("tools = %v", names)
	}
	statuses := f.manager.Statuses()
	if len(statuses) != 1 || statuses[0].Code != "" || !slices.Equal(statuses[0].Drift, []string{"export_all"}) {
		t.Fatalf("statuses = %+v", statuses)
	}
	check, ok, err := f.runtime.healthCheck(&f.h.cfg, f.h.db, nil)
	if err != nil || !ok {
		t.Fatalf("healthCheck = %v, %v", ok, err)
	}
	if codes := findingCodes(check.Checker.Check(t.Context())); codes["mailer"] != string(plugin.ErrToolDrift) {
		t.Fatalf("findings = %v", codes)
	}

	output, err := f.call(t, "search", "search-1", principal.ClassOwner, map[string]any{
		"url": f.api.server.URL + "/v1/messages?q=hi", "headers": map[string]string{"Authorization": "Bearer evil", "Cookie": "a=b"},
	})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	decoded := outputOf(t, output)
	if decoded["status"] != float64(200) || !strings.Contains(string(output), "m1") {
		t.Fatalf("output = %s", output)
	}
	calls := f.api.recorded()
	if len(calls) != 1 || calls[0].Method != http.MethodGet || calls[0].Path != "/v1/messages" || calls[0].Query != "q=hi" || calls[0].Authorization != "" || calls[0].Cookie != "" {
		t.Fatalf("api saw %+v", calls)
	}
	if len(f.h.promptedTools()) != 0 {
		t.Fatalf("a read tool asked for approval: %v", f.h.promptedTools())
	}

	env, err := f.call(t, "search", "env-1", principal.ClassOwner, map[string]any{"mode": "env"})
	if err != nil {
		t.Fatalf("env: %v", err)
	}
	var seen struct {
		Env  []string `json:"env"`
		Args []string `json:"args"`
	}
	if err := json.Unmarshal(env, &seen); err != nil || len(seen.Env) != 0 || len(seen.Args) != 0 {
		t.Fatalf("connector process environment = %s (%v)", env, err)
	}
	pluginsDir, err := pluginDirectory(&f.h.cfg, pluginDirName)
	if err != nil {
		t.Fatalf("pluginDirectory: %v", err)
	}
	f.starter.mu.Lock()
	for _, request := range f.starter.requests {
		if len(request.Environment) != 0 || request.WorkingDir != filepath.Join(pluginsDir, "mailer", "1.0.0") {
			t.Errorf("session request = %+v", request)
		}
	}
	f.starter.mu.Unlock()

	for _, tool := range []string{"search", "send"} {
		_, err := f.call(t, tool, "public-"+tool, principal.ClassPublic, map[string]any{"url": f.api.server.URL + "/x"})
		if !connectorCode(err, string(toolbroker.ResultToolTierDenied)) {
			t.Fatalf("public %s = %v, want tool_tier_denied", tool, err)
		}
	}
	if len(f.api.recorded()) != 1 {
		t.Fatalf("a refused call reached the api: %+v", f.api.recorded())
	}
}

func TestConnectorPluginEffectsAreApprovedJournaledAndNeverResent(t *testing.T) {
	f := newConnectorFixture(t, connectorNoAuthManifest, nil)
	f.start(t)
	post := map[string]any{"url": f.api.server.URL + "/v1/send", "body": `{"to":"x"}`}

	if _, err := f.call(t, "send", "send-1", principal.ClassOwner, post); err != nil {
		t.Fatalf("send: %v", err)
	}
	if prompts := f.h.promptedTools(); !slices.Equal(prompts, []string{"plugin_mailer_send"}) {
		t.Fatalf("approval prompts = %v", prompts)
	}
	if countEffectsIn(t, f.h.db, "succeeded") != 1 {
		t.Fatal("the send was not journaled as succeeded")
	}

	f.api.mu.Lock()
	f.api.handler = func(w http.ResponseWriter, _ *http.Request) {
		hijacker, ok := w.(http.Hijacker)
		if !ok {
			return
		}
		if conn, _, err := hijacker.Hijack(); err == nil {
			_ = conn.Close()
		}
	}
	f.api.mu.Unlock()
	before := len(f.api.recorded())
	_, err := f.call(t, "send", "send-2", principal.ClassOwner, post)
	if !connectorCode(err, "plugin_request_failed") {
		t.Fatalf("send over a dropped connection = %v, want plugin_request_failed", err)
	}
	if countEffectsIn(t, f.h.db, "unknown") != 1 || len(f.api.recorded()) != before+1 {
		t.Fatalf("unknown intents = %d, api requests %d -> %d", countEffectsIn(t, f.h.db, "unknown"), before, len(f.api.recorded()))
	}

}

func TestConnectorCrashFailsTheCallAndTheNextOneWaitsForTheRestartLimit(t *testing.T) {
	f := newConnectorFixture(t, connectorNoAuthManifest, nil)
	f.start(t)
	_, err := f.call(t, "send", "crash-1", principal.ClassOwner, map[string]any{"mode": "crash"})
	if !connectorCode(err, "plugin_backend_unavailable") {
		t.Fatalf("send to a crashing connector = %v, want plugin_backend_unavailable", err)
	}
	if countEffectsIn(t, f.h.db, "failed") != 1 || countEffectsIn(t, f.h.db, "unknown") != 0 {
		t.Fatalf("failed=%d unknown=%d", countEffectsIn(t, f.h.db, "failed"), countEffectsIn(t, f.h.db, "unknown"))
	}
	_, err = f.call(t, "search", "after-crash", principal.ClassOwner, map[string]any{"url": f.api.server.URL + "/x"})
	if !connectorCode(err, "plugin_backend_unavailable") {
		t.Fatalf("a call right after a crash = %v, want the restart limit", err)
	}
	f.starter.mu.Lock()
	defer f.starter.mu.Unlock()
	if len(f.starter.requests) != 1 {
		t.Fatalf("sessions started = %d, a restart inside the interval must not start one", len(f.starter.requests))
	}
}

func TestConnectorMessagesThatBreakTheContractCannotReachTheModel(t *testing.T) {
	cases := []struct {
		name string
		mode string
		want string
	}{
		{"not json", "malformed", "plugin_rpc_invalid"},
		{"free text in an error", "free_text", "plugin_rpc_invalid"},
		{"plain error code", "error", "plugin_connector_error: not_found"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newConnectorFixture(t, connectorNoAuthManifest, nil)
			f.start(t)
			_, err := f.call(t, "search", "bad-1", principal.ClassOwner, map[string]any{"mode": tc.mode})
			if !connectorCode(err, tc.want) || strings.Contains(err.Error(), "ignore previous") || strings.Contains(err.Error(), "this is not json") {
				t.Fatalf("error = %v, want %s without connector text", err, tc.want)
			}
		})
	}
}

func TestConnectorPluginThatCannotStartStaysOffWithBackendUnavailable(t *testing.T) {
	f := newConnectorFixture(t, connectorNoAuthManifest, func(f *connectorFixture) {
		f.starter.failure = errors.New("containment is unavailable")
	})
	plan, err := f.service.Plan(t.Context())
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	f.runtime = &pluginRuntime{service: f.service, plan: plan, installed: 1}
	f.manager, err = startConnectorPlugins(t.Context(), &connectorRuntimeOptions{
		Broker: f.h.executor.Broker(), Logger: slog.New(slog.DiscardHandler), Starter: f.starter, HTTPClient: f.api.server.Client(), Plugins: f.runtime.connectorPlugins(),
	})
	if err != nil {
		t.Fatalf("startConnectorPlugins: %v", err)
	}
	t.Cleanup(func() { _ = f.manager.Close(context.WithoutCancel(t.Context())) })
	f.runtime.connectors = f.manager
	if names := f.h.toolNames(); slices.Contains(names, "plugin_mailer_search") {
		t.Fatalf("tools of a plugin that stays off = %v", names)
	}
	check, ok, err := f.runtime.healthCheck(&f.h.cfg, f.h.db, nil)
	if err != nil || !ok {
		t.Fatalf("healthCheck = %v, %v", ok, err)
	}
	if codes := findingCodes(check.Checker.Check(t.Context())); codes["mailer"] != string(plugin.ErrBackendUnavailable) {
		t.Fatalf("findings = %v", codes)
	}
}

func TestConnectorPluginWithOAuth2CarriesTheBearerWithoutExposingIt(t *testing.T) {
	t.Setenv("AURA_TEST_OAUTH_CLIENT_ID", authtest.ClientID)
	t.Setenv("AURA_TEST_OAUTH_CLIENT_SECRET", authtest.Secret)
	as := authtest.New(t)
	asURL, err := url.Parse(as.URL())
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	manifest := strings.NewReplacer("AS_HOST", asURL.Host, "AS_AUTHORIZE", as.AuthorizeURL(), "AS_TOKEN", as.TokenURL()).Replace(connectorOAuthManifest)
	f := newConnectorFixture(t, manifest, nil)

	plan, err := f.service.Plan(t.Context())
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if len(plan.Active) != 0 || len(plan.Held) != 1 || plan.Held[0].Code != plugin.ErrAuthRequired {
		t.Fatalf("plan before login = %+v", plan)
	}

	out := &bytes.Buffer{}
	opts := &auth.LoginOptions{Paste: true, Input: &approvingInput{out: out, as: as}, Prompt: func(authorizeURL string) { out.WriteString(authorizeURL + "\n") }}
	if err := f.service.Login(t.Context(), "mailer", opts); err != nil {
		t.Fatalf("Login: %v", err)
	}
	f.api.mu.Lock()
	f.api.handler = func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+as.AccessToken(as.Issued()) {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"messages":["m1"]}`))
	}
	f.api.mu.Unlock()
	f.start(t)

	output, err := f.call(t, "search", "oauth-1", principal.ClassOwner, map[string]any{
		"url": f.api.server.URL + "/v1/messages?q=hi", "headers": map[string]string{"Authorization": "Bearer evil"},
	})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if outputOf(t, output)["status"] != float64(200) {
		t.Fatalf("output = %s: the bearer did not reach the api", output)
	}
	calls := f.api.recorded()
	if len(calls) != 1 || calls[0].Authorization != "Bearer "+as.AccessToken(1) {
		t.Fatalf("api saw %+v", calls)
	}
	env, err := f.call(t, "search", "oauth-env", principal.ClassOwner, map[string]any{"mode": "env"})
	if err != nil {
		t.Fatalf("env: %v", err)
	}
	secrets := []string{as.AccessToken(1), as.RefreshToken(1), authtest.ClientID, authtest.Secret}
	for _, secret := range secrets {
		if strings.Contains(string(env), secret) || strings.Contains(string(output), secret) {
			t.Fatalf("a credential reached the tool output")
		}
		for _, payload := range f.starter.allPayloads() {
			if bytes.Contains(payload, []byte(secret)) {
				t.Fatalf("a message to the connector carries a credential: %s", payload)
			}
		}
	}
	f.starter.mu.Lock()
	defer f.starter.mu.Unlock()
	for _, request := range f.starter.requests {
		if len(request.Environment) != 0 || len(request.Arguments) != 0 {
			t.Fatalf("session request = %+v", request)
		}
	}
}
