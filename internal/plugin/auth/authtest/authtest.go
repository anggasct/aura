package authtest

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
)

const (
	PlantedBody = "PLANTED-PROVIDER-BODY-7f3a"
	ClientID    = "client-id-value"
	Secret      = "client-secret-value"
)

type override struct {
	status int
	body   string
}

type Server struct {
	t      *testing.T
	server *httptest.Server

	mu            sync.Mutex
	challenges    map[string]string
	redirects     map[string]string
	refreshTokens map[string]bool
	overrides     []override
	issued        int
	expiresIn     int
	deny          bool
	requireSecret bool
	exchanges     int
	refreshes     int
	verified      bool
	lastForm      url.Values
	lastAuth      string
	lastQuery     url.Values
	issuers       []string
}

func New(t *testing.T) *Server {
	t.Helper()
	s := &Server{
		t: t, challenges: map[string]string{}, redirects: map[string]string{}, refreshTokens: map[string]bool{}, expiresIn: 3600,
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/authorize", s.serveAuthorize)
	mux.HandleFunc("/token", s.serveToken)
	s.server = httptest.NewServer(mux)
	t.Cleanup(s.server.Close)
	return s
}

func (s *Server) URL() string { return s.server.URL }

func (s *Server) AuthorizeURL() string { return s.server.URL + "/authorize" }

func (s *Server) TokenURL() string { return s.server.URL + "/token" }

func (s *Server) RequireClientSecret() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.requireSecret = true
}

func (s *Server) ExpireIn(seconds int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.expiresIn = seconds
}

func (s *Server) DenyAuthorization() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.deny = true
}

func (s *Server) SetCallbackIssuers(issuers ...string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.issuers = issuers
}

func (s *Server) LastAuthorizeQuery() url.Values {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastQuery
}

func (s *Server) QueueTokenResponse(status int, body string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.overrides = append(s.overrides, override{status: status, body: body})
}

func (s *Server) QueueInvalidGrant() {
	s.QueueTokenResponse(http.StatusBadRequest, `{"error":"invalid_grant","error_description":"`+PlantedBody+`"}`)
}

func (s *Server) QueueOutage() {
	s.QueueTokenResponse(http.StatusServiceUnavailable, "upstream failure "+PlantedBody)
}

func (s *Server) Exchanges() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.exchanges
}

func (s *Server) Refreshes() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.refreshes
}

func (s *Server) VerifierChecked() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.verified
}

func (s *Server) LastForm() url.Values {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastForm
}

func (s *Server) LastAuthorization() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastAuth
}

func (s *Server) AccessToken(n int) string { return fmt.Sprintf("access-token-%d", n) }

func (s *Server) RefreshToken(n int) string { return fmt.Sprintf("refresh-token-%d", n) }

func (s *Server) Issued() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.issued
}

func (s *Server) Approve(authorizeURL string) string {
	s.t.Helper()
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	request, err := http.NewRequestWithContext(s.t.Context(), http.MethodGet, authorizeURL, http.NoBody)
	if err != nil {
		s.t.Fatalf("build authorize request: %v", err)
	}
	resp, err := client.Do(request)
	if err != nil {
		s.t.Fatalf("GET authorize url: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	location := resp.Header.Get("Location")
	if resp.StatusCode != http.StatusFound || location == "" {
		s.t.Fatalf("authorize status = %d, location = %q", resp.StatusCode, location)
	}
	return location
}

func (s *Server) serveAuthorize(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	redirect := query.Get("redirect_uri")
	target, err := url.Parse(redirect)
	if err != nil || redirect == "" || query.Get("response_type") != "code" || query.Get("code_challenge_method") != "S256" || query.Get("code_challenge") == "" || query.Get("state") == "" {
		http.Error(w, "invalid authorization request", http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastQuery = query
	values := target.Query()
	values.Set("state", query.Get("state"))
	for _, issuer := range s.issuers {
		values.Add("iss", issuer)
	}
	if s.deny {
		values.Set("error", "access_denied")
		values.Set("error_description", PlantedBody)
	} else {
		code := fmt.Sprintf("code-%d", len(s.challenges)+1)
		s.challenges[code] = query.Get("code_challenge")
		s.redirects[code] = redirect
		values.Set("code", code)
	}
	target.RawQuery = values.Encode()
	w.Header().Set("Location", target.String())
	w.WriteHeader(http.StatusFound)
}

func (s *Server) respond(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(body))
}

func (s *Server) serveToken(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		s.respond(w, http.StatusBadRequest, `{"error":"invalid_request"}`)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastForm = r.PostForm
	user, pass, hasBasic := r.BasicAuth()
	s.lastAuth = ""
	if hasBasic {
		s.lastAuth = user + ":" + pass
	}
	if s.requireSecret && (!hasBasic || user != ClientID || pass != Secret) {
		s.respond(w, http.StatusUnauthorized, `{"error":"invalid_client"}`)
		return
	}
	switch r.PostForm.Get("grant_type") {
	case "authorization_code":
		s.exchanges++
	case "refresh_token":
		s.refreshes++
	}
	if len(s.overrides) > 0 {
		next := s.overrides[0]
		s.overrides = s.overrides[1:]
		s.respond(w, next.status, next.body)
		return
	}
	switch r.PostForm.Get("grant_type") {
	case "authorization_code":
		code := r.PostForm.Get("code")
		sum := sha256.Sum256([]byte(r.PostForm.Get("code_verifier")))
		if s.challenges[code] == "" || s.challenges[code] != base64.RawURLEncoding.EncodeToString(sum[:]) || s.redirects[code] != r.PostForm.Get("redirect_uri") {
			s.respond(w, http.StatusBadRequest, `{"error":"invalid_grant"}`)
			return
		}
		s.verified = true
		delete(s.challenges, code)
	case "refresh_token":
		refresh := r.PostForm.Get("refresh_token")
		if !s.refreshTokens[refresh] {
			s.respond(w, http.StatusBadRequest, `{"error":"invalid_grant"}`)
			return
		}
		delete(s.refreshTokens, refresh)
	default:
		s.respond(w, http.StatusBadRequest, `{"error":"unsupported_grant_type"}`)
		return
	}
	s.issued++
	refresh := s.RefreshToken(s.issued)
	s.refreshTokens[refresh] = true
	payload, err := json.Marshal(map[string]any{
		"access_token": s.AccessToken(s.issued), "refresh_token": refresh, "token_type": "Bearer", "expires_in": s.expiresIn,
	})
	if err != nil {
		s.t.Errorf("marshal token response: %v", err)
		return
	}
	s.respond(w, http.StatusOK, string(payload))
}
