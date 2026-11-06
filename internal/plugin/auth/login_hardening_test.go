package auth

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	declaredIssuer = "https://issuer.example"
	twoScopes      = `{"access_token":"scoped-access","refresh_token":"scoped-refresh","token_type":"Bearer","expires_in":3600,%s}`
)

func loginDetail(err error) string {
	var coded *Error
	if errors.As(err, &coded) {
		return coded.Detail
	}
	return ""
}

func captureAuthorizeURL(h *harness, strategy Interactive) (raw string, err error) {
	input := &pastedInput{}
	err = strategy.Login(h.t.Context(), &LoginOptions{Paste: true, Input: input, Prompt: func(authorizeURL string) {
		raw = authorizeURL
		input.url = h.as.Approve(authorizeURL) + "\n"
	}})
	return raw, err
}

func TestAuthorizeParamsReachTheAuthorizeUrl(t *testing.T) {
	cases := []struct {
		name   string
		params map[string]string
	}{
		{"offline with consent", map[string]string{"access_type": "offline", "prompt": "consent"}},
		{"online only", map[string]string{"access_type": "online"}},
		{"select account", map[string]string{"prompt": "select_account"}},
		{"none declared", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			strategy := h.strategy(h.spec(func(spec *Spec) { spec.AuthorizeParams = tc.params }))
			raw, err := captureAuthorizeURL(h, strategy)
			if err != nil {
				t.Fatalf("Login: %v", err)
			}
			parsed, err := url.Parse(raw)
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			query := parsed.Query()
			for _, key := range []string{"access_type", "prompt"} {
				if got, want := query.Get(key), tc.params[key]; got != want {
					t.Errorf("%s = %q, want %q", key, got, want)
				}
			}
		})
	}
}

func TestAuthorizeParamsOutsideTheAllowListAreRejected(t *testing.T) {
	cases := []struct {
		name   string
		params map[string]string
	}{
		{"unknown key", map[string]string{"login_hint": "someone@example.test"}},
		{"redirect override", map[string]string{"redirect_uri": "https://evil.example/cb"}},
		{"state override", map[string]string{"state": "fixed"}},
		{"unknown access type", map[string]string{"access_type": "forever"}},
		{"unknown prompt", map[string]string{"prompt": "login"}},
		{"empty value", map[string]string{"prompt": ""}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			_, err := h.registry().Strategy(h.spec(func(spec *Spec) { spec.AuthorizeParams = tc.params }))
			requireCode(t, err, ErrInvalid)
		})
	}
}

func TestLoginRefusesWhatItCannotTrustAndStoresNothing(t *testing.T) {
	cases := []struct {
		name    string
		spec    func(spec *Spec)
		prepare func(h *harness)
		edit    func(callback string) string
		detail  string
	}{
		{
			name: "no refresh token",
			prepare: func(h *harness) {
				h.as.QueueTokenResponse(http.StatusOK, `{"access_token":"a","token_type":"Bearer","expires_in":60}`)
			},
			detail: "refresh token",
		},
		{
			name: "partial scope grant",
			spec: func(spec *Spec) { spec.Scopes = []string{"mail.read", "mail.send"} },
			prepare: func(h *harness) {
				h.as.QueueTokenResponse(http.StatusOK, strings.Replace(twoScopes, "%s", `"scope":"mail.read"`, 1))
			},
			detail: "scope",
		},
		{
			name: "empty scope grant",
			prepare: func(h *harness) {
				h.as.QueueTokenResponse(http.StatusOK, strings.Replace(twoScopes, "%s", `"scope":""`, 1))
			},
			detail: "scope",
		},
		{
			name: "scope that is not text",
			prepare: func(h *harness) {
				h.as.QueueTokenResponse(http.StatusOK, strings.Replace(twoScopes, "%s", `"scope":["mail.read"]`, 1))
			},
			detail: "scope",
		},
		{
			name:   "declared issuer missing from the callback",
			spec:   func(spec *Spec) { spec.Issuer = declaredIssuer },
			detail: "issuer",
		},
		{
			name:    "declared issuer differs",
			spec:    func(spec *Spec) { spec.Issuer = declaredIssuer },
			prepare: func(h *harness) { h.as.SetCallbackIssuers("https://other.example") },
			detail:  "issuer",
		},
		{
			name:    "declared issuer differs only in case",
			spec:    func(spec *Spec) { spec.Issuer = declaredIssuer },
			prepare: func(h *harness) { h.as.SetCallbackIssuers("https://ISSUER.example") },
			detail:  "issuer",
		},
		{
			name:    "declared issuer repeated",
			spec:    func(spec *Spec) { spec.Issuer = declaredIssuer },
			prepare: func(h *harness) { h.as.SetCallbackIssuers(declaredIssuer, declaredIssuer) },
			detail:  "issuer",
		},
		{
			name: "wrong issuer is judged before a provider error",
			spec: func(spec *Spec) { spec.Issuer = declaredIssuer },
			prepare: func(h *harness) {
				h.as.DenyAuthorization()
				h.as.SetCallbackIssuers("https://other.example")
			},
			detail: "issuer",
		},
		{
			name:    "wrong issuer is judged before the state",
			spec:    func(spec *Spec) { spec.Issuer = declaredIssuer },
			prepare: func(h *harness) { h.as.SetCallbackIssuers("https://other.example") },
			edit: func(callback string) string {
				parsed, _ := url.Parse(callback)
				query := parsed.Query()
				query.Set("state", "not-the-state-of-this-login")
				parsed.RawQuery = query.Encode()
				return parsed.String()
			},
			detail: "issuer",
		},
		{
			name:    "wrong issuer is judged before a missing code",
			spec:    func(spec *Spec) { spec.Issuer = declaredIssuer },
			prepare: func(h *harness) { h.as.SetCallbackIssuers("https://other.example") },
			edit: func(callback string) string {
				parsed, _ := url.Parse(callback)
				query := parsed.Query()
				query.Del("code")
				parsed.RawQuery = query.Encode()
				return parsed.String()
			},
			detail: "issuer",
		},
	}
	modes := []struct {
		name  string
		login func(h *harness, strategy Interactive, edit func(string) string) error
	}{
		{"loopback listener", func(h *harness, strategy Interactive, edit func(string) string) error {
			return h.loopbackLogin(strategy, edit)
		}},
		{"pasted redirect", func(h *harness, strategy Interactive, edit func(string) string) error {
			return h.pasteLogin(strategy, edit)
		}},
	}
	for _, mode := range modes {
		for _, tc := range cases {
			t.Run(mode.name+"/"+tc.name, func(t *testing.T) {
				h := newHarness(t)
				if tc.prepare != nil {
					tc.prepare(h)
				}
				strategy := h.strategy(h.spec(tc.spec))
				err := mode.login(h, strategy, tc.edit)
				requireCode(t, err, ErrLogin)
				requireNoLeak(t, err)
				if !strings.Contains(loginDetail(err), tc.detail) {
					t.Fatalf("error detail = %q, want it to mention %q", loginDetail(err), tc.detail)
				}
				requireNoTokenFile(t, h)
				requireCode(t, strategy.Ready(t.Context()), ErrRequired)
				if h.redactor.has("scoped-access") || h.redactor.has("scoped-refresh") {
					t.Fatal("a refused token was registered")
				}
			})
		}
	}
}

func TestLoginAcceptsWhatTheContractAllows(t *testing.T) {
	cases := []struct {
		name    string
		spec    func(spec *Spec)
		prepare func(h *harness)
	}{
		{
			name: "access only without a refresh token",
			spec: func(spec *Spec) { spec.AccessOnly = true },
			prepare: func(h *harness) {
				h.as.QueueTokenResponse(http.StatusOK, `{"access_token":"a","token_type":"Bearer","expires_in":3600}`)
			},
		},
		{
			name:    "matching issuer",
			spec:    func(spec *Spec) { spec.Issuer = declaredIssuer },
			prepare: func(h *harness) { h.as.SetCallbackIssuers(declaredIssuer) },
		},
		{
			name:    "issuer sent but not declared",
			prepare: func(h *harness) { h.as.SetCallbackIssuers("https://anything.example") },
		},
		{
			name: "scope superset",
			spec: func(spec *Spec) { spec.Scopes = []string{"mail.read", "mail.send"} },
			prepare: func(h *harness) {
				h.as.QueueTokenResponse(http.StatusOK, strings.Replace(twoScopes, "%s", `"scope":"mail.send extra mail.read"`, 1))
			},
		},
		{
			name: "no scope field means the request was granted",
			spec: func(spec *Spec) { spec.Scopes = []string{"mail.read", "mail.send"} },
		},
	}
	modes := []struct {
		name  string
		login func(h *harness, strategy Interactive) error
	}{
		{"loopback listener", func(h *harness, strategy Interactive) error { return h.loopbackLogin(strategy, nil) }},
		{"pasted redirect", func(h *harness, strategy Interactive) error { return h.pasteLogin(strategy, nil) }},
	}
	for _, mode := range modes {
		for _, tc := range cases {
			t.Run(mode.name+"/"+tc.name, func(t *testing.T) {
				h := newHarness(t)
				if tc.prepare != nil {
					tc.prepare(h)
				}
				strategy := h.strategy(h.spec(tc.spec))
				if err := mode.login(h, strategy); err != nil {
					t.Fatalf("Login: %v", err)
				}
				if err := strategy.Ready(t.Context()); err != nil {
					t.Fatalf("Ready after login: %v", err)
				}
			})
		}
	}
}

type recordedListens struct {
	mu        sync.Mutex
	addresses []string
	listeners []net.Listener
}

func (r *recordedListens) listen(ctx context.Context, address string) (net.Listener, error) {
	var config net.ListenConfig
	listener, err := config.Listen(ctx, "tcp", address)
	if err != nil {
		return nil, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.addresses = append(r.addresses, address)
	r.listeners = append(r.listeners, listener)
	return listener, nil
}

func (r *recordedListens) snapshot() ([]string, []net.Listener) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.addresses...), append([]net.Listener(nil), r.listeners...)
}

func redirectOf(t *testing.T, authorizeURL string) *url.URL {
	t.Helper()
	parsed, err := url.Parse(authorizeURL)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	redirect, err := url.Parse(parsed.Query().Get("redirect_uri"))
	if err != nil {
		t.Fatalf("Parse redirect: %v", err)
	}
	return redirect
}

func TestPortlessRedirectListensBeforeBuildingTheAuthorizeUrlAndKeepsOneUri(t *testing.T) {
	h := newHarness(t)
	listens := &recordedListens{}
	h.listen = listens.listen
	strategy := h.strategy(h.spec(func(spec *Spec) { spec.RedirectURI = "http://127.0.0.1/callback" }))
	prompts := make(chan string, 1)
	done := make(chan error, 1)
	go func() {
		done <- strategy.Login(t.Context(), &LoginOptions{Prompt: func(raw string) { prompts <- raw }})
	}()
	var authorizeURL string
	select {
	case authorizeURL = <-prompts:
	case err := <-done:
		t.Fatalf("Login ended early: %v", err)
	case <-time.After(20 * time.Second):
		t.Fatal("login did not prompt")
	}
	addresses, listeners := listens.snapshot()
	if len(addresses) != 1 || addresses[0] != "127.0.0.1:0" {
		t.Fatalf("listen addresses = %v, want one ephemeral loopback bind", addresses)
	}
	redirect := redirectOf(t, authorizeURL)
	if redirect.Hostname() != "127.0.0.1" || redirect.Path != "/callback" || redirect.Port() == "" || redirect.Port() == "0" {
		t.Fatalf("authorize redirect_uri = %s, want a concrete port", redirect)
	}
	if _, port, _ := net.SplitHostPort(listeners[0].Addr().String()); port != redirect.Port() {
		t.Fatalf("listener port = %s, redirect port = %s", port, redirect.Port())
	}
	resp, err := getURL(t.Context(), h.as.Approve(authorizeURL))
	if err != nil {
		t.Fatalf("callback: %v", err)
	}
	_ = resp.Body.Close()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Login: %v", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("login did not finish")
	}
	if got := h.as.LastForm().Get("redirect_uri"); got != redirect.String() {
		t.Fatalf("token request redirect_uri = %q, authorize redirect_uri = %q", got, redirect)
	}
}

func TestPortlessRedirectInPasteModePicksAConcretePortWithoutListening(t *testing.T) {
	h := newHarness(t)
	listens := &recordedListens{}
	h.listen = listens.listen
	strategy := h.strategy(h.spec(func(spec *Spec) { spec.RedirectURI = "http://127.0.0.1/callback" }))
	raw, err := captureAuthorizeURL(h, strategy)
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	redirect := redirectOf(t, raw)
	if redirect.Port() == "" || redirect.Port() == "0" || redirect.Path != "/callback" {
		t.Fatalf("authorize redirect_uri = %s, want a concrete port", redirect)
	}
	if got := h.as.LastForm().Get("redirect_uri"); got != redirect.String() {
		t.Fatalf("token request redirect_uri = %q, authorize redirect_uri = %q", got, redirect)
	}
	_, listeners := listens.snapshot()
	var dialer net.Dialer
	for _, listener := range listeners {
		if conn, dialErr := dialer.DialContext(t.Context(), "tcp", listener.Addr().String()); dialErr == nil {
			_ = conn.Close()
			t.Fatal("paste mode left a listener open")
		}
	}
}

func TestExplicitPortRedirectStaysExactInBothModes(t *testing.T) {
	h := newHarness(t)
	strategy := h.strategy(h.spec(nil))
	raw, err := captureAuthorizeURL(h, strategy)
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	if got := redirectOf(t, raw).String(); got != h.redirect {
		t.Fatalf("authorize redirect_uri = %q, want %q", got, h.redirect)
	}
}
