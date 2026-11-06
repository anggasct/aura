package auth

import (
	"context"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/plugin/auth/authtest"
)

func getURL(ctx context.Context, target string) (*http.Response, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target, http.NoBody)
	if err != nil {
		return nil, err
	}
	return http.DefaultClient.Do(request)
}

func requireCode(t *testing.T, err error, want ErrorCode) {
	t.Helper()
	got, ok := CodeOf(err)
	if !ok || got != want {
		t.Fatalf("error = %v, want code %s", err, want)
	}
}

func requireNoLeak(t *testing.T, err error) {
	t.Helper()
	if err != nil && strings.Contains(err.Error(), authtest.PlantedBody) {
		t.Fatalf("error leaks provider text: %v", err)
	}
}

func requireNoTokenFile(t *testing.T, h *harness) {
	t.Helper()
	if _, err := os.Stat(h.tokenPath()); !os.IsNotExist(err) {
		t.Fatalf("token file state = %v, want absent", err)
	}
}

func (h *harness) loopbackLogin(strategy Interactive, edit func(callback string) string) error {
	h.t.Helper()
	prompts := make(chan string, 1)
	failures := make(chan error, 1)
	opts := &LoginOptions{Prompt: func(authorizeURL string) { prompts <- authorizeURL }}
	done := make(chan error, 1)
	go func() { done <- strategy.Login(h.t.Context(), opts) }()
	select {
	case authorizeURL := <-prompts:
		callback := h.as.Approve(authorizeURL)
		if edit != nil {
			callback = edit(callback)
		}
		go func() {
			resp, err := getURL(h.t.Context(), callback)
			if err != nil {
				failures <- err
				return
			}
			_ = resp.Body.Close()
			failures <- nil
		}()
	case err := <-done:
		return err
	case <-time.After(20 * time.Second):
		h.t.Fatal("login did not prompt")
	}
	select {
	case err := <-done:
		if callbackErr := <-failures; callbackErr != nil {
			h.t.Fatalf("callback request: %v", callbackErr)
		}
		return err
	case <-time.After(20 * time.Second):
		h.t.Fatal("login did not finish")
	}
	return nil
}

func TestLoginStoresATokenInBothModes(t *testing.T) {
	cases := []struct {
		name  string
		login func(h *harness, strategy Interactive) error
	}{
		{"loopback listener", func(h *harness, strategy Interactive) error { return h.loopbackLogin(strategy, nil) }},
		{"pasted redirect", func(h *harness, strategy Interactive) error { return h.pasteLogin(strategy, nil) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			strategy := h.strategy(h.spec(nil))
			requireCode(t, strategy.Ready(t.Context()), ErrRequired)
			if err := tc.login(h, strategy); err != nil {
				t.Fatalf("Login: %v", err)
			}
			if !h.as.VerifierChecked() {
				t.Fatal("the token request carried no valid PKCE verifier")
			}
			if err := strategy.Ready(t.Context()); err != nil {
				t.Fatalf("Ready after login: %v", err)
			}
			request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, h.as.URL(), http.NoBody)
			if err != nil {
				t.Fatalf("NewRequest: %v", err)
			}
			if err := strategy.Authorize(t.Context(), request); err != nil {
				t.Fatalf("Authorize: %v", err)
			}
			if got := request.Header.Get("Authorization"); got != "Bearer "+h.as.AccessToken(1) {
				t.Fatalf("Authorization = %q", got)
			}
			for _, secretValue := range []string{h.as.AccessToken(1), h.as.RefreshToken(1), authtest.ClientID, authtest.Secret} {
				if !h.redactor.has(secretValue) {
					t.Fatalf("value %q was not registered for redaction", secretValue)
				}
			}
		})
	}
}

func TestLoginSendsPKCEStateAndTheConfiguredClient(t *testing.T) {
	h := newHarness(t)
	h.as.RequireClientSecret()
	strategy := h.strategy(h.spec(nil))
	var authorizeURL string
	input := &pastedInput{}
	opts := &LoginOptions{Paste: true, Input: input, Prompt: func(raw string) {
		authorizeURL = raw
		input.url = h.as.Approve(raw) + "\n"
	}}
	if err := strategy.Login(t.Context(), opts); err != nil {
		t.Fatalf("Login: %v", err)
	}
	parsed, err := url.Parse(authorizeURL)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	query := parsed.Query()
	if query.Get("code_challenge_method") != "S256" || query.Get("code_challenge") == "" || len(query.Get("state")) < 32 {
		t.Fatalf("authorize query = %v", query)
	}
	if query.Get("client_id") != authtest.ClientID || query.Get("scope") != "mail.read" || query.Get("redirect_uri") != h.redirect {
		t.Fatalf("authorize query = %v", query)
	}
	form := h.as.LastForm()
	if form.Get("code_verifier") == "" || form.Get("redirect_uri") != h.redirect || form.Get("grant_type") != "authorization_code" {
		t.Fatalf("token form = %v", form)
	}
	if h.as.LastAuthorization() != authtest.ClientID+":"+authtest.Secret {
		t.Fatalf("client authentication = %q", h.as.LastAuthorization())
	}
}

func TestLoginWithAPublicClientSendsNoSecret(t *testing.T) {
	h := newHarness(t)
	strategy := h.strategy(h.spec(func(spec *Spec) { spec.ClientSecretRef = "" }))
	if err := h.pasteLogin(strategy, nil); err != nil {
		t.Fatalf("Login: %v", err)
	}
	form := h.as.LastForm()
	if form.Get("client_id") != authtest.ClientID || form.Has("client_secret") || h.as.LastAuthorization() != "" {
		t.Fatalf("token form = %v, auth = %q", form, h.as.LastAuthorization())
	}
}

func TestLoginFailuresStoreNothing(t *testing.T) {
	replaceState := func(callback string) string {
		parsed, _ := url.Parse(callback)
		query := parsed.Query()
		query.Set("state", "not-the-state-of-this-login")
		parsed.RawQuery = query.Encode()
		return parsed.String()
	}
	dropCode := func(callback string) string {
		parsed, _ := url.Parse(callback)
		query := parsed.Query()
		query.Del("code")
		parsed.RawQuery = query.Encode()
		return parsed.String()
	}
	cases := []struct {
		name    string
		prepare func(h *harness)
		edit    func(callback string) string
	}{
		{"wrong state", nil, replaceState},
		{"provider error callback", func(h *harness) { h.as.DenyAuthorization() }, nil},
		{"missing code", nil, dropCode},
		{"code rejected", func(h *harness) { h.as.QueueInvalidGrant() }, nil},
		{"outage on exchange", func(h *harness) { h.as.QueueOutage() }, nil},
		{"unusable token type", func(h *harness) {
			h.as.QueueTokenResponse(http.StatusOK, `{"access_token":"mac-token","token_type":"mac","expires_in":60}`)
		}, nil},
		{"token without type", func(h *harness) {
			h.as.QueueTokenResponse(http.StatusOK, `{"access_token":"typeless-token","expires_in":60}`)
		}, nil},
		{"token without access token", func(h *harness) {
			h.as.QueueTokenResponse(http.StatusOK, `{"token_type":"Bearer","error_description":"`+authtest.PlantedBody+`"}`)
		}, nil},
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
				strategy := h.strategy(h.spec(nil))
				err := mode.login(h, strategy, tc.edit)
				requireCode(t, err, ErrLogin)
				requireNoLeak(t, err)
				requireNoTokenFile(t, h)
				requireCode(t, strategy.Ready(t.Context()), ErrRequired)
				for _, leaked := range []string{"mac-token", "typeless-token"} {
					if h.redactor.has(leaked) {
						t.Fatalf("rejected token %q was registered", leaked)
					}
				}
			})
		}
	}
}

func TestLoginListenerShutsDownWhenNoCallbackArrives(t *testing.T) {
	h := newHarness(t)
	strategy := h.strategy(h.spec(nil))
	address := h.listener.Addr().String()
	opts := &LoginOptions{Timeout: time.Millisecond, Prompt: func(string) {}}
	err := strategy.Login(t.Context(), opts)
	requireCode(t, err, ErrLogin)
	requireNoTokenFile(t, h)
	var dialer net.Dialer
	if conn, dialErr := dialer.DialContext(t.Context(), "tcp", address); dialErr == nil {
		_ = conn.Close()
		t.Fatal("the callback listener is still accepting connections")
	}
}

func TestLoginListenerShutsDownWhenTheCallerCancels(t *testing.T) {
	h := newHarness(t)
	strategy := h.strategy(h.spec(nil))
	ctx, cancel := context.WithCancel(t.Context())
	opts := &LoginOptions{Prompt: func(string) { cancel() }}
	err := strategy.Login(ctx, opts)
	requireCode(t, err, ErrLogin)
	var dialer net.Dialer
	if conn, dialErr := dialer.DialContext(t.Context(), "tcp", h.listener.Addr().String()); dialErr == nil {
		_ = conn.Close()
		t.Fatal("the callback listener is still accepting connections")
	}
}

func TestLoginRejectsUnusableModes(t *testing.T) {
	cases := []struct {
		name string
		spec func(spec *Spec)
		opts func() *LoginOptions
		want ErrorCode
	}{
		{"https redirect without paste", func(spec *Spec) { spec.RedirectURI = "https://example.test/callback" }, func() *LoginOptions { return &LoginOptions{Prompt: func(string) {}} }, ErrLogin},
		{"paste without input", nil, func() *LoginOptions { return &LoginOptions{Paste: true, Prompt: func(string) {}} }, ErrInvalid},
		{"no prompt", nil, func() *LoginOptions { return &LoginOptions{} }, ErrInvalid},
		{"empty paste", nil, func() *LoginOptions {
			return &LoginOptions{Paste: true, Input: strings.NewReader(""), Prompt: func(string) {}}
		}, ErrLogin},
		{"foreign pasted url", nil, func() *LoginOptions {
			return &LoginOptions{Paste: true, Input: strings.NewReader("https://evil.test/callback?code=a&state=b\n"), Prompt: func(string) {}}
		}, ErrLogin},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			strategy := h.strategy(h.spec(tc.spec))
			requireCode(t, strategy.Login(t.Context(), tc.opts()), tc.want)
			requireNoTokenFile(t, h)
		})
	}
}

func TestLoginCallbackServesOnlyTheRedirectPath(t *testing.T) {
	h := newHarness(t)
	strategy := h.strategy(h.spec(nil))
	prompts := make(chan string, 1)
	done := make(chan error, 1)
	go func() {
		done <- strategy.Login(t.Context(), &LoginOptions{Prompt: func(raw string) { prompts <- raw }})
	}()
	var authorizeURL string
	select {
	case authorizeURL = <-prompts:
	case <-time.After(20 * time.Second):
		t.Fatal("login did not prompt")
	}
	for _, probe := range []struct{ method, path string }{{http.MethodGet, "/other"}, {http.MethodPost, "/callback"}} {
		request, err := http.NewRequestWithContext(t.Context(), probe.method, "http://"+h.listener.Addr().String()+probe.path, http.NoBody)
		if err != nil {
			t.Fatalf("NewRequest: %v", err)
		}
		resp, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatalf("%s %s: %v", probe.method, probe.path, err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			t.Fatalf("%s %s answered 200", probe.method, probe.path)
		}
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
}

func TestLoginListensOnTheRedirectAddressByDefault(t *testing.T) {
	h := newHarness(t)
	address := h.listener.Addr().String()
	if err := h.listener.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	registry, err := NewRegistry(&Options{Dir: h.dir, Secrets: h.secrets, HTTPClient: http.DefaultClient, Redactor: h.redactor})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	strategy, err := registry.Strategy(h.spec(nil))
	if err != nil {
		t.Fatalf("Strategy: %v", err)
	}
	interactive, ok := strategy.(Interactive)
	if !ok {
		t.Fatalf("strategy %T is not interactive", strategy)
	}
	prompts := make(chan string, 1)
	done := make(chan error, 1)
	go func() {
		done <- interactive.Login(t.Context(), &LoginOptions{Prompt: func(raw string) { prompts <- raw }})
	}()
	var authorizeURL string
	select {
	case authorizeURL = <-prompts:
	case <-time.After(20 * time.Second):
		t.Fatal("login did not prompt")
	}
	resp, err := getURL(t.Context(), h.as.Approve(authorizeURL))
	if err != nil {
		t.Fatalf("callback on %s: %v", address, err)
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
}
