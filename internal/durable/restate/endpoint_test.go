package restate

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/durable"
)

func startTestEndpoint(t *testing.T) (*Endpoint, context.CancelFunc) {
	t.Helper()
	endpoint, err := NewEndpoint(EndpointConfig{HandlerAddr: "127.0.0.1:0"}, nil)
	if err != nil {
		t.Fatalf("NewEndpoint: %v", err)
	}
	endpoint.RegisterHandler("workflow", func(context.Context, durable.Invocation) error { return nil })
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- endpoint.Start(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("endpoint Start: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("endpoint did not stop after cancel")
		}
	})
	return endpoint, cancel
}

func TestEndpointServesHealthAndDiscovery(t *testing.T) {
	endpoint, _ := startTestEndpoint(t)
	bound := endpoint.BoundAddr()
	if bound == nil {
		t.Fatal("endpoint never bound a listener")
	}
	base := "http://" + bound.String()

	deadline := time.Now().Add(5 * time.Second)
	for {
		request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, base+"/health", http.NoBody)
		if err != nil {
			t.Fatalf("health request: %v", err)
		}
		response, err := http.DefaultClient.Do(request)
		if err == nil {
			_ = response.Body.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("endpoint never became ready: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}

	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, base+"/discover", http.NoBody)
	if err != nil {
		t.Fatalf("discovery request: %v", err)
	}
	request.Header.Set("accept", "application/vnd.restate.endpointmanifest.v4+json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("discovery: %v", err)
	}
	body, _ := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("discovery status = %d, want 200", response.StatusCode)
	}
	for _, want := range []string{"WorkflowRun", runHandlerName, signalHandlerName, statusHandlerName} {
		if !strings.Contains(string(body), want) {
			t.Errorf("discovery body misses %q:\n%s", want, body)
		}
	}
}

func TestEndpointStopWithoutStartFails(t *testing.T) {
	endpoint, err := NewEndpoint(EndpointConfig{}, nil)
	if err != nil {
		t.Fatalf("NewEndpoint: %v", err)
	}
	if err := endpoint.Stop(context.Background()); err == nil {
		t.Error("expected Stop without Start to fail, got nil")
	}
}

func TestEndpointDefaults(t *testing.T) {
	endpoint, err := NewEndpoint(EndpointConfig{}, nil)
	if err != nil {
		t.Fatalf("NewEndpoint: %v", err)
	}
	if endpoint.Addr() != defaultHandlerAddr {
		t.Errorf("addr = %q, want %q", endpoint.Addr(), defaultHandlerAddr)
	}
}
