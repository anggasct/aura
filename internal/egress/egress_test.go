package egress

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// staticResolver returns a fixed set of IPs per host, so tests can exercise
// DNS-rebinding and address-class rejection without real DNS.
type staticResolver map[string][]net.IP

func (r staticResolver) LookupIP(_ context.Context, host string) ([]net.IP, error) {
	ips, ok := r[host]
	if !ok {
		return nil, &net.DNSError{Err: "no such host", Name: host}
	}
	return ips, nil
}

func mustParseIP(t *testing.T, raw string) net.IP {
	t.Helper()
	ip := net.ParseIP(raw)
	if ip == nil {
		t.Fatalf("invalid IP %q", raw)
	}
	return ip
}

func TestValidateRejectsPrivateDestinations(t *testing.T) {
	ctx := context.Background()
	resolver := staticResolver{
		"internal.example":     {mustParseIP(t, "10.0.0.5")},
		"nat.example":          {mustParseIP(t, "192.168.1.10")},
		"loopback.example":     {mustParseIP(t, "127.0.0.1")},
		"linklocal.example":    {mustParseIP(t, "169.254.10.20")},
		"ipv6loopback.example": {mustParseIP(t, "::1")},
		"public.example":       {mustParseIP(t, "93.184.216.34")},
		"mixed.example":        {mustParseIP(t, "93.184.216.34"), mustParseIP(t, "10.0.0.9")},
	}
	cases := []struct {
		raw string
	}{
		{raw: "https://internal.example/x"},
		{raw: "https://nat.example/x"},
		{raw: "https://loopback.example/x"},
		{raw: "https://linklocal.example/x"},
		{raw: "https://ipv6loopback.example/x"},
		{raw: "https://mixed.example/x"},
		{raw: "https://10.0.0.1/x"},
		{raw: "https://127.0.0.1/x"},
		{raw: "https://[::1]/x"},
		{raw: "https://169.254.169.254/latest/meta-data/"},
		{raw: "https://169.254.169.253/x"},
		{raw: "ftp://example.com/x"},
		{raw: "https://user:pass@example.com/x"},
		{raw: "https://example.com/x?q=1"},
		{raw: "https://example.com/x#frag"},
	}
	for _, tc := range cases {
		t.Run(tc.raw, func(t *testing.T) {
			_, err := Validate(ctx, tc.raw, resolver)
			if code, ok := CodeOf(err); !ok || code != ErrorCodeEgressDenied {
				t.Fatalf("Validate(%q) = %v, want egress_denied", tc.raw, err)
			}
		})
	}

	dest, err := Validate(ctx, "https://public.example/x", resolver)
	if err != nil {
		t.Fatalf("Validate(public): %v", err)
	}
	if !dest.IP.Equal(mustParseIP(t, "93.184.216.34")) {
		t.Errorf("IP = %v", dest.IP)
	}
}

func TestValidateRejectsUnixSocketAndEmptyHost(t *testing.T) {
	ctx := context.Background()
	resolver := staticResolver{}
	for _, raw := range []string{"https:///var/run/x.sock", "unix:///var/run/x.sock", "https://"} {
		_, err := Validate(ctx, raw, resolver)
		if code, ok := CodeOf(err); !ok || code != ErrorCodeEgressDenied {
			t.Fatalf("Validate(%q) = %v, want egress_denied", raw, err)
		}
	}
}

// DNS rebinding: the first lookup returns a public IP, a later lookup for
// the same host would return a private one. The pinned dialer must resolve
// exactly once and dial the validated IP, never re-resolve.
func TestPinnedDialerResolvesOnce(t *testing.T) {
	var calls int
	counting := resolverFunc(func(_ context.Context, host string) ([]net.IP, error) {
		calls++
		if host != "rebind.example" {
			t.Errorf("resolved host %q, want rebind.example", host)
		}
		// First lookup would be public; a re-lookup (rebinding) would be
		// private. The dialer must not re-resolve, so only one call happens
		// and the returned address is used as-is. 192.0.2.1 is the TEST-NET
		// documentation range: passes the address-class checks, never
		// dials anything.
		return []net.IP{mustParseIP(t, "192.0.2.1")}, nil
	})

	dialer := PinnedDialer{Resolver: counting}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := dialer.DialContext(ctx, "tcp", "rebind.example:443")
	if conn != nil {
		_ = conn.Close()
	}
	if err == nil {
		t.Fatal("expected dial to fail (documentation range is unroutable)")
	}
	if calls != 1 {
		t.Fatalf("resolver calls = %d, want exactly 1 (pinning, no re-resolution)", calls)
	}
}

// resolverFunc adapts a function to the Resolver interface.
type resolverFunc func(context.Context, string) ([]net.IP, error)

func (f resolverFunc) LookupIP(ctx context.Context, host string) ([]net.IP, error) {
	return f(ctx, host)
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestValidatingTransportPinsTheValidatedDestination(t *testing.T) {
	calls := 0
	resolver := resolverFunc(func(context.Context, string) ([]net.IP, error) {
		calls++
		return []net.IP{mustParseIP(t, "93.184.216.34")}, nil
	})
	transport := &validatingTransport{
		resolver: resolver,
		next: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
			destination, ok := destinationFromContext(req.Context())
			if !ok || !destination.IP.Equal(mustParseIP(t, "93.184.216.34")) {
				t.Fatalf("request destination = %+v, ok=%v; want pinned public IP", destination, ok)
			}
			return &http.Response{StatusCode: http.StatusNoContent, Body: http.NoBody, Request: req}, nil
		}),
	}
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "https://public.example/path", http.NoBody)
	resp, err := transport.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	_ = resp.Body.Close()
	if calls != 1 {
		t.Fatalf("resolver calls = %d, want one validation lookup", calls)
	}
}

func TestPinnedDialerDeniesPrivateResolution(t *testing.T) {
	resolver := staticResolver{"rebind.example": {mustParseIP(t, "10.0.0.5")}}
	dialer := PinnedDialer{Resolver: resolver}
	conn, err := dialer.DialContext(context.Background(), "tcp", "rebind.example:80")
	if conn != nil {
		_ = conn.Close()
		t.Fatal("expected dial to be denied, got a connection")
	}
	if code, ok := CodeOf(err); !ok || code != ErrorCodeEgressDenied {
		t.Fatalf("dial error = %v, want egress_denied", err)
	}
}

func TestClientRejectsInvalidInitialRequest(t *testing.T) {
	resolver := staticResolver{"public.example": {net.ParseIP("93.184.216.34")}}
	client := NewClient(resolver)
	cases := []string{
		"https://user:pass@public.example/x",
		"https://public.example/x?q=1",
		"https://public.example/x#frag",
		"ftp://public.example/x",
		"https://10.0.0.1/x",
	}
	for _, raw := range cases {
		t.Run(raw, func(t *testing.T) {
			req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, raw, http.NoBody)
			if err != nil {
				t.Fatalf("new request: %v", err)
			}
			resp, err := client.Do(req)
			if code, ok := CodeOf(err); !ok || code != ErrorCodeEgressDenied {
				if resp != nil {
					_ = resp.Body.Close()
				}
				t.Fatalf("Do(%q) = %v, want egress_denied", raw, err)
			}
		})
	}
}

func TestNewClientRejectsCrossOriginRedirect(t *testing.T) {
	resolver := staticResolver{
		"public.example": {net.ParseIP("93.184.216.34")},
		"other.example":  {net.ParseIP("93.184.216.35")},
	}
	client := NewClient(resolver)
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, "https://other.example/next", http.NoBody)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	first, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, "https://public.example/start", http.NoBody)
	err = client.CheckRedirect(req, []*http.Request{first})
	if code, ok := CodeOf(err); !ok || code != ErrorCodeEgressDenied {
		t.Fatalf("CheckRedirect(cross-origin) = %v, want egress_denied", err)
	}
	if !strings.Contains(err.Error(), "cross-origin") {
		t.Fatalf("error should name cross-origin rejection: %v", err)
	}
}

func TestNewClientRejectsRedirectToPrivate(t *testing.T) {
	// First hop is a public-ish server (here, the test server itself via a
	// host the resolver maps to a dialable address); the redirect target
	// resolves to a private IP and must be rejected by CheckRedirect.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://metadata.internal/steal", http.StatusFound)
	}))
	defer srv.Close()

	host, port, err := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		t.Fatalf("split: %v", err)
	}
	_ = port

	resolver := staticResolver{
		host:                {net.ParseIP("127.0.0.1")},
		"metadata.internal": {net.ParseIP("169.254.169.254")},
		"public.example":    {net.ParseIP("93.184.216.34")},
	}
	// httptest always binds loopback, so resolution of the first hop is
	// loopback and would be denied before the redirect is even followed.
	// The CheckRedirect logic is unit-tested directly instead.
	client := NewClient(resolver)
	if client.CheckRedirect == nil {
		t.Fatal("CheckRedirect must be set")
	}
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, "https://metadata.internal/steal", http.NoBody)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	err = client.CheckRedirect(req, []*http.Request{{URL: req.URL}})
	if code, ok := CodeOf(err); !ok || code != ErrorCodeEgressDenied {
		t.Fatalf("CheckRedirect = %v, want egress_denied for metadata redirect", err)
	}

	// An allowed redirect (public resolution) passes CheckRedirect.
	okReq, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, "https://public.example/next", http.NoBody)
	err = client.CheckRedirect(okReq, []*http.Request{{URL: okReq.URL}})
	if err != nil {
		t.Fatalf("CheckRedirect(public) = %v, want nil", err)
	}
}

func TestValidateNoResolverDefaultsToSystem(t *testing.T) {
	// No resolver provided: Validate must not panic and must return a
	// typed error for an unroutable host rather than crashing.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := Validate(ctx, "https://definitely-not-a-real-host.invalid/x", nil)
	if err == nil {
		t.Fatal("expected an error for an unresolvable host")
	}
}

func TestValidateRequiresHTTPS(t *testing.T) {
	resolver := staticResolver{"public.example": {net.ParseIP("93.184.216.34")}}

	if _, err := Validate(context.Background(), "https://public.example/v1", resolver); err != nil {
		t.Fatalf("Validate(https) = %v, want allowed", err)
	}

	_, err := Validate(context.Background(), "http://public.example/v1", resolver)
	code, ok := CodeOf(err)
	if !ok || code != ErrorCodeEgressDenied {
		t.Fatalf("Validate(http) = %v, want egress_denied", err)
	}
	if !strings.Contains(err.Error(), "https is required") {
		t.Fatalf("error should name the https requirement: %v", err)
	}
}
