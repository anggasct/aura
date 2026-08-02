package model

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"syscall"
	"testing"
	"time"

	"google.golang.org/genai"
)

func testHTTPResponse(status int, retryAfter string) *http.Response {
	resp := &http.Response{
		StatusCode: status,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader("")),
	}
	if retryAfter != "" {
		resp.Header.Set("Retry-After", retryAfter)
	}
	return resp
}

func TestRetryHTTP_ExponentialBackoff(t *testing.T) {
	var delays []time.Duration
	cfg := RetryConfig{
		MaxRetries: 3,
		BaseDelay:  10 * time.Millisecond,
		MaxDelay:   100 * time.Millisecond,
		Jitter:     0,
		Sleep: func(_ context.Context, delay time.Duration) error {
			delays = append(delays, delay)
			return nil
		},
	}
	calls := 0
	resp, err := retryHTTP(context.Background(), cfg, func() (*http.Response, error) {
		calls++
		if calls <= 3 {
			return testHTTPResponse(http.StatusServiceUnavailable, ""), nil
		}
		return testHTTPResponse(http.StatusOK, ""), nil
	})
	if err != nil {
		t.Fatalf("retryHTTP: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK || calls != 4 {
		t.Errorf("status=%d calls=%d, want status 200 after 4 calls", resp.StatusCode, calls)
	}
	want := []time.Duration{10 * time.Millisecond, 20 * time.Millisecond, 40 * time.Millisecond}
	if len(delays) != len(want) {
		t.Fatalf("delays=%v, want %v", delays, want)
	}
	for i := range want {
		if delays[i] != want[i] {
			t.Errorf("delay[%d]=%v, want %v", i, delays[i], want[i])
		}
	}
}

func TestRetryHTTP_RetryAfterOverridesBackoff(t *testing.T) {
	var delay time.Duration
	cfg := RetryConfig{
		MaxRetries: 1,
		BaseDelay:  10 * time.Millisecond,
		MaxDelay:   60 * time.Second,
		Jitter:     0.25,
		Sleep: func(_ context.Context, d time.Duration) error {
			delay = d
			return nil
		},
	}
	calls := 0
	resp, err := retryHTTP(context.Background(), cfg, func() (*http.Response, error) {
		calls++
		if calls == 1 {
			return testHTTPResponse(http.StatusTooManyRequests, "2"), nil
		}
		return testHTTPResponse(http.StatusOK, ""), nil
	})
	if err != nil {
		t.Fatalf("retryHTTP: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if delay != 2*time.Second {
		t.Errorf("delay=%v, want 2s from Retry-After", delay)
	}
}

func TestRetryHTTP_MaxRetriesAndNonTransient(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusNotFound} {
		calls := 0
		resp, err := retryHTTP(context.Background(), defaultRetryConfig(), func() (*http.Response, error) {
			calls++
			return testHTTPResponse(status, ""), nil
		})
		if err != nil || resp.StatusCode != status || calls != 1 {
			t.Errorf("status %d: resp=%v err=%v calls=%d, want one attempt", status, resp, err, calls)
		}
		if resp != nil {
			_ = resp.Body.Close()
		}
	}

	calls := 0
	cfg := RetryConfig{MaxRetries: 3, BaseDelay: time.Millisecond, MaxDelay: time.Millisecond, Sleep: func(context.Context, time.Duration) error { return nil }}
	resp, err := retryHTTP(context.Background(), cfg, func() (*http.Response, error) {
		calls++
		return testHTTPResponse(http.StatusBadGateway, ""), nil
	})
	if err != nil || resp.StatusCode != http.StatusBadGateway || calls != 4 {
		t.Errorf("exhausted retries: resp=%v err=%v calls=%d, want final 502 after 4 attempts", resp, err, calls)
	}
	if resp != nil {
		_ = resp.Body.Close()
	}
}

func TestRetryHTTP_ContextCancellationDuringSleep(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	cfg := RetryConfig{MaxRetries: 1, Sleep: sleepWithContext}
	resp, err := retryHTTP(ctx, cfg, func() (*http.Response, error) {
		return testHTTPResponse(http.StatusServiceUnavailable, ""), nil
	})
	if resp != nil {
		_ = resp.Body.Close()
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err=%v, want context.Canceled", err)
	}
}

func TestExponentialDelay_CapsJitteredDelay(t *testing.T) {
	cfg := RetryConfig{BaseDelay: time.Second, MaxDelay: time.Second, Jitter: 1}
	for range 1000 {
		if got := exponentialDelay(cfg, 1); got > cfg.MaxDelay {
			t.Fatalf("delay=%v, want <= %v", got, cfg.MaxDelay)
		}
	}
}

func TestOpenAI_RetryTransientThenSuccess(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		writeFixture(t, w, fixtureBytes(t, "openai_completion.json"))
	}))
	defer srv.Close()

	a := mustOpenAIAdapter(t, "gpt-4o", srv.URL, "sk-test", 0)
	a.core.retry = RetryConfig{MaxRetries: 1, BaseDelay: time.Millisecond, MaxDelay: time.Millisecond, Sleep: func(context.Context, time.Duration) error { return nil }}
	resps, err := collect(a.GenerateContent(context.Background(), sampleRequest("hi"), false))
	if err != nil || len(resps) != 1 || calls != 2 {
		t.Fatalf("responses=%d err=%v calls=%d, want success after retry", len(resps), err, calls)
	}
}

func TestRetryConfigForRequestDisablesReplayAfterToolResult(t *testing.T) {
	req := sampleRequest("continue")
	req.Contents = append(req.Contents, &genai.Content{
		Role:  "user",
		Parts: []*genai.Part{{FunctionResponse: &genai.FunctionResponse{ID: "call-1", Name: "write_file", Response: map[string]any{"ok": true}}}},
	})
	cfg := retryConfigForRequest(defaultRetryConfig(), req)
	if cfg.MaxRetries != 0 {
		t.Fatalf("MaxRetries = %d, want 0 after tool result", cfg.MaxRetries)
	}
}

func TestOpenAI_400TypedErrors(t *testing.T) {
	cases := []struct {
		name string
		body string
		want error
	}{
		{name: "content filter", body: `{"error":{"code":"content_filter"}}`, want: ErrContentFiltered},
		{name: "context length", body: `{"error":{"code":"context_length_exceeded"}}`, want: ErrContextTooLong},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = io.WriteString(w, tc.body)
			}))
			defer srv.Close()

			a := mustOpenAIAdapter(t, "gpt-4o", srv.URL, "sk-test", 0)
			a.core.retry.MaxRetries = 0
			_, err := collect(a.GenerateContent(context.Background(), sampleRequest("hi"), false))
			if !errors.Is(err, tc.want) {
				t.Errorf("err=%v, want %v", err, tc.want)
			}
		})
	}
}

func TestRetryHTTP_GatewayTimeoutRetried(t *testing.T) {
	calls := 0
	cfg := RetryConfig{
		MaxRetries: 1,
		BaseDelay:  time.Millisecond,
		MaxDelay:   time.Millisecond,
		Jitter:     0,
		Sleep: func(context.Context, time.Duration) error {
			return nil
		},
	}
	resp, err := retryHTTP(context.Background(), cfg, func() (*http.Response, error) {
		calls++
		if calls == 1 {
			return testHTTPResponse(http.StatusGatewayTimeout, ""), nil
		}
		return testHTTPResponse(http.StatusOK, ""), nil
	})
	if err != nil {
		t.Fatalf("retryHTTP: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK || calls != 2 {
		t.Errorf("status=%d calls=%d, want 504 retried then 200", resp.StatusCode, calls)
	}
}

func TestClassifyHTTPResponse_StructuralJSON(t *testing.T) {
	cases := []struct {
		name string
		body string
		want error
	}{
		{name: "openai code", body: `{"error":{"code":"content_filter"}}`, want: ErrContentFiltered},
		{name: "anthropic type", body: `{"error":{"type":"context_length_exceeded"}}`, want: ErrContextTooLong},
		{name: "gemini blocked status", body: `{"error":{"code":400,"message":"request blocked","status":"BLOCKED"}}`, want: ErrContentFiltered},
		{name: "message fallback", body: `{"error":{"message":"prompt is too long"}}`, want: ErrContextTooLong},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := &http.Response{
				StatusCode: http.StatusBadRequest,
				Body:       io.NopCloser(strings.NewReader(tc.body)),
			}
			err := classifyHTTPResponse(resp, "test")
			if !errors.Is(err, tc.want) {
				t.Errorf("err=%v, want %v", err, tc.want)
			}
		})
	}
}

func TestClassifyHTTPResponse_NonJSONBodyIsPlain400(t *testing.T) {
	resp := &http.Response{
		StatusCode: http.StatusBadRequest,
		Body:       io.NopCloser(strings.NewReader("not json at all")),
	}
	err := classifyHTTPResponse(resp, "test")
	if _, ok := CodeOf(err); ok {
		t.Fatalf("CodeOf(%v) = %q, want no typed code for a non-JSON body", err, err)
	}
}

func TestAnthropic_400TypedErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":{"message":"prompt is too long"}}`)
	}))
	defer srv.Close()

	a := mustAnthropicAdapter(t, "claude", srv.URL, "sk-ant", 0)
	a.core.retry.MaxRetries = 0
	_, err := collect(a.GenerateContent(context.Background(), sampleRequest("hi"), false))
	if !errors.Is(err, ErrContextTooLong) {
		t.Errorf("err=%v, want ErrContextTooLong", err)
	}
}

// A transport failure must survive classification with its cause chain
// intact, or retryHTTP cannot recognise it as transient and every connection
// reset is treated as permanent.
func TestClassifyRequestErrorPreservesTransientCause(t *testing.T) {
	for _, cause := range []error{syscall.ECONNRESET, syscall.EPIPE, io.ErrUnexpectedEOF, net.ErrClosed} {
		classified := classifyRequestError(&url.Error{Op: "Post", URL: "https://provider.example", Err: cause})
		if !errors.Is(classified, cause) {
			t.Errorf("classifyRequestError(%v) lost the cause chain: %v", cause, classified)
		}
		if !isTransientError(classified) {
			t.Errorf("classified %v is not recognised as transient", cause)
		}
		if code, ok := CodeOf(classified); !ok || code != ErrorCodeConnectionFailed {
			t.Errorf("CodeOf = %q, %v; want %q", code, ok, ErrorCodeConnectionFailed)
		}
	}
}

func TestRetryHTTPRetriesTransportFailures(t *testing.T) {
	attempts := 0
	cfg := RetryConfig{MaxRetries: 3, Sleep: func(context.Context, time.Duration) error { return nil }}
	resp, err := retryHTTP(context.Background(), cfg, func() (*http.Response, error) {
		attempts++
		return nil, classifyRequestError(&url.Error{Op: "Post", Err: syscall.ECONNRESET})
	})
	if resp != nil {
		_ = resp.Body.Close()
	}
	if err == nil {
		t.Fatal("expected the transport failure to surface after retries")
	}
	if attempts != 4 {
		t.Errorf("attempts = %d, want 4 (initial + 3 retries)", attempts)
	}
}
