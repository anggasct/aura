package search

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/anggasct/aura/internal/platform/approval"
	"github.com/anggasct/aura/internal/platform/egress"
	"github.com/anggasct/aura/internal/platform/secret"
	"github.com/anggasct/aura/internal/platform/toolbroker"
)

const (
	providerDuckDuckGo = "duckduckgo"
	providerBrave      = "brave"
	providerTavily     = "tavily"
	providerSearXNG    = "searxng"

	userAgent = "Aura (+https://github.com/anggasct/aura)"

	detailBlocked       = "search provider blocked the request"
	detailNoReadable    = "search provider returned no readable results"
	detailUnreadable    = "search provider returned an unreadable response"
	detailEnginesFailed = "search engines did not respond"
	detailRequestFailed = "search provider request failed"
	detailRefused       = "search provider refused the request"
	detailRejected      = "search provider rejected the query"
	detailRateLimited   = "search provider rate or usage limit reached"
)

type Options struct {
	Provider          string
	CredentialRef     string
	Endpoint          string
	LoopbackAuthority string
	Timeout           time.Duration
	MaxResults        int
	MaxBodyBytes      int64
	Resolver          egress.Resolver

	client *http.Client
}

type provider interface {
	newRequest(ctx context.Context, query string, limit int, credential string) (*http.Request, error)
	parse(body []byte) ([]searchResult, error)
}

type credentialMode int

const (
	credentialNever credentialMode = iota
	credentialRequired
	credentialOptional
)

type arguments struct {
	Query      string `json:"query"`
	MaxResults int    `json:"max_results"`
}

type result struct {
	Results []searchResult `json:"results"`
}

type searchResult struct {
	Title   string `json:"title"`
	URL     string `json:"url"`
	Snippet string `json:"snippet"`
}

type searcher struct {
	provider   provider
	credential credentialMode
	options    *Options
	client     *http.Client
}

func New(options *Options) (toolbroker.Adapter, error) {
	if options == nil {
		return nil, errors.New("search: options must not be nil")
	}
	optionsCopy := *options
	options = &optionsCopy
	selected, mode, err := selectProvider(options)
	if err != nil {
		return nil, err
	}
	if options.Timeout <= 0 {
		return nil, errors.New("search: timeout must be positive")
	}
	if options.MaxResults < 1 || options.MaxResults > 20 {
		return nil, errors.New("search: max results must be between 1 and 20")
	}
	if options.MaxBodyBytes <= 0 {
		return nil, errors.New("search: body limit must be positive")
	}
	client, err := buildClient(options)
	if err != nil {
		return nil, err
	}
	instance := &searcher{provider: selected, credential: mode, options: options, client: client}
	return instance.run, nil
}

func selectProvider(options *Options) (provider, credentialMode, error) {
	switch options.Provider {
	case providerDuckDuckGo:
		if options.CredentialRef != "" {
			return nil, credentialNever, errors.New("search: provider duckduckgo does not take a credential")
		}
		return duckDuckGo{}, credentialNever, rejectEndpoint(options)
	case providerBrave:
		return brave{}, credentialRequired, rejectEndpoint(options)
	case providerTavily:
		return tavily{}, credentialRequired, rejectEndpoint(options)
	case providerSearXNG:
		if options.Endpoint == "" {
			return nil, credentialNever, errors.New("search: provider searxng requires an endpoint")
		}
		return searXNG{endpoint: options.Endpoint}, credentialOptional, nil
	}
	return nil, credentialNever, errors.New("search: unsupported provider")
}

func rejectEndpoint(options *Options) error {
	if options.Endpoint != "" || options.LoopbackAuthority != "" {
		return fmt.Errorf("search: provider %s does not accept an endpoint", options.Provider)
	}
	return nil
}

func refuseRedirect(_ *http.Request, _ []*http.Request) error {
	return http.ErrUseLastResponse
}

func buildClient(options *Options) (*http.Client, error) {
	var client *http.Client
	switch {
	case options.client != nil:
		copied := *options.client
		client = &copied
	case options.LoopbackAuthority != "":
		if err := validateLoopbackEndpoint(options.Endpoint, options.LoopbackAuthority); err != nil {
			return nil, err
		}
		loopback, err := egress.NewLoopbackClient(options.LoopbackAuthority)
		if err != nil {
			return nil, fmt.Errorf("search: loopback client: %w", err)
		}
		client = loopback
	default:
		if options.Endpoint != "" {
			if err := egress.ValidateDestinationShape(options.Endpoint); err != nil {
				return nil, fmt.Errorf("search: endpoint is not a permitted destination: %w", err)
			}
		}
		client = egress.NewAPIClient(options.Resolver)
	}
	client.CheckRedirect = refuseRedirect
	return client, nil
}

func validateLoopbackEndpoint(raw, authority string) error {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme != "http" || parsed.Host != authority || parsed.User != nil || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" {
		return errors.New("search: loopback endpoint must be an http URL on the loopback authority without credentials, query or fragment")
	}
	return nil
}

func (s *searcher) run(ctx context.Context, request *toolbroker.ToolRequest, constraints approval.Constraints) (toolbroker.ToolResult, error) {
	if request == nil {
		return toolbroker.ToolResult{}, toolbroker.Errorf(toolbroker.ResultInvalidArgument, "request must not be nil")
	}
	var args arguments
	if err := json.Unmarshal(request.Arguments, &args); err != nil || strings.TrimSpace(args.Query) == "" {
		return toolbroker.ToolResult{}, toolbroker.Errorf(toolbroker.ResultInvalidArgument, "query must be a non-empty string")
	}
	topN := s.options.MaxResults
	if args.MaxResults > 0 && args.MaxResults < topN {
		topN = args.MaxResults
	}
	credential, err := s.resolveCredential()
	if err != nil {
		return toolbroker.ToolResult{}, toolbroker.Errorf(toolbroker.ResultCapabilityUnavailable, "search credential is unavailable")
	}
	vault := secret.New()
	vault.Set("provider", credential)
	timeout := s.options.Timeout
	if constraints.Timeout > 0 && constraints.Timeout < timeout {
		timeout = constraints.Timeout
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if !request.Deadline.IsZero() {
		deadlineCtx, deadlineCancel := context.WithDeadline(runCtx, request.Deadline)
		defer deadlineCancel()
		runCtx = deadlineCtx
	}
	httpRequest, err := s.provider.newRequest(runCtx, args.Query, topN, credential)
	if err != nil {
		return toolbroker.ToolResult{}, toolbroker.Errorf(toolbroker.ResultInvalidArgument, "build search request")
	}
	response, err := s.client.Do(httpRequest)
	if err != nil {
		return toolbroker.ToolResult{}, transportError(err)
	}
	defer func() { _ = response.Body.Close() }()
	if err := statusError(response.StatusCode); err != nil {
		return toolbroker.ToolResult{}, err
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, s.options.MaxBodyBytes+1))
	if err != nil {
		return toolbroker.ToolResult{}, transportError(err)
	}
	if int64(len(body)) > s.options.MaxBodyBytes {
		return toolbroker.ToolResult{}, toolbroker.Errorf(toolbroker.ResultExecutionFailed, "search provider response exceeds limit")
	}
	parsed, err := s.provider.parse(body)
	if err != nil {
		return toolbroker.ToolResult{}, err
	}
	results := make([]searchResult, 0, min(topN, len(parsed)))
	for _, item := range parsed {
		if len(results) == topN {
			break
		}
		if item.URL == "" {
			continue
		}
		results = append(results, searchResult{Title: vault.Redact(item.Title), URL: vault.Redact(item.URL), Snippet: vault.Redact(item.Snippet)})
	}
	encoded, err := json.Marshal(result{Results: results})
	if err != nil {
		return toolbroker.ToolResult{}, fmt.Errorf("search: encode result: %w", err)
	}
	return toolbroker.ToolResult{Class: toolbroker.ResultOK, Output: encoded}, nil
}

func (s *searcher) resolveCredential() (string, error) {
	if s.credential == credentialNever {
		return "", nil
	}
	if s.options.CredentialRef == "" {
		if s.credential == credentialOptional {
			return "", nil
		}
		return "", errors.New("credential reference is empty")
	}
	value, err := resolveCredential(s.options.CredentialRef)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(value) == "" {
		return "", errors.New("credential is empty")
	}
	return value, nil
}

func resolveCredential(raw string) (string, error) {
	if name, ok := strings.CutPrefix(raw, "env://"); ok && name != "" {
		return secret.Reference{Env: name}.Resolve()
	}
	if name, ok := strings.CutPrefix(raw, "file://"); ok && name != "" {
		return secret.Reference{File: name}.Resolve()
	}
	return "", errors.New("invalid credential reference")
}

func transportError(err error) error {
	var denied *egress.Error
	if errors.As(err, &denied) {
		return denied
	}
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return context.DeadlineExceeded
	case errors.Is(err, context.Canceled):
		return context.Canceled
	}
	return toolbroker.Errorf(toolbroker.ResultExecutionFailed, detailRequestFailed)
}

func statusError(status int) error {
	switch {
	case status >= 200 && status < 300:
		return nil
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return toolbroker.Errorf(toolbroker.ResultCapabilityUnavailable, detailRefused)
	case status == http.StatusBadRequest || status == http.StatusUnprocessableEntity:
		return toolbroker.Errorf(toolbroker.ResultInvalidArgument, detailRejected)
	case status == http.StatusTooManyRequests || status == 432 || status == 433:
		return toolbroker.Errorf(toolbroker.ResultExecutionFailed, detailRateLimited)
	}
	return toolbroker.Errorf(toolbroker.ResultExecutionFailed, "search provider returned status %d", status)
}

func newProviderRequest(ctx context.Context, method, target string, body io.Reader, accept string) (*http.Request, error) {
	request, err := http.NewRequestWithContext(ctx, method, target, body)
	if err != nil {
		return nil, fmt.Errorf("search: build request: %w", err)
	}
	request.Header.Set("Accept", accept)
	request.Header.Set("User-Agent", userAgent)
	return request, nil
}

func unreadableResponse() error {
	return toolbroker.Errorf(toolbroker.ResultExecutionFailed, detailUnreadable)
}
