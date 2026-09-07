package fetch

import (
	"context"
	"errors"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/egress"
)

func TestFetchTimesOut(t *testing.T) {
	adapter, err := New(Options{
		Timeout:         50 * time.Millisecond,
		MaxRedirects:    2,
		MaxEncodedBytes: 1024,
		MaxDecodedBytes: 1024,
		client: cannedClient(func(req *http.Request) (*http.Response, error) {
			<-req.Context().Done()
			return nil, req.Context().Err()
		}),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = adapter(context.Background(), fetchRequest("https://public.example/slow"), constraints())
	if err == nil {
		t.Fatal("slow origin returned without error")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want context.DeadlineExceeded (broker maps it to deadline_exceeded)", err)
	}
}

func TestFetchResolvesOnceAgainstRebindingResolver(t *testing.T) {
	var calls int
	rebinding := resolverFunc(func(_ context.Context, host string) ([]net.IP, error) {
		calls++
		if host != "rebind.example" {
			t.Errorf("resolved host %q, want rebind.example", host)
		}
		if calls == 1 {
			return []net.IP{net.ParseIP("192.0.2.1")}, nil
		}
		return []net.IP{net.ParseIP("10.0.0.8")}, nil
	})
	adapter, err := New(Options{
		Timeout:         500 * time.Millisecond,
		MaxRedirects:    2,
		MaxEncodedBytes: 1024,
		MaxDecodedBytes: 1024,
		Resolver:        rebinding,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	_, firstErr := adapter(context.Background(), fetchRequest("https://rebind.example/doc"), constraints())
	if firstErr == nil {
		t.Fatal("dial unexpectedly succeeded into TEST-NET space")
	}
	if _, ok := egress.CodeOf(firstErr); ok {
		t.Fatalf("first attempt err = %v, want a dial failure, not egress_denied", firstErr)
	}
	if calls != 1 {
		t.Fatalf("resolver calls after first attempt = %d, want exactly 1 (pinned destination, no re-resolution)", calls)
	}

	_, secondErr := adapter(context.Background(), fetchRequest("https://rebind.example/doc"), constraints())
	if code, ok := egress.CodeOf(secondErr); !ok || code != egress.ErrorCodeEgressDenied {
		t.Fatalf("second attempt err = %v, want egress_denied for the private answer", secondErr)
	}
	if calls != 2 {
		t.Fatalf("resolver calls = %d, want 2 (one per attempt)", calls)
	}
}

func TestFetchPinsAnswerAcrossMultiAnswerLookup(t *testing.T) {
	var calls int
	multi := resolverFunc(func(_ context.Context, host string) ([]net.IP, error) {
		calls++
		return []net.IP{net.ParseIP("192.0.2.10"), net.ParseIP("198.51.100.20")}, nil
	})
	adapter, err := New(Options{
		Timeout:         500 * time.Millisecond,
		MaxRedirects:    2,
		MaxEncodedBytes: 1024,
		MaxDecodedBytes: 1024,
		Resolver:        multi,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, dialErr := adapter(context.Background(), fetchRequest("https://multi.example/doc"), constraints())
	if dialErr == nil {
		t.Fatal("dial unexpectedly succeeded into documentation ranges")
	}
	if calls != 1 {
		t.Fatalf("resolver calls = %d, want 1", calls)
	}
}

func TestFetchRejectsSchemeDowngradeRedirect(t *testing.T) {
	resolver := staticResolver{"public.example": {net.ParseIP("93.184.216.34")}}
	client := egress.NewClient(resolver)
	if client.CheckRedirect == nil {
		t.Fatal("CheckRedirect must be set")
	}
	downgraded, err := http.NewRequestWithContext(context.Background(), http.MethodGet, "http://public.example/plain", http.NoBody)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	original, err := http.NewRequestWithContext(context.Background(), http.MethodGet, "https://public.example/start", http.NoBody)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	err = client.CheckRedirect(downgraded, []*http.Request{original})
	if code, ok := egress.CodeOf(err); !ok || code != egress.ErrorCodeEgressDenied {
		t.Fatalf("CheckRedirect(downgrade) = %v, want egress_denied", err)
	}
	sameScheme, err := http.NewRequestWithContext(context.Background(), http.MethodGet, "https://public.example/next", http.NoBody)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if err := client.CheckRedirect(sameScheme, []*http.Request{original}); err != nil {
		t.Fatalf("CheckRedirect(same scheme) = %v, want allowed", err)
	}
}
