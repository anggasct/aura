package auth

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	"golang.org/x/oauth2"
)

const (
	tokenExpiryMargin = 30 * time.Second

	errorInvalidGrant  = "invalid_grant"
	errorInvalidClient = "invalid_client"
)

type oauth2Strategy struct {
	registry *Registry
	spec     Spec
	store    *tokenStore

	mu      sync.Mutex
	loaded  bool
	loadErr error
	current tokenFile
	present bool
	failed  bool

	refreshMu sync.Mutex
}

func newOAuth2(r *Registry, spec *Spec) (Strategy, error) {
	if spec.AuthorizeURL == "" || spec.TokenURL == "" || spec.RedirectURI == "" || len(spec.Scopes) == 0 || spec.ClientIDRef == "" || spec.Fingerprint == "" {
		return nil, Errorf(ErrInvalid, "oauth2 needs endpoints, a redirect uri, scopes, a client id and a fingerprint")
	}
	cloned := *spec
	cloned.Scopes = append([]string(nil), spec.Scopes...)
	return &oauth2Strategy{registry: r, spec: cloned, store: r.store(spec.PluginID)}, nil
}

func (s *oauth2Strategy) ensureLoaded() {
	if s.loaded {
		return
	}
	s.loaded = true
	stored, found, err := s.store.load()
	if err != nil {
		s.loadErr = err
		return
	}
	if !found || stored.Fingerprint != s.spec.Fingerprint {
		return
	}
	s.current = stored
	s.present = true
	s.registry.register(stored.AccessToken, stored.RefreshToken)
}

func (s *oauth2Strategy) stateError() error {
	switch {
	case s.loadErr != nil:
		return s.loadErr
	case s.failed:
		return Errorf(ErrFailed, "the authorization was rejected and needs a new login")
	case !s.present:
		return Errorf(ErrRequired, "the plugin is not logged in")
	case s.current.State == stateFailed:
		return Errorf(ErrFailed, "the authorization was rejected and needs a new login")
	case s.current.AccessToken == "" && s.current.RefreshToken == "":
		return Errorf(ErrRequired, "the plugin is not logged in")
	}
	return nil
}

func (s *oauth2Strategy) clientCredentials(ctx context.Context) (clientID, clientSecret string, err error) {
	clientID, err = s.registry.secrets.ResolveSecret(ctx, s.spec.ClientIDRef)
	clientID = strings.TrimSpace(clientID)
	if err != nil || clientID == "" {
		return "", "", Errorf(ErrRequired, "the oauth2 client credentials are unavailable")
	}
	if s.spec.ClientSecretRef != "" {
		clientSecret, err = s.registry.secrets.ResolveSecret(ctx, s.spec.ClientSecretRef)
		clientSecret = strings.TrimSpace(clientSecret)
		if err != nil || clientSecret == "" {
			return "", "", Errorf(ErrRequired, "the oauth2 client credentials are unavailable")
		}
	}
	s.registry.register(clientID, clientSecret)
	return clientID, clientSecret, nil
}

func (s *oauth2Strategy) config(clientID, clientSecret string) *oauth2.Config {
	style := oauth2.AuthStyleInParams
	if clientSecret != "" {
		style = oauth2.AuthStyleInHeader
	}
	return &oauth2.Config{
		ClientID:     clientID,
		ClientSecret: clientSecret,
		Endpoint:     oauth2.Endpoint{AuthURL: s.spec.AuthorizeURL, TokenURL: s.spec.TokenURL, AuthStyle: style},
		RedirectURL:  s.spec.RedirectURI,
		Scopes:       append([]string(nil), s.spec.Scopes...),
	}
}

func (s *oauth2Strategy) tokenContext(ctx context.Context) context.Context {
	return context.WithValue(ctx, oauth2.HTTPClient, s.registry.tokenHTTP)
}

func (s *oauth2Strategy) Ready(ctx context.Context) error {
	if ctx == nil {
		return Errorf(ErrInvalid, "a context is required")
	}
	s.mu.Lock()
	s.ensureLoaded()
	err := s.stateError()
	s.mu.Unlock()
	if err != nil {
		return err
	}
	_, _, err = s.clientCredentials(ctx)
	return err
}

func (s *oauth2Strategy) valid() bool {
	if s.current.AccessToken == "" {
		return false
	}
	return s.current.Expiry.IsZero() || s.registry.now().Add(tokenExpiryMargin).Before(s.current.Expiry)
}

func (s *oauth2Strategy) accessToken() (access string, usable bool, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ensureLoaded()
	if err := s.stateError(); err != nil {
		return "", false, err
	}
	return s.current.AccessToken, s.valid(), nil
}

func (s *oauth2Strategy) Authorize(ctx context.Context, req *http.Request) error {
	if ctx == nil || req == nil {
		return Errorf(ErrInvalid, "a context and a request are required")
	}
	access, usable, err := s.accessToken()
	if err != nil {
		return err
	}
	if !usable {
		access, err = s.refresh(ctx)
		if err != nil {
			return err
		}
	}
	req.Header.Set(DefaultHeader, "Bearer "+access)
	return nil
}

func (s *oauth2Strategy) refresh(ctx context.Context) (string, error) {
	s.refreshMu.Lock()
	defer s.refreshMu.Unlock()
	s.mu.Lock()
	if err := s.stateError(); err != nil {
		s.mu.Unlock()
		return "", err
	}
	if s.valid() {
		access := s.current.AccessToken
		s.mu.Unlock()
		return access, nil
	}
	previous := s.current
	s.mu.Unlock()
	if previous.RefreshToken == "" {
		return "", Errorf(ErrRequired, "the access token expired and cannot be refreshed; log in again")
	}
	clientID, clientSecret, err := s.clientCredentials(ctx)
	if err != nil {
		return "", err
	}
	source := s.config(clientID, clientSecret).TokenSource(s.tokenContext(ctx), &oauth2.Token{RefreshToken: previous.RefreshToken})
	fetched, err := source.Token()
	if err != nil {
		if permanentRejection(err) {
			s.markFailed()
			return "", Errorf(ErrFailed, "the authorization was rejected and needs a new login")
		}
		return "", Errorf(ErrUnavailable, "the token endpoint is unavailable")
	}
	next, err := s.acceptToken(fetched, previous.RefreshToken)
	if err != nil {
		return "", err
	}
	if err := s.store.save(&next); err != nil {
		return "", err
	}
	s.registry.register(next.AccessToken, next.RefreshToken)
	s.mu.Lock()
	s.current = next
	s.present = true
	s.mu.Unlock()
	return next.AccessToken, nil
}

func (s *oauth2Strategy) acceptToken(fetched *oauth2.Token, fallbackRefresh string) (tokenFile, error) {
	if fetched == nil || fetched.AccessToken == "" || !strings.EqualFold(fetched.TokenType, "bearer") {
		return tokenFile{}, Errorf(ErrUnavailable, "the token endpoint returned an unusable token")
	}
	refresh := fetched.RefreshToken
	if refresh == "" {
		refresh = fallbackRefresh
	}
	return tokenFile{State: stateOK, Fingerprint: s.spec.Fingerprint, AccessToken: fetched.AccessToken, RefreshToken: refresh, Expiry: fetched.Expiry}, nil
}

func (s *oauth2Strategy) markFailed() {
	s.mu.Lock()
	s.failed = true
	s.current = tokenFile{State: stateFailed, Fingerprint: s.spec.Fingerprint}
	s.mu.Unlock()
	_ = s.store.save(&tokenFile{State: stateFailed, Fingerprint: s.spec.Fingerprint})
}

func permanentRejection(err error) bool {
	var rejected *oauth2.RetrieveError
	if !errors.As(err, &rejected) || rejected.Response == nil {
		return false
	}
	if rejected.Response.StatusCode < 400 || rejected.Response.StatusCode >= 500 {
		return false
	}
	return rejected.ErrorCode == errorInvalidGrant || rejected.ErrorCode == errorInvalidClient
}
