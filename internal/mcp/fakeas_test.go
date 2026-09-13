package mcp

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

type fakeAuthorizationServer struct {
	t               *testing.T
	mux             *http.ServeMux
	server          *httptest.Server
	mu              sync.Mutex
	challenges      map[string]string
	codes           map[string]string
	refreshTokens   map[string]bool
	rotations       int
	tokenHits       atomic.Int64
	verifierChecked atomic.Bool
	requirePKCE     bool
	failRefresh     bool
	accessToken     string
	refreshToken    string
}

func (f *fakeAuthorizationServer) currentAccess() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.accessToken
}

func (f *fakeAuthorizationServer) attachMCPServer(t *testing.T, path string) {
	t.Helper()
	server := sdk.NewServer(&sdk.Implementation{Name: "oauth-mcp-server", Version: "1.0.0"}, nil)
	sdk.AddTool(server, &sdk.Tool{Name: "echo", Description: "echoes message"}, func(_ context.Context, _ *sdk.CallToolRequest, in EchoInput) (*sdk.CallToolResult, EchoOutput, error) {
		return nil, EchoOutput{Reply: in.Message}, nil
	})
	handler := sdk.NewStreamableHTTPHandler(func(_ *http.Request) *sdk.Server { return server }, nil)
	f.mux.Handle(path, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+f.currentAccess() {
			w.Header().Set("Www-Authenticate", `Bearer resource_metadata="`+f.server.URL+`/.well-known/oauth-protected-resource"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		handler.ServeHTTP(w, r)
	}))
}

func newFakeAuthorizationServer(t *testing.T) *fakeAuthorizationServer {
	t.Helper()
	fake := &fakeAuthorizationServer{
		t:             t,
		mux:           http.NewServeMux(),
		challenges:    make(map[string]string),
		codes:         make(map[string]string),
		refreshTokens: make(map[string]bool),
		accessToken:   "fake-access-1",
		refreshToken:  "fake-refresh-1",
		requirePKCE:   true,
	}
	fake.mux.HandleFunc("/.well-known/oauth-protected-resource", fake.serveProtectedResource)
	fake.mux.HandleFunc("/.well-known/oauth-authorization-server/as", fake.serveAuthorizationMetadata)
	fake.mux.HandleFunc("/as/.well-known/oauth-authorization-server", fake.serveAuthorizationMetadata)
	fake.mux.HandleFunc("/as/.well-known/openid-configuration", fake.serveAuthorizationMetadata)
	fake.mux.HandleFunc("/as/authorize", fake.serveAuthorize)
	fake.mux.HandleFunc("/as/token", fake.serveToken)
	fake.server = httptest.NewServer(fake.mux)
	t.Cleanup(fake.server.Close)
	fake.refreshTokens[fake.refreshToken] = true
	return fake
}

func (f *fakeAuthorizationServer) writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	raw, err := json.Marshal(value)
	if err != nil {
		f.t.Fatalf("marshal fake response: %v", err)
	}
	if _, err := w.Write(raw); err != nil {
		f.t.Fatalf("write fake response: %v", err)
	}
}

func (f *fakeAuthorizationServer) serveProtectedResource(w http.ResponseWriter, r *http.Request) {
	resource := "http://" + r.Host
	if r.URL.Path != "/.well-known/oauth-protected-resource" {
		resource = f.server.URL + "/mcp"
	}
	f.writeJSON(w, http.StatusOK, map[string]any{
		"resource":              resource,
		"authorization_servers": []string{f.server.URL + "/as"},
	})
}

func (f *fakeAuthorizationServer) serveAuthorizationMetadata(w http.ResponseWriter, _ *http.Request) {
	f.writeJSON(w, http.StatusOK, map[string]any{
		"issuer":                           f.server.URL + "/as",
		"authorization_endpoint":           f.server.URL + "/as/authorize",
		"token_endpoint":                   f.server.URL + "/as/token",
		"response_types_supported":         []string{"code"},
		"grant_types_supported":            []string{"authorization_code", "refresh_token"},
		"code_challenge_methods_supported": []string{"S256"},
	})
}

func (f *fakeAuthorizationServer) serveAuthorize(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	challenge := query.Get("code_challenge")
	if f.requirePKCE && (challenge == "" || query.Get("code_challenge_method") != "S256") {
		f.writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid_request"})
		return
	}
	code := "auth-code-" + strings.ReplaceAll(query.Get("state"), " ", "-")
	f.mu.Lock()
	f.challenges[code] = challenge
	f.mu.Unlock()
	redirect := query.Get("redirect_uri") + "?code=" + url.QueryEscape(code) + "&state=" + url.QueryEscape(query.Get("state"))
	w.Header().Set("Location", redirect)
	w.WriteHeader(http.StatusFound)
}

func (f *fakeAuthorizationServer) checkVerifier(code, verifier string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	challenge, ok := f.challenges[code]
	if !ok {
		return false
	}
	sum := sha256.Sum256([]byte(verifier))
	computed := base64.RawURLEncoding.EncodeToString(sum[:])
	if computed == challenge {
		f.verifierChecked.Store(true)
		return true
	}
	return false
}

func (f *fakeAuthorizationServer) serveToken(w http.ResponseWriter, r *http.Request) {
	f.tokenHits.Add(1)
	if err := r.ParseForm(); err != nil {
		f.writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid_request"})
		return
	}
	switch r.Form.Get("grant_type") {
	case "authorization_code":
		code := r.Form.Get("code")
		if !f.checkVerifier(code, r.Form.Get("code_verifier")) {
			f.writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid_grant"})
			return
		}
		f.writeJSON(w, http.StatusOK, map[string]any{
			"access_token": f.accessToken, "refresh_token": f.refreshToken,
			"token_type": "Bearer", "expires_in": 3600,
		})
	case "refresh_token":
		if f.failRefresh {
			f.writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid_grant"})
			return
		}
		f.mu.Lock()
		refresh := r.Form.Get("refresh_token")
		if !f.refreshTokens[refresh] {
			f.mu.Unlock()
			f.writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid_grant"})
			return
		}
		delete(f.refreshTokens, refresh)
		f.rotations++
		rotated := "fake-refresh-rotated"
		if f.rotations > 1 {
			rotated = "fake-refresh-rotated-again"
		}
		f.refreshTokens[rotated] = true
		f.accessToken = "fake-access-rotated"
		f.mu.Unlock()
		f.writeJSON(w, http.StatusOK, map[string]any{
			"access_token": f.accessToken, "refresh_token": rotated,
			"token_type": "Bearer", "expires_in": 3600,
		})
	default:
		f.writeJSON(w, http.StatusBadRequest, map[string]any{"error": "unsupported_grant_type"})
	}
}

func codeFromRedirect(t *testing.T, location string) string {
	t.Helper()
	parsed, err := url.Parse(location)
	if err != nil {
		t.Fatalf("parse redirect: %v", err)
	}
	code := parsed.Query().Get("code")
	if code == "" {
		t.Fatalf("no code in redirect %q", location)
	}
	return code
}

func noRedirectClient(t *testing.T) *http.Client {
	t.Helper()
	return &http.Client{CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}
}
