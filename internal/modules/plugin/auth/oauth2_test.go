package auth

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/anggasct/aura/internal/modules/plugin/auth/authtest"
)

func authorizeOnce(t *testing.T, strategy Strategy) (*http.Request, error) {
	t.Helper()
	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://mail.example.test/mcp", http.NoBody)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	return request, strategy.Authorize(t.Context(), request)
}

func readTokenFile(t *testing.T, h *harness) tokenFile {
	t.Helper()
	data, err := os.ReadFile(h.tokenPath())
	if err != nil {
		t.Fatalf("read token file: %v", err)
	}
	var stored tokenFile
	if err := json.Unmarshal(data, &stored); err != nil {
		t.Fatalf("decode token file: %v", err)
	}
	return stored
}

func TestAuthorizeUsesTheStoredTokenWithoutRefreshing(t *testing.T) {
	h := newHarness(t)
	strategy := h.loggedIn(h.spec(nil))
	for range 3 {
		request, err := authorizeOnce(t, strategy)
		if err != nil {
			t.Fatalf("Authorize: %v", err)
		}
		if got := request.Header.Get("Authorization"); got != "Bearer "+h.as.AccessToken(1) {
			t.Fatalf("Authorization = %q", got)
		}
	}
	if h.as.Refreshes() != 0 {
		t.Fatalf("refreshes = %d, want 0", h.as.Refreshes())
	}
}

func TestRefreshRotatesAndPersistsTheTokenBeforeUse(t *testing.T) {
	h := newHarness(t)
	h.as.ExpireIn(1)
	strategy := h.loggedIn(h.spec(nil))
	request, err := authorizeOnce(t, strategy)
	if err != nil {
		t.Fatalf("Authorize: %v", err)
	}
	if got := request.Header.Get("Authorization"); got != "Bearer "+h.as.AccessToken(2) {
		t.Fatalf("Authorization = %q, want the rotated access token", got)
	}
	stored := readTokenFile(t, h)
	if stored.AccessToken != h.as.AccessToken(2) || stored.RefreshToken != h.as.RefreshToken(2) || stored.State != stateOK {
		t.Fatalf("persisted token = %+v", stored)
	}
	for _, rotated := range []string{h.as.AccessToken(2), h.as.RefreshToken(2)} {
		if !h.redactor.has(rotated) {
			t.Fatalf("refreshed value %q was not registered for redaction", rotated)
		}
	}
	restarted := h.strategy(h.spec(nil))
	h.as.ExpireIn(3600)
	next, err := authorizeOnce(t, restarted)
	if err != nil {
		t.Fatalf("Authorize after restart: %v", err)
	}
	if got := next.Header.Get("Authorization"); got != "Bearer "+h.as.AccessToken(3) {
		t.Fatalf("Authorization after restart = %q", got)
	}
}

func TestRefreshKeepsTheOldRefreshTokenWhenTheProviderOmitsOne(t *testing.T) {
	h := newHarness(t)
	h.as.ExpireIn(1)
	strategy := h.loggedIn(h.spec(nil))
	h.as.QueueTokenResponse(http.StatusOK, `{"access_token":"access-without-refresh","token_type":"bearer","expires_in":3600}`)
	request, err := authorizeOnce(t, strategy)
	if err != nil {
		t.Fatalf("Authorize: %v", err)
	}
	if request.Header.Get("Authorization") != "Bearer access-without-refresh" {
		t.Fatalf("Authorization = %q", request.Header.Get("Authorization"))
	}
	if stored := readTokenFile(t, h); stored.RefreshToken != h.as.RefreshToken(1) {
		t.Fatalf("persisted refresh token = %q, want the previous one", stored.RefreshToken)
	}
}

func TestRefreshFailsClosedWhenThePersistFails(t *testing.T) {
	h := newHarness(t)
	h.as.ExpireIn(1)
	strategy := h.loggedIn(h.spec(nil))
	if err := strategy.Ready(t.Context()); err != nil {
		t.Fatalf("Ready: %v", err)
	}
	namespace := filepath.Join(h.dir, testPluginID)
	if err := os.RemoveAll(namespace); err != nil {
		t.Fatalf("RemoveAll: %v", err)
	}
	if err := os.WriteFile(namespace, []byte("not a directory"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	request, err := authorizeOnce(t, strategy)
	requireCode(t, err, ErrUnavailable)
	if got := request.Header.Get("Authorization"); got != "" {
		t.Fatalf("a call that could not persist its token carried %q", got)
	}
	if h.as.Refreshes() != 1 {
		t.Fatalf("refreshes = %d, want 1", h.as.Refreshes())
	}
}

func TestPermanentRefreshFailureIsRecordedAcrossRestarts(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"invalid grant", `{"error":"invalid_grant","error_description":"` + authtest.PlantedBody + `"}`},
		{"invalid client", `{"error":"invalid_client","error_description":"` + authtest.PlantedBody + `"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			h.as.ExpireIn(1)
			strategy := h.loggedIn(h.spec(nil))
			h.as.QueueTokenResponse(http.StatusBadRequest, tc.body)
			request, err := authorizeOnce(t, strategy)
			requireCode(t, err, ErrFailed)
			requireNoLeak(t, err)
			if request.Header.Get("Authorization") != "" {
				t.Fatal("a rejected authorization carried a bearer")
			}
			requireCode(t, strategy.Ready(t.Context()), ErrFailed)
			refreshes := h.as.Refreshes()
			_, err = authorizeOnce(t, strategy)
			requireCode(t, err, ErrFailed)
			if h.as.Refreshes() != refreshes {
				t.Fatal("a failed plugin kept calling the token endpoint")
			}
			stored := readTokenFile(t, h)
			if stored.State != stateFailed || stored.AccessToken != "" || stored.RefreshToken != "" {
				t.Fatalf("persisted state = %+v", stored)
			}
			raw, err := os.ReadFile(h.tokenPath())
			if err != nil || bytes.Contains(raw, []byte(authtest.PlantedBody)) {
				t.Fatalf("token file keeps provider text: %v", err)
			}
			restarted := h.strategy(h.spec(nil))
			requireCode(t, restarted.Ready(t.Context()), ErrFailed)
			_, err = authorizeOnce(t, restarted)
			requireCode(t, err, ErrFailed)
			if h.as.Refreshes() != refreshes {
				t.Fatal("a restart retried a permanently rejected refresh token")
			}
		})
	}
}

func TestTransientRefreshFailuresChangeNothing(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
	}{
		{"server error", http.StatusServiceUnavailable, "maintenance " + authtest.PlantedBody},
		{"server error carrying invalid grant", http.StatusServiceUnavailable, `{"error":"invalid_grant","error_description":"` + authtest.PlantedBody + `"}`},
		{"rate limit", http.StatusTooManyRequests, `{"error":"slow_down","error_description":"` + authtest.PlantedBody + `"}`},
		{"bad request without a permanent code", http.StatusBadRequest, `{"error":"invalid_request","error_description":"` + authtest.PlantedBody + `"}`},
		{"redirect", http.StatusFound, authtest.PlantedBody},
		{"unparsable success", http.StatusOK, authtest.PlantedBody},
		{"unusable token type", http.StatusOK, `{"access_token":"mac-token","token_type":"mac","expires_in":60}`},
		{"oversized body", http.StatusOK, `{"access_token":"` + strings.Repeat("a", 70<<10) + `","token_type":"Bearer"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			h.as.ExpireIn(1)
			strategy := h.loggedIn(h.spec(nil))
			before, err := os.ReadFile(h.tokenPath())
			if err != nil {
				t.Fatalf("ReadFile: %v", err)
			}
			h.as.QueueTokenResponse(tc.status, tc.body)
			request, err := authorizeOnce(t, strategy)
			requireCode(t, err, ErrUnavailable)
			requireNoLeak(t, err)
			if request.Header.Get("Authorization") != "" {
				t.Fatal("a failed refresh carried a bearer")
			}
			after, err := os.ReadFile(h.tokenPath())
			if err != nil || !bytes.Equal(before, after) {
				t.Fatalf("a transient failure changed the token file: %v", err)
			}
			if err := strategy.Ready(t.Context()); err != nil {
				t.Fatalf("Ready after a transient failure: %v", err)
			}
			recovered, err := authorizeOnce(t, strategy)
			if err != nil {
				t.Fatalf("Authorize after recovery: %v", err)
			}
			if recovered.Header.Get("Authorization") != "Bearer "+h.as.AccessToken(2) {
				t.Fatalf("Authorization after recovery = %q", recovered.Header.Get("Authorization"))
			}
		})
	}
}

func TestConcurrentCallsShareOneRefresh(t *testing.T) {
	h := newHarness(t)
	h.as.ExpireIn(1)
	strategy := h.loggedIn(h.spec(nil))
	h.as.ExpireIn(3600)
	var wg sync.WaitGroup
	results := make(chan string, 8)
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			request := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "https://mail.example.test/mcp", nil)
			if err := strategy.Authorize(t.Context(), request); err != nil {
				results <- "error: " + err.Error()
				return
			}
			results <- request.Header.Get("Authorization")
		}()
	}
	wg.Wait()
	close(results)
	for got := range results {
		if !strings.HasPrefix(got, "Bearer access-token-") {
			t.Fatalf("result = %q", got)
		}
	}
	if h.as.Refreshes() != 1 {
		t.Fatalf("refreshes = %d, want one shared refresh", h.as.Refreshes())
	}
}

func TestStoredTokenBelongsToTheAuthBlockItWasIssuedFor(t *testing.T) {
	h := newHarness(t)
	h.loggedIn(h.spec(nil))
	changed := h.strategy(h.spec(func(spec *Spec) { spec.Fingerprint = "fingerprint-two" }))
	requireCode(t, changed.Ready(t.Context()), ErrRequired)
	request, err := authorizeOnce(t, changed)
	requireCode(t, err, ErrRequired)
	if request.Header.Get("Authorization") != "" {
		t.Fatal("a token issued for another auth block was sent")
	}
	if err := h.strategy(h.spec(nil)).Ready(t.Context()); err != nil {
		t.Fatalf("the original auth block lost its token: %v", err)
	}
}

func TestUnreadableTokenFilesCountAsNotLoggedIn(t *testing.T) {
	cases := []struct {
		name    string
		content string
	}{
		{"not json", "not json"},
		{"unknown field", `{"version":1,"state":"ok","fingerprint":"fingerprint-one","access_token":"a","extra":1}`},
		{"trailing data", `{"version":1,"state":"ok","fingerprint":"fingerprint-one","access_token":"a"} {}`},
		{"wrong version", `{"version":9,"state":"ok","fingerprint":"fingerprint-one","access_token":"a"}`},
		{"unknown state", `{"version":1,"state":"weird","fingerprint":"fingerprint-one","access_token":"a"}`},
		{"empty tokens", `{"version":1,"state":"ok","fingerprint":"fingerprint-one"}`},
		{"oversized", strings.Repeat(" ", maxTokenFileBytes+1)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			if err := os.MkdirAll(filepath.Join(h.dir, testPluginID), 0o700); err != nil {
				t.Fatalf("MkdirAll: %v", err)
			}
			if err := os.WriteFile(h.tokenPath(), []byte(tc.content), 0o600); err != nil {
				t.Fatalf("WriteFile: %v", err)
			}
			requireCode(t, h.strategy(h.spec(nil)).Ready(t.Context()), ErrRequired)
		})
	}
}

func TestExpiredTokenWithoutARefreshTokenNeedsANewLogin(t *testing.T) {
	h := newHarness(t)
	h.as.ExpireIn(1)
	strategy := h.strategy(h.spec(func(spec *Spec) { spec.AccessOnly = true }))
	prompt := func(input *pastedInput) func(string) {
		return func(raw string) {
			input.url = h.as.Approve(raw) + "\n"
		}
	}
	input := &pastedInput{}
	h.as.QueueTokenResponse(http.StatusOK, `{"access_token":"short-lived","token_type":"Bearer","expires_in":1}`)
	if err := strategy.Login(t.Context(), &LoginOptions{Paste: true, Input: input, Prompt: prompt(input)}); err != nil {
		t.Fatalf("Login: %v", err)
	}
	_, err := authorizeOnce(t, strategy)
	requireCode(t, err, ErrRequired)
}

func TestMissingClientCredentialsKeepThePluginNotReady(t *testing.T) {
	h := newHarness(t)
	strategy := h.loggedIn(h.spec(nil))
	h.secrets.set(clientIDRef, "")
	requireCode(t, h.strategy(h.spec(nil)).Ready(t.Context()), ErrRequired)
	_, err := authorizeOnce(t, strategy)
	if err != nil {
		t.Fatalf("a valid access token needs no client credentials: %v", err)
	}
}
