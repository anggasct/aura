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

// TestFetchTimesOut proves a slow origin produces a deadline error the
// broker maps to deadline_exceeded, not a hang or an unclassified failure.
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

// TestFetchResolvesOnceAgainstRebindingResolver runs the production
// construction path (mediated egress client, no canned transport) against a
// rebinding resolver: the first lookup returns a public address, every later
// lookup a private one. Within an attempt the destination must be resolved
// exactly once and pinned; after the rebinding flip the next attempt must be
// denied at validation. A DNS change can therefore never redirect a dial
// after validation.
func TestFetchResolvesOnceAgainstRebindingResolver(t *testing.T) {
	var calls int
	rebinding := resolverFunc(func(_ context.Context, host string) ([]net.IP, error) {
		calls++
		if host != "rebind.example" {
			t.Errorf("resolved host %q, want rebind.example", host)
		}
		// 192.0.2.0/24 is the TEST-NET documentation range: it passes the
		// address-class checks and never routes, so the dial fails without
		// depending on any network.
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

	// After the rebinding flip the next lookup is private and must be
	// denied before any dial.
	_, secondErr := adapter(context.Background(), fetchRequest("https://rebind.example/doc"), constraints())
	if code, ok := egress.CodeOf(secondErr); !ok || code != egress.ErrorCodeEgressDenied {
		t.Fatalf("second attempt err = %v, want egress_denied for the private answer", secondErr)
	}
	if calls != 2 {
		t.Fatalf("resolver calls = %d, want 2 (one per attempt)", calls)
	}
}

// TestFetchPinsAnswerAcrossMultiAnswerLookup feeds a multi-answer lookup
// whose answers are all public: the dial must use an answer the validation
// checked, and with an unroutable answer set the attempt fails without
// ever succeeding through an unchecked address.
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

// TestFetchRejectsSchemeDowngradeRedirect proves an https-to-http redirect
// on the same host is rejected: the cross-origin rule pins both host and
// scheme, so a downgrade can never be followed.
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
