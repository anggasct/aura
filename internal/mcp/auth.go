package mcp

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/anggasct/aura/internal/config"
	sdkauth "github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/oauthex"
	"golang.org/x/oauth2"
)

const defaultAuthHeader = "Authorization"

const tokenExpiryMargin = 30 * time.Second

const metadataDocumentLimit = 64 * 1024

type OAuthTokenStore interface {
	Load() (TokenRecord, bool, error)
	Store(record TokenRecord) error
}

type MemoryTokenStore struct {
	mu     sync.Mutex
	record TokenRecord
	found  bool
}

func NewMemoryTokenStore() *MemoryTokenStore {
	return &MemoryTokenStore{}
}

func (s *MemoryTokenStore) Load() (TokenRecord, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.record, s.found, nil
}

func (s *MemoryTokenStore) Store(record TokenRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.record = record
	s.found = true
	return nil
}

type staticAuthTransport struct {
	next       http.RoundTripper
	header     string
	ref        string
	serverName string
	resolver   SecretResolver
}

func (t *staticAuthTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req == nil {
		return nil, Errorf(ErrConfigInvalid, "request must not be nil")
	}
	if t.resolver == nil {
		return nil, Errorf(ErrAuthRequired, "server %q has no credential resolver", t.serverName)
	}
	token, err := t.resolver.ResolveSecret(req.Context(), t.ref)
	if err != nil || strings.TrimSpace(token) == "" {
		return nil, Errorf(ErrAuthRequired, "server %q credential is unavailable", t.serverName)
	}
	header := t.header
	if header == "" {
		header = defaultAuthHeader
	}
	out := req.Clone(req.Context())
	value := strings.TrimSpace(token)
	if strings.EqualFold(header, defaultAuthHeader) && !strings.Contains(value, " ") {
		value = "Bearer " + value
	}
	out.Header.Set(header, value)
	return t.next.RoundTrip(out)
}

func (t *staticAuthTransport) CloseIdleConnections() {
	if closer, ok := t.next.(interface{ CloseIdleConnections() }); ok {
		closer.CloseIdleConnections()
	}
}

type OAuthProvider struct {
	serverName string
	serverURL  string
	clientID   string
	clientSec  string
	httpClient *http.Client
	tokens     OAuthTokenStore
	fetch      sdkauth.AuthorizationCodeFetcher

	disabled atomic.Bool
}

func NewOAuthProvider(serverName, serverURL, clientID, clientSecret string, httpClient *http.Client, tokens OAuthTokenStore, fetch sdkauth.AuthorizationCodeFetcher) (*OAuthProvider, error) {
	if strings.TrimSpace(serverName) == "" {
		return nil, Errorf(ErrConfigInvalid, "server name must not be empty")
	}
	if strings.TrimSpace(serverURL) == "" {
		return nil, Errorf(ErrConfigInvalid, "server url must not be empty")
	}
	if strings.TrimSpace(clientID) == "" || strings.TrimSpace(clientSecret) == "" {
		return nil, Errorf(ErrConfigInvalid, "oauth client credentials must not be empty")
	}
	if httpClient == nil {
		return nil, Errorf(ErrConfigInvalid, "http client must not be nil")
	}
	if tokens == nil {
		return nil, Errorf(ErrConfigInvalid, "token store must not be nil")
	}
	return &OAuthProvider{
		serverName: serverName,
		serverURL:  serverURL,
		clientID:   clientID,
		clientSec:  clientSecret,
		httpClient: httpClient,
		tokens:     tokens,
		fetch:      fetch,
	}, nil
}

func (p *OAuthProvider) storedSource(ctx context.Context) (oauth2.TokenSource, error) {
	record, found, err := p.tokens.Load()
	if err != nil {
		return nil, err
	}
	if !found || strings.TrimSpace(record.AccessToken) == "" {
		return nil, Errorf(ErrAuthRequired, "server %q has no stored token", p.serverName)
	}
	token := &oauth2.Token{
		AccessToken:  record.AccessToken,
		RefreshToken: record.RefreshToken,
		TokenType:    record.TokenType,
		Expiry:       record.Expiry,
	}
	if token.RefreshToken == "" {
		if token.Expiry.IsZero() || token.Expiry.After(time.Now().Add(tokenExpiryMargin)) {
			return oauth2.StaticTokenSource(token), nil
		}
		return nil, Errorf(ErrAuthRequired, "server %q token is expired", p.serverName)
	}
	tokenURL, err := discoverTokenEndpoint(ctx, p.httpClient, p.serverURL)
	if err != nil {
		return nil, err
	}
	base := (&oauth2.Config{
		ClientID:     p.clientID,
		ClientSecret: p.clientSec,
		Endpoint:     oauth2.Endpoint{TokenURL: tokenURL},
	}).TokenSource(ctx, token)
	return &persistingTokenSource{source: base, save: p.tokens.Store}, nil
}

func (p *OAuthProvider) disable() {
	p.disabled.Store(true)
}

func (p *OAuthProvider) isDisabled() bool {
	return p.disabled.Load()
}

func (p *OAuthProvider) credentials() *oauthex.ClientCredentials {
	return &oauthex.ClientCredentials{
		ClientID:         p.clientID,
		ClientSecretAuth: &oauthex.ClientSecretAuth{ClientSecret: p.clientSec},
	}
}

func persistentTokenSource(ctx context.Context, cfg *oauth2.Config, token *oauth2.Token, save func(TokenRecord) error) (oauth2.TokenSource, error) {
	record := TokenRecord{AccessToken: token.AccessToken, TokenType: token.TokenType, Expiry: token.Expiry}
	if token.RefreshToken != "" {
		record.RefreshToken = token.RefreshToken
	}
	if save != nil {
		if err := save(record); err != nil {
			return nil, err
		}
	}
	return &persistingTokenSource{source: cfg.TokenSource(ctx, token), save: save, last: record}, nil
}

func (p *OAuthProvider) newTokenSourceHook() func(context.Context, *oauth2.Config, *oauth2.Token) (oauth2.TokenSource, error) {
	return func(ctx context.Context, cfg *oauth2.Config, token *oauth2.Token) (oauth2.TokenSource, error) {
		return persistentTokenSource(ctx, cfg, token, p.tokens.Store)
	}
}

func validRedirectURL(rawURL string) error {
	if err := config.ValidateBaseURL(rawURL); err != nil {
		return Errorf(ErrConfigInvalid, "redirect url is not valid")
	}
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return Errorf(ErrConfigInvalid, "redirect url is not valid")
	}
	if parsed.Scheme != "https" && !config.IsLoopbackBaseURL(rawURL) {
		return Errorf(ErrConfigInvalid, "redirect url must use https outside loopback")
	}
	return nil
}

func interactiveSetupUnavailable(_ context.Context, _ *sdkauth.AuthorizationArgs) (*sdkauth.AuthorizationResult, error) {
	return nil, Errorf(ErrAuthRequired, "interactive authorization is not available; provision a token out of band")
}

func (p *OAuthProvider) Handler(ctx context.Context) (*sdkauth.AuthorizationCodeHandler, error) {
	if ctx == nil {
		return nil, Errorf(ErrConfigInvalid, "context must not be nil")
	}
	source, err := p.storedSource(ctx)
	if err != nil {
		return nil, err
	}
	fetch := p.fetch
	if fetch == nil {
		fetch = interactiveSetupUnavailable
	}
	return sdkauth.NewAuthorizationCodeHandler(&sdkauth.AuthorizationCodeHandlerConfig{
		PreregisteredClient:      p.credentials(),
		RedirectURL:              p.serverURL,
		AuthorizationCodeFetcher: fetch,
		RequestRefreshToken:      true,
		Client:                   p.httpClient,
		NewTokenSource:           p.newTokenSourceHook(),
		InitialTokenSource: &sanitizedTokenSource{
			source:     &disablingTokenSource{source: source, disable: p.disable, isDisabled: p.isDisabled},
			serverName: p.serverName,
		},
	})
}

func (p *OAuthProvider) ExchangeCode(ctx context.Context, redirectURL string) error {
	if ctx == nil {
		return Errorf(ErrConfigInvalid, "context must not be nil")
	}
	if err := validRedirectURL(redirectURL); err != nil {
		return err
	}
	if p.fetch == nil {
		return Errorf(ErrConfigInvalid, "authorization code fetcher must not be nil")
	}
	handler, err := sdkauth.NewAuthorizationCodeHandler(&sdkauth.AuthorizationCodeHandlerConfig{
		PreregisteredClient:      p.credentials(),
		RedirectURL:              redirectURL,
		AuthorizationCodeFetcher: p.fetch,
		RequestRefreshToken:      true,
		Client:                   p.httpClient,
		NewTokenSource:           p.newTokenSourceHook(),
	})
	if err != nil {
		return Errorf(ErrConfigInvalid, "oauth handler is not valid")
	}
	challenge := `Bearer resource_metadata="` + strings.TrimSuffix(p.serverURL, "/") + `/.well-known/oauth-protected-resource"`
	fabricatedReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.serverURL, http.NoBody)
	if err != nil {
		return Errorf(ErrConfigInvalid, "server url is not valid")
	}
	fabricatedResp := &http.Response{
		Status:     "401 Unauthorized",
		StatusCode: http.StatusUnauthorized,
		Header:     http.Header{"Www-Authenticate": {challenge}},
		Body:       http.NoBody,
		Request:    fabricatedReq,
	}
	if err := handler.Authorize(ctx, fabricatedReq, fabricatedResp); err != nil {
		return Errorf(ErrAuthRequired, "server %q authorization failed", p.serverName)
	}
	source, err := handler.TokenSource(ctx)
	if err != nil || source == nil {
		return Errorf(ErrAuthRequired, "server %q authorization failed", p.serverName)
	}
	if _, err := source.Token(); err != nil {
		return Errorf(ErrAuthRequired, "server %q authorization failed", p.serverName)
	}
	p.disabled.Store(false)
	return nil
}

type disablingTokenSource struct {
	source     oauth2.TokenSource
	disable    func()
	isDisabled func() bool
}

func (s *disablingTokenSource) Token() (*oauth2.Token, error) {
	if s.isDisabled != nil && s.isDisabled() {
		return nil, Errorf(ErrAuthRequired, "authentication is disabled")
	}
	token, err := s.source.Token()
	if err != nil {
		if s.disable != nil {
			s.disable()
		}
		return nil, err
	}
	return token, nil
}

type sanitizedTokenSource struct {
	source     oauth2.TokenSource
	serverName string
}

func (s *sanitizedTokenSource) Token() (*oauth2.Token, error) {
	token, err := s.source.Token()
	if err != nil {
		if _, ok := CodeOf(err); ok {
			return nil, err
		}
		return nil, Errorf(ErrAuthRequired, "server %q authentication failed", s.serverName)
	}
	return token, nil
}

type persistingTokenSource struct {
	mu     sync.Mutex
	source oauth2.TokenSource
	save   func(TokenRecord) error
	last   TokenRecord
}

func (s *persistingTokenSource) Token() (*oauth2.Token, error) {
	token, err := s.source.Token()
	if err != nil {
		return nil, err
	}
	record := TokenRecord{AccessToken: token.AccessToken, TokenType: token.TokenType, Expiry: token.Expiry}
	if token.RefreshToken != "" {
		record.RefreshToken = token.RefreshToken
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if record != s.last {
		s.last = record
		if s.save != nil {
			if err := s.save(record); err != nil {
				return nil, err
			}
		}
	}
	return token, nil
}

type authorizationServerMetadata struct {
	Issuer                        string   `json:"issuer"`
	AuthorizationEndpoint         string   `json:"authorization_endpoint"`
	TokenEndpoint                 string   `json:"token_endpoint"`
	RegistrationEndpoint          string   `json:"registration_endpoint,omitempty"`
	ScopesSupported               []string `json:"scopes_supported,omitempty"`
	ResponseTypesSupported        []string `json:"response_types_supported,omitempty"`
	CodeChallengeMethodsSupported []string `json:"code_challenge_methods_supported,omitempty"`
}

type protectedResourceMetadata struct {
	Resource             string   `json:"resource"`
	AuthorizationServers []string `json:"authorization_servers"`
}

func fetchJSON(ctx context.Context, client *http.Client, rawURL string, target any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, http.NoBody)
	if err != nil {
		return Errorf(ErrConfigInvalid, "metadata request is not valid")
	}
	resp, err := client.Do(req)
	if err != nil {
		if isResponseTooLarge(err) {
			return Errorf(ErrMessageTooLarge, "metadata document exceeds the message bound")
		}
		return Errorf(ErrServerUnavailable, "metadata endpoint is not available")
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return Errorf(ErrServerUnavailable, "metadata endpoint is not available")
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, metadataDocumentLimit+1))
	if err != nil {
		if isResponseTooLarge(err) {
			return Errorf(ErrMessageTooLarge, "metadata document exceeds the message bound")
		}
		return Errorf(ErrServerUnavailable, "metadata document could not be read")
	}
	if int64(len(body)) > metadataDocumentLimit {
		return Errorf(ErrMessageTooLarge, "metadata document exceeds the message bound")
	}
	if err := json.Unmarshal(body, target); err != nil {
		return Errorf(ErrResultInvalid, "metadata document is not valid")
	}
	return nil
}

func discoverTokenEndpoint(ctx context.Context, client *http.Client, serverURL string) (string, error) {
	base, err := url.Parse(serverURL)
	if err != nil {
		return "", Errorf(ErrConfigInvalid, "server url is not valid")
	}
	origin := base.Scheme + "://" + base.Host
	candidates := []string{
		strings.TrimSuffix(serverURL, "/") + "/.well-known/oauth-protected-resource",
		origin + "/.well-known/oauth-protected-resource",
	}
	var authorizationServers []string
	for _, candidate := range candidates {
		var prm protectedResourceMetadata
		if err := fetchJSON(ctx, client, candidate, &prm); err != nil {
			continue
		}
		if len(prm.AuthorizationServers) > 0 {
			authorizationServers = prm.AuthorizationServers
			break
		}
	}
	if len(authorizationServers) == 0 {
		authorizationServers = []string{origin}
	}
	for _, issuer := range authorizationServers {
		root := strings.TrimSuffix(strings.TrimSpace(issuer), "/")
		if root == "" {
			continue
		}
		for _, candidate := range []string{
			root + "/.well-known/oauth-authorization-server",
			root + "/.well-known/openid-configuration",
		} {
			var metadata authorizationServerMetadata
			if err := fetchJSON(ctx, client, candidate, &metadata); err != nil {
				continue
			}
			if strings.TrimSpace(metadata.TokenEndpoint) != "" {
				return metadata.TokenEndpoint, nil
			}
		}
	}
	return "", Errorf(ErrAuthRequired, "no token endpoint discovered")
}
