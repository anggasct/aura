package fetch

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
	"github.com/anggasct/aura/internal/toolbroker"
)

type Options struct {
	Timeout         time.Duration
	MaxRedirects    int
	MaxEncodedBytes int64
	MaxDecodedBytes int64
	Resolver        egress.Resolver

	// client is unexported so external constructors can only obtain the
	// mediated egress client; in-package tests use it as a canned seam.
	client *http.Client
}

type arguments struct {
	URL string `json:"url"`
}

type result struct {
	URL         string `json:"url"`
	StatusCode  int    `json:"status_code"`
	ContentType string `json:"content_type"`
	Body        string `json:"body"`
	Truncated   bool   `json:"truncated"`
}

func New(options Options) (toolbroker.Adapter, error) {
	if options.Timeout <= 0 {
		return nil, errors.New("fetch: timeout must be positive")
	}
	if options.MaxRedirects < 0 || options.MaxRedirects > 10 {
		return nil, errors.New("fetch: max redirects must be between 0 and 10")
	}
	if options.MaxEncodedBytes <= 0 || options.MaxDecodedBytes <= 0 {
		return nil, errors.New("fetch: body limits must be positive")
	}
	client := options.client
	if client == nil {
		client = egress.NewClient(options.Resolver)
	}
	clientCopy := *client
	originalRedirect := client.CheckRedirect
	clientCopy.CheckRedirect = func(request *http.Request, via []*http.Request) error {
		if len(via) >= options.MaxRedirects {
			return egress.Errorf(egress.ErrorCodeEgressDenied, "redirect limit exceeded")
		}
		if originalRedirect != nil {
			return originalRedirect(request, via)
		}
		return nil
	}
	return func(ctx context.Context, request *toolbroker.ToolRequest, constraints approval.Constraints) (toolbroker.ToolResult, error) {
		return run(ctx, request, constraints, options, &clientCopy)
	}, nil
}

func run(ctx context.Context, request *toolbroker.ToolRequest, constraints approval.Constraints, options Options, client *http.Client) (toolbroker.ToolResult, error) {
	if request == nil {
		return toolbroker.ToolResult{}, toolbroker.Errorf(toolbroker.ResultInvalidArgument, "request must not be nil")
	}
	var args arguments
	if err := json.Unmarshal(request.Arguments, &args); err != nil || strings.TrimSpace(args.URL) == "" {
		return toolbroker.ToolResult{}, toolbroker.Errorf(toolbroker.ResultInvalidArgument, "url must be a non-empty string")
	}
	parsed, err := url.Parse(args.URL)
	if err != nil || parsed.Scheme != "http" && parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil {
		return toolbroker.ToolResult{}, toolbroker.Errorf(toolbroker.ResultPolicyDenied, "public HTTP(S) URL is required")
	}
	if parsed.RawQuery != "" || parsed.Fragment != "" {
		return toolbroker.ToolResult{}, toolbroker.Errorf(toolbroker.ResultPolicyDenied, "query and fragment are not allowed")
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
	httpRequest, err := http.NewRequestWithContext(runCtx, http.MethodGet, parsed.String(), http.NoBody)
	if err != nil {
		return toolbroker.ToolResult{}, toolbroker.Errorf(toolbroker.ResultInvalidArgument, "build request")
	}
	httpRequest.Header.Set("Accept", "text/plain, text/html, application/json, application/xml")
	response, err := client.Do(httpRequest)
	if err != nil {
		return toolbroker.ToolResult{}, err
	}
	defer func() { _ = response.Body.Close() }()
	if response.ContentLength > options.MaxEncodedBytes {
		return toolbroker.ToolResult{}, toolbroker.Errorf(toolbroker.ResultPolicyDenied, "response exceeds encoded body limit")
	}
	if encoding := response.Header.Get("Content-Encoding"); encoding != "" && !strings.EqualFold(encoding, "identity") {
		return toolbroker.ToolResult{}, toolbroker.Errorf(toolbroker.ResultPolicyDenied, "compressed response encoding is not accepted")
	}
	contentType := strings.ToLower(strings.TrimSpace(strings.Split(response.Header.Get("Content-Type"), ";")[0]))
	if !allowedContentType(contentType) {
		return toolbroker.ToolResult{}, toolbroker.Errorf(toolbroker.ResultPolicyDenied, "content type is not readable")
	}
	// The stream is read up to the larger cap plus one byte so the encoded
	// bound is enforceable even when the decoded cap is the smaller one:
	// truncation to MaxDecodedBytes must never hide an over-limit wire
	// body. With identity transfer the stream bytes are the encoded bytes;
	// transparently decompressed responses are covered by the decoded cap
	// because the compressed wire stream is never larger than the
	// decompressed body.
	readLimit := options.MaxEncodedBytes
	if options.MaxDecodedBytes > readLimit {
		readLimit = options.MaxDecodedBytes
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, readLimit+1))
	if err != nil {
		return toolbroker.ToolResult{}, fmt.Errorf("fetch: read response: %w", err)
	}
	if !response.Uncompressed && int64(len(body)) > options.MaxEncodedBytes {
		return toolbroker.ToolResult{}, toolbroker.Errorf(toolbroker.ResultPolicyDenied, "response exceeds encoded body limit")
	}
	truncated := int64(len(body)) > options.MaxDecodedBytes
	if truncated {
		body = body[:options.MaxDecodedBytes]
	}
	if isHTML(contentType) {
		converted, convertedTruncated := htmlToMarkdown(body, options.MaxDecodedBytes)
		body = []byte(converted)
		truncated = truncated || convertedTruncated
	}
	encoded, err := json.Marshal(result{URL: parsed.String(), StatusCode: response.StatusCode, ContentType: contentType, Body: string(body), Truncated: truncated})
	if err != nil {
		return toolbroker.ToolResult{}, fmt.Errorf("fetch: encode result: %w", err)
	}
	class := toolbroker.ResultOK
	if response.StatusCode >= 400 {
		class = toolbroker.ResultExecutionFailed
	}
	return toolbroker.ToolResult{Class: class, Output: encoded}, nil
}

func allowedContentType(contentType string) bool {
	return strings.HasPrefix(contentType, "text/") || contentType == "application/json" || contentType == "application/xml" || contentType == "application/xhtml+xml"
}
