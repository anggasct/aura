package mcp

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/config"
	sdkauth "github.com/modelcontextprotocol/go-sdk/auth"
)

type stubSecretResolver struct {
	values map[string]string
	calls  int
}

func (s *stubSecretResolver) ResolveSecret(_ context.Context, ref string) (string, error) {
	s.calls++
	if value, ok := s.values[ref]; ok {
		return value, nil
	}
	return "", Errorf(ErrAuthRequired, "credential is unavailable")
}

func oauthTestProvider(t *testing.T, fake *fakeAuthorizationServer, tokens OAuthTokenStore) *OAuthProvider {
	t.Helper()
	provider, err := NewOAuthProvider(
		"oauth-server", fake.server.URL+"/mcp", "test-client-id", "test-client-secret",
		fakeHTTPClient(), tokens, codeFetcher(t),
	)
	if err != nil {
		t.Fatalf("NewOAuthProvider(): %v", err)
	}
	return provider
}

func fakeHTTPClient() *http.Client {
	return &http.Client{}
}

func codeFetcher(t *testing.T) sdkauth.AuthorizationCodeFetcher {
	return func(ctx context.Context, args *sdkauth.AuthorizationArgs) (*sdkauth.AuthorizationResult, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, args.URL, http.NoBody)
		if err != nil {
			t.Fatalf("authorize request: %v", err)
		}
		resp, err := noRedirectClient(t).Do(req)
		if err != nil {
			t.Fatalf("authorize call: %v", err)
		}
		defer func() { _, _ = io.Copy(io.Discard, resp.Body); _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusFound {
			t.Fatalf("authorize status = %s, want redirect", resp.Status)
		}
		location, err := resp.Location()
		if err != nil {
			t.Fatalf("authorize location: %v", err)
		}
		return &sdkauth.AuthorizationResult{Code: codeFromRedirect(t, location.String()), State: location.Query().Get("state")}, nil
	}
}

func TestOAuthExchangePersistsTokensWithPKCE(t *testing.T) {
	fake := newFakeAuthorizationServer(t)
	tokens := NewMemoryTokenStore()
	provider := oauthTestProvider(t, fake, tokens)

	redirect := fake.server.URL + "/callback"
	if err := provider.ExchangeCode(t.Context(), redirect); err != nil {
		t.Fatalf("ExchangeCode(): %v", err)
	}
	if !fake.verifierChecked.Load() {
		t.Error("authorization server never verified the PKCE challenge")
	}
	record, found, err := tokens.Load()
	if err != nil || !found {
		t.Fatalf("Load() = %+v, %v, %v", record, found, err)
	}
	if record.AccessToken != "fake-access-1" || record.RefreshToken != "fake-refresh-1" {
		t.Errorf("record = %+v", record)
	}
	if record.Expiry.IsZero() || time.Until(record.Expiry) <= 0 {
		t.Errorf("expiry = %v, want future", record.Expiry)
	}
}

func TestOAuthRefreshRotatesAndPersists(t *testing.T) {
	fake := newFakeAuthorizationServer(t)
	tokens := NewMemoryTokenStore()
	if err := tokens.Store(TokenRecord{
		AccessToken: "stale-access", RefreshToken: "fake-refresh-1",
		TokenType: "Bearer", Expiry: time.Now().Add(-time.Hour),
	}); err != nil {
		t.Fatalf("Store(): %v", err)
	}
	provider := oauthTestProvider(t, fake, tokens)

	handler, err := provider.Handler(t.Context())
	if err != nil {
		t.Fatalf("Handler(): %v", err)
	}
	source, err := handler.TokenSource(t.Context())
	if err != nil {
		t.Fatalf("TokenSource(): %v", err)
	}
	token, err := source.Token()
	if err != nil {
		t.Fatalf("Token(): %v", err)
	}
	if token.AccessToken != "fake-access-rotated" {
		t.Errorf("access = %q, want rotated", token.AccessToken)
	}
	record, found, err := tokens.Load()
	if err != nil || !found {
		t.Fatalf("Load() = %+v, %v, %v", record, found, err)
	}
	if record.RefreshToken != "fake-refresh-rotated" {
		t.Errorf("refresh not rotated in store: %+v", record)
	}
}

func TestOAuthRefreshFailureDisablesServer(t *testing.T) {
	fake := newFakeAuthorizationServer(t)
	fake.failRefresh = true
	tokens := NewMemoryTokenStore()
	if err := tokens.Store(TokenRecord{
		AccessToken: "stale-access", RefreshToken: "fake-refresh-1",
		TokenType: "Bearer", Expiry: time.Now().Add(-time.Hour),
	}); err != nil {
		t.Fatalf("Store(): %v", err)
	}
	provider := oauthTestProvider(t, fake, tokens)

	handler, err := provider.Handler(t.Context())
	if err != nil {
		t.Fatalf("Handler(): %v", err)
	}
	source, err := handler.TokenSource(t.Context())
	if err != nil {
		t.Fatalf("TokenSource(): %v", err)
	}
	if _, err := source.Token(); err == nil {
		t.Fatal("refresh failure swallowed")
	}
	hitsAfterFirst := fake.tokenHits.Load()
	if _, err := source.Token(); err == nil {
		t.Fatal("disabled server retried refresh")
	}
	if fake.tokenHits.Load() != hitsAfterFirst {
		t.Error("disabled server hit the token endpoint again")
	}
}

func TestOAuthRequiresStoredToken(t *testing.T) {
	fake := newFakeAuthorizationServer(t)
	provider := oauthTestProvider(t, fake, NewMemoryTokenStore())
	if _, err := provider.Handler(t.Context()); err == nil {
		t.Fatal("missing token accepted")
	} else if code, ok := CodeOf(err); !ok || code != ErrAuthRequired {
		t.Errorf("code = %v, %v", code, ok)
	}
}

func oauthStreamableConfig(name, serverURL, tokenStore string) *config.MCPServer {
	return &config.MCPServer{
		Name: name, Transport: config.MCPTransportStreamableHTTP, URL: serverURL,
		Auth:           &config.MCPAuth{OAuth: &config.MCPOAuth{ClientIDEnv: "OAUTH_TEST_ID", ClientSecretEnv: "OAUTH_TEST_SECRET", TokenStore: tokenStore}},
		RequestTimeout: config.Duration(10 * time.Second),
		ConnectTimeout: config.Duration(10 * time.Second),
		StartupTimeout: config.Duration(10 * time.Second),
		MaxMessageSize: 1 << 20,
	}
}

func TestOAuthProvisionedConnect(t *testing.T) {
	ctx := t.Context()
	fake := newFakeAuthorizationServer(t)
	fake.attachMCPServer(t, "/mcp")
	t.Setenv("OAUTH_TEST_ID", "test-client-id")
	t.Setenv("OAUTH_TEST_SECRET", "test-client-secret")

	tokenFile := t.TempDir() + "/tokens.json"
	provider, err := NewOAuthProvider("oauth-prov", fake.server.URL+"/mcp", "test-client-id", "test-client-secret", fakeHTTPClient(), mustFileStore(t, tokenFile), codeFetcher(t))
	if err != nil {
		t.Fatalf("NewOAuthProvider(): %v", err)
	}
	if err := provider.ExchangeCode(ctx, fake.server.URL+"/callback"); err != nil {
		t.Fatalf("ExchangeCode(): %v", err)
	}

	client, err := NewClient(oauthStreamableConfig("oauth-prov", fake.server.URL+"/mcp", tokenFile), nil,
		WithHTTPClient(fakeHTTPClient()),
		WithEndpointPolicy(stubEndpointPolicy{}),
	)
	if err != nil {
		t.Fatalf("NewClient(): %v", err)
	}
	defer func() { _ = client.Close() }()
	if err := client.Connect(ctx, nil); err != nil {
		t.Fatalf("Connect(): %v", err)
	}
	tools, err := client.DiscoverTools(ctx)
	if err != nil {
		t.Fatalf("DiscoverTools(): %v", err)
	}
	if len(tools) != 1 || tools[0].Name != "echo" {
		t.Fatalf("tools = %+v", tools)
	}
	args, err := json.Marshal(EchoInput{Message: "hello oauth"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	res, err := client.CallTool(ctx, "echo", args)
	if err != nil {
		t.Fatalf("CallTool(): %v", err)
	}
	if res.IsError {
		t.Fatal("tool call returned error result")
	}
}

func TestOAuthAutoFlowOnChallenge(t *testing.T) {
	ctx := t.Context()
	fake := newFakeAuthorizationServer(t)
	fake.attachMCPServer(t, "/mcp")
	t.Setenv("OAUTH_TEST_ID", "test-client-id")
	t.Setenv("OAUTH_TEST_SECRET", "test-client-secret")

	tokenFile := t.TempDir() + "/tokens.json"
	seed := mustFileStore(t, tokenFile)
	if err := seed.Store(TokenRecord{AccessToken: "wrong-access", TokenType: "Bearer", Expiry: time.Now().Add(time.Hour)}); err != nil {
		t.Fatalf("Store(): %v", err)
	}
	serverCfg := oauthStreamableConfig("oauth-auto", fake.server.URL+"/mcp", tokenFile)
	client, err := NewClient(serverCfg, nil,
		WithHTTPClient(fakeHTTPClient()),
		WithEndpointPolicy(stubEndpointPolicy{}),
		WithOAuthCodeFetcher(codeFetcher(t)),
	)
	if err != nil {
		t.Fatalf("NewClient(): %v", err)
	}
	defer func() { _ = client.Close() }()
	if err := client.Connect(ctx, nil); err != nil {
		t.Fatalf("Connect(): %v", err)
	}
	tools, err := client.DiscoverTools(ctx)
	if err != nil {
		t.Fatalf("DiscoverTools(): %v", err)
	}
	if len(tools) != 1 || tools[0].Name != "echo" {
		t.Fatalf("tools = %+v", tools)
	}
	reloaded, found, err := seed.Load()
	if err != nil || !found {
		t.Fatalf("Load() = %+v, %v, %v", reloaded, found, err)
	}
	if reloaded.AccessToken != "fake-access-1" {
		t.Errorf("exchanged token not persisted: %+v", reloaded)
	}
}

func TestOAuthNoTokenFailsClosed(t *testing.T) {
	ctx := t.Context()
	fake := newFakeAuthorizationServer(t)
	fake.attachMCPServer(t, "/mcp")
	t.Setenv("OAUTH_TEST_ID", "test-client-id")
	t.Setenv("OAUTH_TEST_SECRET", "test-client-secret")

	client, err := NewClient(oauthStreamableConfig("oauth-empty", fake.server.URL+"/mcp", ""), nil,
		WithHTTPClient(fakeHTTPClient()),
		WithEndpointPolicy(stubEndpointPolicy{}),
	)
	if err != nil {
		t.Fatalf("NewClient(): %v", err)
	}
	defer func() { _ = client.Close() }()
	if err := client.Connect(ctx, nil); err == nil {
		t.Fatal("tokenless connect accepted")
	} else if code, ok := CodeOf(err); !ok || code != ErrAuthRequired {
		t.Fatalf("code = %v, %v", code, ok)
	}
	if hits := fake.tokenHits.Load(); hits != 0 {
		t.Errorf("token endpoint hit %d times without a stored token", hits)
	}
}

func mustFileStore(t *testing.T, path string) *FileTokenStore {
	t.Helper()
	store, err := NewFileTokenStore(path)
	if err != nil {
		t.Fatalf("NewFileTokenStore(): %v", err)
	}
	return store
}

func TestOAuthRedirectValidation(t *testing.T) {
	fake := newFakeAuthorizationServer(t)
	provider := oauthTestProvider(t, fake, NewMemoryTokenStore())
	for _, redirect := range []string{
		"http://example.com/callback",
		"https://example.com/callback?next=/x",
		"https://user@example.com/callback",
		"not-a-url%%",
	} {
		if err := provider.ExchangeCode(t.Context(), redirect); err == nil {
			t.Errorf("redirect %q accepted", redirect)
		}
	}
}

func TestStaticAuthHeaderModes(t *testing.T) {
	var seen http.Header
	backend := &stubRoundTripper{roundTrip: func(req *http.Request) (*http.Response, error) {
		seen = req.Header.Clone()
		return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody, Header: http.Header{}, Request: req}, nil
	}}
	resolver := &stubSecretResolver{values: map[string]string{"env://TOKEN": "secret-value"}}

	bearer := &staticAuthTransport{next: backend, ref: "env://TOKEN", serverName: "s", resolver: resolver}
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, "http://127.0.0.1/x", http.NoBody)
	resp, err := bearer.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip(): %v", err)
	}
	_ = resp.Body.Close()
	if seen.Get("Authorization") != "Bearer secret-value" {
		t.Errorf("authorization = %q", seen.Get("Authorization"))
	}

	custom := &staticAuthTransport{next: backend, header: "X-Api-Key", ref: "env://TOKEN", serverName: "s", resolver: resolver}
	resp, err = custom.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip(): %v", err)
	}
	_ = resp.Body.Close()
	if seen.Get("X-Api-Key") != "secret-value" || seen.Get("Authorization") != "" {
		t.Errorf("headers = %v", seen)
	}

	missing := &staticAuthTransport{next: backend, ref: "env://MISSING", serverName: "s", resolver: resolver}
	resp, err = missing.RoundTrip(req)
	if resp != nil {
		_ = resp.Body.Close()
	}
	if err == nil {
		t.Error("missing credential accepted")
	} else if code, ok := CodeOf(err); !ok || code != ErrAuthRequired {
		t.Errorf("code = %v, %v", code, ok)
	}
}

func TestAuthErrorsCarryNoSecrets(t *testing.T) {
	fake := newFakeAuthorizationServer(t)
	fake.failRefresh = true
	canaries := []string{"fake-access-1", "fake-refresh-1", "test-client-secret", "fake-access-rotated"}

	tokens := NewMemoryTokenStore()
	if err := tokens.Store(TokenRecord{
		AccessToken: "stale-access", RefreshToken: "fake-refresh-1",
		TokenType: "Bearer", Expiry: time.Now().Add(-time.Hour),
	}); err != nil {
		t.Fatalf("Store(): %v", err)
	}
	provider := oauthTestProvider(t, fake, tokens)
	handler, err := provider.Handler(t.Context())
	if err != nil {
		t.Fatalf("Handler(): %v", err)
	}
	source, err := handler.TokenSource(t.Context())
	if err != nil {
		t.Fatalf("TokenSource(): %v", err)
	}
	tokenErr := func() error { _, err := source.Token(); return err }()
	if tokenErr == nil {
		t.Fatal("refresh failure swallowed")
	}
	for _, canary := range canaries {
		if strings.Contains(tokenErr.Error(), canary) {
			t.Errorf("secret %q in error %q", canary, tokenErr.Error())
		}
	}
	if code, ok := CodeOf(tokenErr); !ok || code != ErrAuthRequired {
		t.Errorf("code = %v, %v", code, ok)
	}
}
