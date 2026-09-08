//go:build durable

package restate

import (
	"context"
	"errors"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/durable"
)

func liveIngressURL(t *testing.T) string {
	t.Helper()
	if url := os.Getenv("AURA_RESTATE_INGRESS"); url != "" {
		return url
	}
	return "http://127.0.0.1:8080"
}

func waitForIngress(t *testing.T, baseURL string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, baseURL+"/restate/health", http.NoBody)
		if err != nil {
			t.Fatalf("health request: %v", err)
		}
		response, err := http.DefaultClient.Do(request)
		if err == nil {
			_ = response.Body.Close()
			if response.StatusCode == http.StatusOK {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("restate ingress at %s never became ready", baseURL)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

func TestLiveStatusUnknownRun(t *testing.T) {
	url := liveIngressURL(t)
	waitForIngress(t, url)
	adapter, err := NewAdapter(Config{IngressURL: url}, nil)
	if err != nil {
		t.Fatalf("NewAdapter: %v", err)
	}
	adapter.RegisterHandler("greet", func(_ context.Context, _ *durable.Invocation) error { return nil })
	if _, err := adapter.Status(t.Context(), durable.RunRef{Key: "run-never-existed"}); !errors.Is(err, durable.ErrUnknownRun) {
		t.Errorf("status err = %v, want %v", err, durable.ErrUnknownRun)
	}
}
