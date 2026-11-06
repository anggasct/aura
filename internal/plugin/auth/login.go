package auth

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"golang.org/x/oauth2"
)

const (
	maxPastedBytes       = 8 << 10
	stateBytes           = 32
	callbackHeaderLimit  = 10 * time.Second
	callbackShutdownWait = 5 * time.Second
)

type callbackResult struct {
	code          string
	state         string
	providerError bool
}

func (s *oauth2Strategy) Login(ctx context.Context, opts *LoginOptions) error {
	if ctx == nil || opts == nil || opts.Prompt == nil {
		return Errorf(ErrInvalid, "a context, login options and a prompt are required")
	}
	if opts.Paste && opts.Input == nil {
		return Errorf(ErrInvalid, "paste mode needs an input")
	}
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = LoginTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	clientID, clientSecret, err := s.clientCredentials(ctx)
	if err != nil {
		return err
	}
	state, err := randomState()
	if err != nil {
		return Errorf(ErrLogin, "a login state cannot be generated")
	}
	verifier := oauth2.GenerateVerifier()
	config := s.config(clientID, clientSecret)
	authorizeURL := config.AuthCodeURL(state, oauth2.S256ChallengeOption(verifier))
	result, err := s.collect(ctx, opts, authorizeURL)
	if err != nil {
		return err
	}
	if result.providerError {
		return Errorf(ErrLogin, "the authorization server reported an error")
	}
	if len(result.state) != len(state) || subtle.ConstantTimeCompare([]byte(result.state), []byte(state)) != 1 {
		return Errorf(ErrLogin, "the callback state does not match this login")
	}
	if result.code == "" {
		return Errorf(ErrLogin, "the callback carries no authorization code")
	}
	fetched, err := config.Exchange(s.tokenContext(ctx), result.code, oauth2.VerifierOption(verifier))
	if err != nil {
		return Errorf(ErrLogin, "the authorization code could not be exchanged")
	}
	next, err := s.acceptToken(fetched, "")
	if err != nil {
		return Errorf(ErrLogin, "the token endpoint returned an unusable token")
	}
	if err := s.store.save(&next); err != nil {
		return err
	}
	s.registry.register(next.AccessToken, next.RefreshToken)
	s.mu.Lock()
	s.loaded = true
	s.loadErr = nil
	s.current = next
	s.present = true
	s.failed = false
	s.mu.Unlock()
	return nil
}

func randomState() (string, error) {
	raw := make([]byte, stateBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

func (s *oauth2Strategy) collect(ctx context.Context, opts *LoginOptions, authorizeURL string) (callbackResult, error) {
	redirect, err := url.Parse(s.spec.RedirectURI)
	if err != nil {
		return callbackResult{}, Errorf(ErrInvalid, "the redirect uri is not valid")
	}
	if opts.Paste {
		return s.collectPasted(ctx, opts, redirect, authorizeURL)
	}
	if redirect.Scheme != "http" {
		return callbackResult{}, Errorf(ErrLogin, "this redirect uri needs paste mode")
	}
	return s.collectLoopback(ctx, opts, redirect, authorizeURL)
}

func (s *oauth2Strategy) collectPasted(ctx context.Context, opts *LoginOptions, redirect *url.URL, authorizeURL string) (callbackResult, error) {
	if err := ctx.Err(); err != nil {
		return callbackResult{}, Errorf(ErrLogin, "the login was cancelled")
	}
	opts.Prompt(authorizeURL)
	line, err := bufio.NewReader(io.LimitReader(opts.Input, maxPastedBytes)).ReadString('\n')
	if err != nil && line == "" {
		return callbackResult{}, Errorf(ErrLogin, "no redirected url was provided")
	}
	pasted, err := url.Parse(strings.TrimSpace(line))
	if err != nil || !strings.EqualFold(pasted.Scheme, redirect.Scheme) || !strings.EqualFold(pasted.Host, redirect.Host) || pasted.Path != redirect.Path {
		return callbackResult{}, Errorf(ErrLogin, "the pasted url is not the redirect uri")
	}
	return resultOf(pasted.Query()), nil
}

func resultOf(query url.Values) callbackResult {
	return callbackResult{code: query.Get("code"), state: query.Get("state"), providerError: query.Get("error") != ""}
}

func (s *oauth2Strategy) collectLoopback(ctx context.Context, opts *LoginOptions, redirect *url.URL, authorizeURL string) (callbackResult, error) {
	listener, err := s.registry.listen(ctx, redirect.Host)
	if err != nil {
		return callbackResult{}, Errorf(ErrLogin, "the callback address cannot be opened")
	}
	results := make(chan callbackResult, 1)
	var once sync.Once
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+redirect.Path, func(w http.ResponseWriter, r *http.Request) {
		if r.Host != redirect.Host {
			http.NotFound(w, r)
			return
		}
		once.Do(func() { results <- resultOf(r.URL.Query()) })
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Referrer-Policy", "no-referrer")
		_, _ = io.WriteString(w, "You can close this window and return to the terminal.\n")
	})
	server := &http.Server{Handler: mux, ReadHeaderTimeout: callbackHeaderLimit}
	served := make(chan struct{})
	go func() {
		defer close(served)
		_ = server.Serve(listener)
	}()
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), callbackShutdownWait)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			_ = server.Close()
		}
		<-served
	}()
	opts.Prompt(authorizeURL)
	select {
	case result := <-results:
		return result, nil
	case <-ctx.Done():
		return callbackResult{}, Errorf(ErrLogin, "no callback arrived before the login ended")
	}
}
