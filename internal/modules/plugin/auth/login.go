package auth

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"io"
	"maps"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strconv"
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
	issuers       []string
	providerError bool
}

type redirectTarget struct {
	uri      *url.URL
	listener net.Listener
}

func (t *redirectTarget) close() {
	if t.listener != nil {
		_ = t.listener.Close()
	}
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
	target, err := s.openRedirect(ctx, opts)
	if err != nil {
		return err
	}
	defer target.close()
	config := s.config(clientID, clientSecret)
	config.RedirectURL = target.uri.String()
	authorizeURL := config.AuthCodeURL(state, s.authorizeOptions(verifier)...)
	result, err := s.collect(ctx, opts, target, authorizeURL)
	if err != nil {
		return err
	}
	if s.spec.Issuer != "" && (len(result.issuers) != 1 || result.issuers[0] != s.spec.Issuer) {
		return Errorf(ErrLogin, "the callback does not carry the declared issuer")
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
	if err := s.checkGrant(fetched); err != nil {
		return err
	}
	if next.RefreshToken == "" && !s.spec.AccessOnly {
		return Errorf(ErrLogin, "the token endpoint returned no refresh token")
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

func (s *oauth2Strategy) authorizeOptions(verifier string) []oauth2.AuthCodeOption {
	options := make([]oauth2.AuthCodeOption, 1, 1+len(s.spec.AuthorizeParams))
	options[0] = oauth2.S256ChallengeOption(verifier)
	for _, key := range slices.Sorted(maps.Keys(s.spec.AuthorizeParams)) {
		options = append(options, oauth2.SetAuthURLParam(key, s.spec.AuthorizeParams[key]))
	}
	return options
}

func (s *oauth2Strategy) checkGrant(fetched *oauth2.Token) error {
	raw := fetched.Extra("scope")
	if raw == nil {
		return nil
	}
	scope, isText := raw.(string)
	if !isText {
		return Errorf(ErrLogin, "the token endpoint reported a scope that cannot be read")
	}
	granted := strings.Fields(scope)
	for _, requested := range s.spec.Scopes {
		if !slices.Contains(granted, requested) {
			return Errorf(ErrLogin, "the authorization server did not grant every requested scope")
		}
	}
	return nil
}

func (s *oauth2Strategy) openRedirect(ctx context.Context, opts *LoginOptions) (*redirectTarget, error) {
	redirect, err := url.Parse(s.spec.RedirectURI)
	if err != nil {
		return nil, Errorf(ErrInvalid, "the redirect uri is not valid")
	}
	portless := redirect.Scheme == "http" && redirect.Port() == ""
	if opts.Paste {
		if !portless {
			return &redirectTarget{uri: redirect}, nil
		}
		reserved, err := s.registry.listen(ctx, net.JoinHostPort(redirect.Hostname(), "0"))
		if err != nil {
			return nil, Errorf(ErrLogin, "a callback port cannot be chosen")
		}
		port, portErr := listenerPort(reserved)
		_ = reserved.Close()
		if portErr != nil {
			return nil, portErr
		}
		return &redirectTarget{uri: withPort(redirect, port)}, nil
	}
	if redirect.Scheme != "http" {
		return nil, Errorf(ErrLogin, "this redirect uri needs paste mode")
	}
	address := redirect.Host
	if portless {
		address = net.JoinHostPort(redirect.Hostname(), "0")
	}
	listener, err := s.registry.listen(ctx, address)
	if err != nil {
		return nil, Errorf(ErrLogin, "the callback address cannot be opened")
	}
	port, err := listenerPort(listener)
	if err != nil {
		_ = listener.Close()
		return nil, err
	}
	return &redirectTarget{uri: withPort(redirect, port), listener: listener}, nil
}

func listenerPort(listener net.Listener) (int, error) {
	_, text, err := net.SplitHostPort(listener.Addr().String())
	if err != nil {
		return 0, Errorf(ErrLogin, "the callback address cannot be read")
	}
	port, err := strconv.Atoi(text)
	if err != nil || port < 1 || port > 65535 {
		return 0, Errorf(ErrLogin, "the callback address cannot be read")
	}
	return port, nil
}

func withPort(redirect *url.URL, port int) *url.URL {
	concrete := *redirect
	concrete.Host = net.JoinHostPort(redirect.Hostname(), strconv.Itoa(port))
	return &concrete
}

func (s *oauth2Strategy) collect(ctx context.Context, opts *LoginOptions, target *redirectTarget, authorizeURL string) (callbackResult, error) {
	if opts.Paste {
		return s.collectPasted(ctx, opts, target.uri, authorizeURL)
	}
	return s.collectLoopback(ctx, opts, target, authorizeURL)
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
	return callbackResult{code: query.Get("code"), state: query.Get("state"), issuers: slices.Clone(query["iss"]), providerError: query.Get("error") != ""}
}

func (s *oauth2Strategy) collectLoopback(ctx context.Context, opts *LoginOptions, target *redirectTarget, authorizeURL string) (callbackResult, error) {
	redirect := target.uri
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
		_ = server.Serve(target.listener)
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
