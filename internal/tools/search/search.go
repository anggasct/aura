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

	"github.com/anggasct/aura/internal/approval"
	"github.com/anggasct/aura/internal/egress"
	"github.com/anggasct/aura/internal/secret"
	"github.com/anggasct/aura/internal/toolbroker"
)

const defaultEndpoint = "https://api.search.brave.com/res/v1/web/search"

type Options struct {
	Provider      string
	CredentialRef string
	Endpoint      string
	Timeout       time.Duration
	MaxResults    int
	MaxBodyBytes  int64
	Resolver      egress.Resolver
	Client        *http.Client
}

type arguments struct {
	Query      string `json:"query"`
	MaxResults int    `json:"max_results"`
}

type providerResponse struct {
	Web struct {
		Results []struct {
			Title       string `json:"title"`
			URL         string `json:"url"`
			Description string `json:"description"`
		} `json:"results"`
	} `json:"web"`
}

type result struct {
	Results []searchResult `json:"results"`
}

type searchResult struct {
	Title   string `json:"title"`
	URL     string `json:"url"`
	Snippet string `json:"snippet"`
}

func New(options *Options) (toolbroker.Adapter, error) {
	if options == nil {
		return nil, errors.New("search: options must not be nil")
	}
	optionsCopy := *options
	options = &optionsCopy
	if strings.TrimSpace(options.Provider) == "" {
		return nil, errors.New("search: provider must not be empty")
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
	if options.Endpoint == "" {
		options.Endpoint = defaultEndpoint
	}
	if _, err := url.ParseRequestURI(options.Endpoint); err != nil {
		return nil, errors.New("search: endpoint is invalid")
	}
	client := options.Client
	if client == nil {
		client = egress.NewClient(options.Resolver)
	}
	clientCopy := *client
	return func(ctx context.Context, request *toolbroker.ToolRequest, constraints approval.Constraints) (toolbroker.ToolResult, error) {
		return run(ctx, request, constraints, options, &clientCopy)
	}, nil
}

func run(ctx context.Context, request *toolbroker.ToolRequest, constraints approval.Constraints, options *Options, client *http.Client) (toolbroker.ToolResult, error) {
	if request == nil {
		return toolbroker.ToolResult{}, toolbroker.Errorf(toolbroker.ResultInvalidArgument, "request must not be nil")
	}
	var args arguments
	if err := json.Unmarshal(request.Arguments, &args); err != nil || strings.TrimSpace(args.Query) == "" {
		return toolbroker.ToolResult{}, toolbroker.Errorf(toolbroker.ResultInvalidArgument, "query must be a non-empty string")
	}
	topN := options.MaxResults
	if args.MaxResults > 0 && args.MaxResults < topN {
		topN = args.MaxResults
	}
	credential, err := resolveCredential(options.CredentialRef)
	if err != nil {
		return toolbroker.ToolResult{}, toolbroker.Errorf(toolbroker.ResultCapabilityUnavailable, "search credential is unavailable")
	}
	vault := secret.New()
	vault.Set("provider", credential)
	payload, err := json.Marshal(map[string]any{"query": args.Query, "max_results": topN})
	if err != nil {
		return toolbroker.ToolResult{}, fmt.Errorf("search: encode request: %w", err)
	}
	timeout := options.Timeout
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
	httpRequest, err := http.NewRequestWithContext(runCtx, http.MethodPost, options.Endpoint, strings.NewReader(string(payload)))
	if err != nil {
		return toolbroker.ToolResult{}, toolbroker.Errorf(toolbroker.ResultInvalidArgument, "build search request")
	}
	httpRequest.Header.Set("Accept", "application/json")
	httpRequest.Header.Set("Content-Type", "application/json")
	switch options.Provider {
	case "brave":
		httpRequest.Header.Set("X-Subscription-Token", credential)
	default:
		httpRequest.Header.Set("Authorization", "Bearer "+credential)
	}
	response, err := client.Do(httpRequest)
	if err != nil {
		return toolbroker.ToolResult{}, err
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode >= 400 {
		return toolbroker.ToolResult{}, fmt.Errorf("search provider returned status %d", response.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, options.MaxBodyBytes+1))
	if err != nil {
		return toolbroker.ToolResult{}, fmt.Errorf("search: read provider response: %w", err)
	}
	if int64(len(body)) > options.MaxBodyBytes {
		return toolbroker.ToolResult{}, toolbroker.Errorf(toolbroker.ResultExecutionFailed, "search provider response exceeds limit")
	}
	var provider providerResponse
	if err := json.Unmarshal(body, &provider); err != nil {
		return toolbroker.ToolResult{}, errors.New("search: invalid provider response")
	}
	results := make([]searchResult, 0, resultLimit(topN, len(provider.Web.Results)))
	for _, item := range provider.Web.Results {
		if len(results) == topN {
			break
		}
		results = append(results, searchResult{Title: vault.Redact(item.Title), URL: vault.Redact(item.URL), Snippet: vault.Redact(item.Description)})
	}
	encoded, err := json.Marshal(result{Results: results})
	if err != nil {
		return toolbroker.ToolResult{}, fmt.Errorf("search: encode result: %w", err)
	}
	return toolbroker.ToolResult{Class: toolbroker.ResultOK, Output: encoded}, nil
}

func resolveCredential(raw string) (string, error) {
	if strings.HasPrefix(raw, "env://") {
		return secret.Reference{Env: strings.TrimPrefix(raw, "env://")}.Resolve()
	}
	if strings.HasPrefix(raw, "file://") {
		return secret.Reference{File: strings.TrimPrefix(raw, "file://")}.Resolve()
	}
	return "", errors.New("invalid credential reference")
}

func resultLimit(a, b int) int {
	if a < b {
		return a
	}
	return b
}
