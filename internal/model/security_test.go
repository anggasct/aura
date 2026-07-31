package model

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAPIKeyRedaction(t *testing.T) {
	const secret = "sk-SUPERSECRET-7f3a-9d1c"
	prev := slog.Default()
	var buf bytes.Buffer
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	defer slog.SetDefault(prev)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	oa := mustOpenAIAdapter(t, "gpt-4o", srv.URL, secret, 0)
	oa.retry.MaxRetries = 0
	_, errOpenAI := collect(oa.GenerateContent(context.Background(), sampleRequest("hi"), false))

	aa := mustAnthropicAdapter(t, "claude", srv.URL, secret, 0)
	aa.retry.MaxRetries = 0
	_, errAnthropic := collect(aa.GenerateContent(context.Background(), sampleRequest("hi"), false))

	combined := buf.String()
	if errOpenAI != nil {
		combined += errOpenAI.Error()
	}
	if errAnthropic != nil {
		combined += errAnthropic.Error()
	}
	if strings.Contains(combined, secret) {
		t.Fatalf("API key leaked into logs or error messages:\n%s", combined)
	}
}

func TestAPIKeySentInHeader(t *testing.T) {
	const secret = "sk-HEADER-CHECK-123"
	var gotOpenAI, gotAnthropic string
	openaiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotOpenAI = r.Header.Get("Authorization")
		writeFixture(t, w, fixtureBytes(t, "openai_completion.json"))
	}))
	defer openaiSrv.Close()

	anthropicSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAnthropic = r.Header.Get("x-api-key")
		writeFixture(t, w, fixtureBytes(t, "anthropic_message.json"))
	}))
	defer anthropicSrv.Close()

	oa := mustOpenAIAdapter(t, "gpt-4o", openaiSrv.URL, secret, 0)
	if _, err := collect(oa.GenerateContent(context.Background(), sampleRequest("hi"), false)); err != nil {
		t.Fatalf("openai: %v", err)
	}
	aa := mustAnthropicAdapter(t, "claude", anthropicSrv.URL, secret, 0)
	if _, err := collect(aa.GenerateContent(context.Background(), sampleRequest("hi"), false)); err != nil {
		t.Fatalf("anthropic: %v", err)
	}
	if gotOpenAI != "Bearer "+secret {
		t.Errorf("openai auth = %q", gotOpenAI)
	}
	if gotAnthropic != secret {
		t.Errorf("anthropic key = %q", gotAnthropic)
	}
}
