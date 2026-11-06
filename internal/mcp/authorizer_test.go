package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/config"
)

const plantedProviderText = "PLANTED-PROVIDER-TEXT-91c2"

type stubAuthorizer struct {
	mu    sync.Mutex
	token string
	err   error
	calls int
}

func (s *stubAuthorizer) Authorize(_ context.Context, req *http.Request) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	if s.err != nil {
		return s.err
	}
	req.Header.Set("Authorization", "Bearer "+s.token)
	return nil
}

func (s *stubAuthorizer) setToken(token string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.token = token
}

func (s *stubAuthorizer) setError(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.err = err
}

func (s *stubAuthorizer) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

func pluginHTTPConfig(name, serverURL string) *config.MCPServer {
	return &config.MCPServer{
		Name: name, Transport: config.MCPTransportStreamableHTTP, URL: serverURL,
		RequestTimeout: config.Duration(10 * time.Second),
		ConnectTimeout: config.Duration(10 * time.Second),
		StartupTimeout: config.Duration(10 * time.Second),
		MaxMessageSize: 1 << 20,
	}
}

func TestAuthorizerTransportSignsOnlyTheBackendOrigin(t *testing.T) {
	var mu sync.Mutex
	seen := map[string]string{}
	record := func(name string, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		seen[name] = r.Header.Get("Authorization")
	}
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		record("other", r)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(other.Close)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/redirect" {
			http.Redirect(w, r, other.URL+"/landing", http.StatusFound)
			return
		}
		record("backend", r)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(backend.Close)
	origin, err := url.Parse(backend.URL)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	authorizer := &stubAuthorizer{token: "secret-bearer"}
	client := &http.Client{Transport: &authorizerTransport{next: http.DefaultTransport, authorizer: authorizer, origin: origin}}
	for _, target := range []string{backend.URL + "/mcp", backend.URL + "/redirect"} {
		request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, target, http.NoBody)
		if err != nil {
			t.Fatalf("NewRequest: %v", err)
		}
		resp, err := client.Do(request)
		if err != nil {
			t.Fatalf("Do(%s): %v", target, err)
		}
		_ = resp.Body.Close()
	}
	mu.Lock()
	defer mu.Unlock()
	if seen["backend"] != "Bearer secret-bearer" {
		t.Fatalf("backend Authorization = %q", seen["backend"])
	}
	if got, reached := seen["other"]; !reached || got != "" {
		t.Fatalf("redirect target reached = %v with Authorization %q, want it reached without a bearer", reached, got)
	}
	if authorizer.callCount() != 2 {
		t.Fatalf("authorizer calls = %d, want only the two backend requests", authorizer.callCount())
	}
}

func TestAuthorizerTransportDoesNotMutateTheCallersRequest(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))
	t.Cleanup(backend.Close)
	origin, err := url.Parse(backend.URL)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	transport := &authorizerTransport{next: http.DefaultTransport, authorizer: &stubAuthorizer{token: "x"}, origin: origin}
	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, backend.URL, http.NoBody)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	resp, err := transport.RoundTrip(request)
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	_ = resp.Body.Close()
	if request.Header.Get("Authorization") != "" {
		t.Fatal("the transport wrote the bearer into the caller's request")
	}
	if resp, err := transport.RoundTrip(nil); err == nil {
		_ = resp.Body.Close()
		t.Fatal("a nil request was accepted")
	}
}

func TestClientCarriesTheAuthorizerBearerAndFollowsRotation(t *testing.T) {
	ctx := t.Context()
	var current struct {
		sync.Mutex
		token string
	}
	current.token = "plugin-token-1"
	httpServer := authedStreamableServer(t, func() string {
		current.Lock()
		defer current.Unlock()
		return current.token
	}, func() string { return "ok" })
	authorizer := &stubAuthorizer{token: "plugin-token-1"}
	client, err := NewClient(pluginHTTPConfig("plugin-mailer", httpServer.URL+"/mcp"), nil,
		WithHTTPClient(&http.Client{}),
		WithEndpointPolicy(stubEndpointPolicy{}),
		WithRequestAuthorizer(authorizer),
	)
	if err != nil {
		t.Fatalf("NewClient(): %v", err)
	}
	defer func() { _ = client.Close(t.Context()) }()
	if err := client.Connect(ctx, nil); err != nil {
		t.Fatalf("Connect(): %v", err)
	}
	args, err := json.Marshal(EchoInput{Message: "hi"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if _, err := client.CallTool(ctx, "echo", args); err != nil {
		t.Fatalf("CallTool(): %v", err)
	}
	current.Lock()
	current.token = "plugin-token-2"
	current.Unlock()
	authorizer.setToken("plugin-token-2")
	if _, err := client.CallTool(ctx, "echo", args); err != nil {
		t.Fatalf("CallTool after rotation: %v", err)
	}
	if authorizer.callCount() < 3 {
		t.Fatalf("authorizer calls = %d, want one per request", authorizer.callCount())
	}
}

func TestClientRefusesAnAuthorizerNextToConfiguredAuth(t *testing.T) {
	cfg := staticStreamableConfig("plugin-mailer", "http://127.0.0.1:1/mcp")
	client, err := NewClient(cfg, nil,
		WithHTTPClient(&http.Client{}),
		WithEndpointPolicy(stubEndpointPolicy{}),
		WithSecretResolver(&stubSecretResolver{values: map[string]string{"env://MCP_STATIC_TOKEN": "x"}}),
		WithRequestAuthorizer(&stubAuthorizer{token: "x"}),
	)
	if err != nil {
		t.Fatalf("NewClient(): %v", err)
	}
	defer func() { _ = client.Close(t.Context()) }()
	err = client.Connect(t.Context(), nil)
	if code, ok := CodeOf(err); !ok || code != ErrConfigInvalid {
		t.Fatalf("Connect error = %v", err)
	}
}

func TestAuthorizerFailureKeepsTheRequestOffTheWire(t *testing.T) {
	var hits int
	var mu sync.Mutex
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		hits++
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)
	failure := errors.New("plugin_auth_failed: the authorization was rejected and needs a new login")
	client, err := NewClient(pluginHTTPConfig("plugin-mailer", server.URL+"/mcp"), nil,
		WithHTTPClient(&http.Client{}),
		WithEndpointPolicy(stubEndpointPolicy{}),
		WithRequestAuthorizer(&stubAuthorizer{err: failure}),
	)
	if err != nil {
		t.Fatalf("NewClient(): %v", err)
	}
	defer func() { _ = client.Close(t.Context()) }()
	err = client.Connect(t.Context(), nil)
	if err == nil {
		t.Fatal("Connect succeeded with a failing authorizer")
	}
	if strings.Contains(err.Error(), "Bearer") {
		t.Fatalf("error mentions a bearer: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if hits != 0 {
		t.Fatalf("server hits = %d, want 0", hits)
	}
}

func TestManagerRunsAPluginThroughItsAuthorizer(t *testing.T) {
	httpServer := authedStreamableServer(t, func() string { return "plugin-token" }, func() string { return "ok" })
	approvedEcho := []PluginTool{{Name: "echo", Description: "approved echo"}}
	cases := []struct {
		name       string
		authorizer *stubAuthorizer
		wantTools  []string
		wantCode   ErrorCode
	}{
		{"valid token", &stubAuthorizer{token: "plugin-token"}, []string{"plugin_mailer_echo"}, ""},
		{"rejected token", &stubAuthorizer{token: "wrong-token"}, nil, ErrServerUnavailable},
		{"failing authorizer", &stubAuthorizer{err: errors.New("plugin_auth_failed: needs a new login " + plantedProviderText)}, nil, ErrServerUnavailable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var prompts int
			broker := countingBroker(t, &prompts)
			server := *pluginHTTPConfig("mailer", httpServer.URL+"/mcp")
			server.Capabilities = nil
			mgr, err := NewManager(&ManagerOptions{
				Config:         &config.MCP{},
				Broker:         broker,
				TrustRegistry:  NewMemoryTrustRegistry(),
				HTTPClient:     &http.Client{},
				EndpointPolicy: stubEndpointPolicy{},
				Plugins:        []PluginServer{{ID: "mailer", Server: server, Authorizer: tc.authorizer, Tools: approvedEcho}},
			})
			if err != nil {
				t.Fatalf("NewManager(): %v", err)
			}
			t.Cleanup(func() { _ = mgr.Close(context.WithoutCancel(t.Context())) })
			_ = mgr.Start(t.Context())
			statuses := mgr.PluginStatuses()
			if len(statuses) != 1 || statuses[0].Code != tc.wantCode {
				t.Fatalf("statuses = %+v, want code %q", statuses, tc.wantCode)
			}
			if names := pluginToolNames(broker); len(names) != len(tc.wantTools) || (len(names) == 1 && names[0] != tc.wantTools[0]) {
				t.Fatalf("plugin tools = %v, want %v", names, tc.wantTools)
			}
		})
	}
}

func TestAuthorizerFailureOnACallKeepsTheSession(t *testing.T) {
	ctx := t.Context()
	httpServer := authedStreamableServer(t, func() string { return "plugin-token" }, func() string { return "ok" })
	authorizer := &stubAuthorizer{token: "plugin-token"}
	client, err := NewClient(pluginHTTPConfig("plugin-mailer", httpServer.URL+"/mcp"), nil,
		WithHTTPClient(&http.Client{}),
		WithEndpointPolicy(stubEndpointPolicy{}),
		WithRequestAuthorizer(authorizer),
	)
	if err != nil {
		t.Fatalf("NewClient(): %v", err)
	}
	defer func() { _ = client.Close(t.Context()) }()
	if err := client.Connect(ctx, nil); err != nil {
		t.Fatalf("Connect(): %v", err)
	}
	args, err := json.Marshal(EchoInput{Message: "hi"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	authorizer.setError(errors.New("plugin_auth_unavailable: the token endpoint is unavailable"))
	_, err = client.CallTool(ctx, "echo", args)
	if code, ok := CodeOf(err); !ok || code != ErrAuthRequired {
		t.Fatalf("CallTool error = %v, want %s", err, ErrAuthRequired)
	}
	if !client.Connected() {
		t.Fatal("an authorization failure dropped the session")
	}
	authorizer.setError(nil)
	if _, err := client.CallTool(ctx, "echo", args); err != nil {
		t.Fatalf("CallTool after recovery: %v", err)
	}
}
