package model

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/config"

	adkmodel "google.golang.org/adk/v2/model"
)

func TestStreamStopSentinelIsDistinctFromEOF(t *testing.T) {
	if errors.Is(errStopYield, io.EOF) {
		t.Error("errStopYield aliases io.EOF — a dropped connection would look like a clean stop")
	}
}

func TestAnthropicStreamAcceptsCRLF(t *testing.T) {
	fixture := fixtureBytes(t, "anthropic_stream.txt")
	crlf := strings.ReplaceAll(string(fixture), "\n", "\r\n")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(crlf))
	}))
	defer srv.Close()

	a := mustAnthropicAdapter(t, "claude-sonnet-4-20250514", srv.URL, "sk-ant", 0)
	resps, err := collect(a.GenerateContent(context.Background(), sampleRequest("hi"), true))
	if err != nil {
		t.Fatalf("GenerateContent: %v", err)
	}
	var final *adkmodel.LLMResponse
	for _, r := range resps {
		if r.TurnComplete {
			final = r
		}
	}
	if final == nil {
		t.Fatal("expected a final response, got none")
	}
}

func TestAnthropicStreamTruncatedBeforeTerminal(t *testing.T) {
	fixture := fixtureBytes(t, "anthropic_stream.txt")
	// Cut at the event boundary before message_stop so the remaining stream
	// is complete events that simply never reach the terminal.
	stopAt := bytes.Index(fixture, []byte(`"message_stop"`))
	eventStart := bytes.LastIndex(fixture[:stopAt], []byte("\n\n"))
	truncated := fixture[:eventStart+2]
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write(truncated)
	}))
	defer srv.Close()

	a := mustAnthropicAdapter(t, "claude-sonnet-4-20250514", srv.URL, "sk-ant", 0)
	resps, err := collect(a.GenerateContent(context.Background(), sampleRequest("hi"), true))
	if err == nil {
		t.Fatal("expected truncation error, got nil")
	}
	wantCode(t, err, ErrorCodeStreamInvalid)
	if len(resps) == 0 {
		t.Error("partials should still be delivered before the truncation error")
	}
}

func TestReadCappedRejectsOversizedBody(t *testing.T) {
	big := bytes.NewReader(bytes.Repeat([]byte("x"), maxResponseBytes+1))
	if _, err := readCapped(big); err == nil {
		t.Error("readCapped accepted a body above the size limit")
	}
}

func TestRetryIncludes408(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			w.WriteHeader(http.StatusRequestTimeout)
			return
		}
		writeFixture(t, w, fixtureBytes(t, "openai_completion.json"))
	}))
	defer srv.Close()

	a := mustOpenAIAdapter(t, "gpt-4o", srv.URL, "sk-test", 0)
	a.core.retry = RetryConfig{MaxRetries: 1, BaseDelay: time.Millisecond, MaxDelay: time.Millisecond, Sleep: func(context.Context, time.Duration) error { return nil }}
	resps, err := collect(a.GenerateContent(context.Background(), sampleRequest("hi"), false))
	if err != nil || len(resps) != 1 {
		t.Fatalf("responses=%d err=%v, want success after 408 retry", len(resps), err)
	}
	if calls != 2 {
		t.Errorf("calls = %d, want 2 (one retry)", calls)
	}
}

func TestIsTransientError(t *testing.T) {
	if !isTransientError(io.ErrUnexpectedEOF) {
		t.Error("io.ErrUnexpectedEOF should be transient")
	}
	if isTransientError(context.Canceled) {
		t.Error("context.Canceled must not be transient")
	}
}

func TestRegisterAdaptersValidatesAllBeforeRegistering(t *testing.T) {
	t.Setenv("TEST_MODEL_API_KEY", "k")
	valid := configuredDefinition(config.ProtocolOpenAIChatCompat, "atomic-reg-test-1", "https://api.openai.com")
	invalid := config.ModelDefinition{Protocol: config.ProtocolAnthropicMessages, Model: "atomic-reg-test-1", BaseURL: "https://api.anthropic.com"}
	models := config.Models{Definitions: map[string]config.ModelDefinition{"primary": valid, "auxiliary": invalid}}

	// auxiliary is invalid (no secret and a non-loopback endpoint), so the
	// whole registration must fail without registering primary.
	if err := RegisterAdapters(nil, models); err == nil {
		t.Fatal("expected registration failure for invalid auxiliary")
	}
	// If primary had been registered despite the failure, registering it
	// again would hit the duplicate check.
	if err := RegisterAdapters(nil, config.Models{Definitions: map[string]config.ModelDefinition{"primary": valid}}); err != nil {
		t.Fatalf("re-register after failed batch: %v", err)
	}
}
