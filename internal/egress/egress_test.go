package egress

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

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

func TestPinnedDialerResolvesOnce(t *testing.T) {
	var calls int
	counting := resolverFunc(func(_ context.Context, host string) ([]net.IP, error) {
		calls++
		if host != "rebind.example" {
			t.Errorf("resolved host %q, want rebind.example", host)
		}
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

	okReq, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, "https://public.example/next", http.NoBody)
	err = client.CheckRedirect(okReq, []*http.Request{{URL: okReq.URL}})
	if err != nil {
		t.Fatalf("CheckRedirect(public) = %v, want nil", err)
	}
}

func TestValidateNoResolverDefaultsToSystem(t *testing.T) {
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

func TestAPIClientAllowsQueriesButStillValidatesTheHost(t *testing.T) {
	resolver := staticResolver{
		"public.example":   {mustParseIP(t, "93.184.216.34")},
		"internal.example": {mustParseIP(t, "10.0.0.5")},
	}
	reached := 0
	next := roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		reached++
		return &http.Response{StatusCode: http.StatusNoContent, Body: http.NoBody, Request: req}, nil
	})
	cases := []struct {
		name    string
		target  func(*url.URL) string
		raw     string
		allowed bool
	}{
		{"api client with query", originTarget, "https://public.example/list?q=a&page=2", true},
		{"api client with empty query marker", originTarget, "https://public.example/list?", true},
		{"api client with private host and query", originTarget, "https://internal.example/list?q=a", false},
		{"full client with query", fullTarget, "https://public.example/list?q=a", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reached = 0
			transport := &validatingTransport{resolver: resolver, next: next, target: tc.target}
			req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, tc.raw, http.NoBody)
			resp, err := transport.RoundTrip(req)
			if resp != nil {
				_ = resp.Body.Close()
			}
			if tc.allowed {
				if err != nil || reached != 1 {
					t.Fatalf("RoundTrip(%q) = %v, reached %d, want it to pass", tc.raw, err, reached)
				}
				return
			}
			if code, ok := CodeOf(err); !ok || code != ErrorCodeEgressDenied || reached != 0 {
				t.Fatalf("RoundTrip(%q) = %v, reached %d, want egress_denied", tc.raw, err, reached)
			}
		})
	}
}

func TestValidateRejectsSpecialPurposeRanges(t *testing.T) {
	denied := map[string]string{
		"this network":         "0.1.2.3",
		"shared address space": "100.64.0.1",
		"tailnet host":         "100.100.100.200",
		"shared space end":     "100.127.255.254",
		"ietf protocol":        "192.0.0.8",
		"benchmarking":         "198.18.0.1",
		"reserved":             "240.0.0.1",
		"broadcast":            "255.255.255.255",
		"mapped shared space":  "::ffff:100.64.0.1",
		"ipv4 compatible":      "::a00:1",
		"nat64":                "64:ff9b::a00:1",
		"local nat64":          "64:ff9b:1::1",
		"discard":              "100::1",
		"teredo":               "2001:0:4136:e378::1",
		"6to4":                 "2002:a00:1::1",
		"as112 ipv4":           "192.31.196.1",
		"amt ipv4":             "192.52.193.1",
		"6to4 relay anycast":   "192.88.99.1",
		"as112 delegation":     "192.175.48.1",
		"dummy ipv6":           "100:0:0:1::1",
		"ipv6 benchmarking":    "2001:2::1",
		"amt ipv6":             "2001:3::1",
		"as112 ipv6":           "2001:4:112::1",
		"orchid":               "2001:10::1",
		"orchid v2":            "2001:20::1",
		"drone entity tags":    "2001:30::1",
		"as112 ipv6 delegated": "2620:4f:8000::1",
		"srv6":                 "5f00::1",
		"site local":           "fec0::1",
	}
	allowed := map[string]string{
		"below shared space": "100.63.255.255",
		"above shared space": "100.128.0.1",
		"public ipv4":        "93.184.216.34",
		"public ipv6":        "2606:2800:220:1::1",
	}
	for name, raw := range denied {
		t.Run("denied "+name, func(t *testing.T) {
			resolver := staticResolver{"target.example": {mustParseIP(t, raw)}}
			if _, err := Validate(t.Context(), "https://target.example/x", resolver); err == nil {
				t.Fatalf("Validate accepted %s", raw)
			}
			dialer := PinnedDialer{Resolver: resolver}
			if conn, err := dialer.DialContext(t.Context(), "tcp", "target.example:443"); err == nil {
				_ = conn.Close()
				t.Fatalf("DialContext accepted %s", raw)
			}
		})
	}
	for name, raw := range allowed {
		t.Run("allowed "+name, func(t *testing.T) {
			resolver := staticResolver{"target.example": {mustParseIP(t, raw)}}
			if _, err := Validate(t.Context(), "https://target.example/x", resolver); err != nil {
				t.Fatalf("Validate: %v", err)
			}
		})
	}
}

func TestValidateLiteralDestinationRejectsNonPublicLiteralsAndNames(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		ok   bool
	}{
		{"public ipv4", "https://203.0.113.5/v1", true},
		{"public ipv6", "https://[2606:4700::1111]/v1", true},
		{"loopback", "https://127.0.0.1/v1", false},
		{"ipv6 loopback", "https://[::1]/v1", false},
		{"private", "https://10.0.0.5/v1", false},
		{"link local metadata", "https://169.254.169.254/v1", false},
		{"hostname", "https://api.example.com/v1", false},
		{"plain http", "http://203.0.113.5/v1", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateLiteralDestination(tc.raw)
			if tc.ok != (err == nil) {
				t.Fatalf("ValidateLiteralDestination(%q) = %v, want ok=%v", tc.raw, err, tc.ok)
			}
			if err != nil {
				if code, coded := CodeOf(err); !coded || code != ErrorCodeEgressDenied {
					t.Fatalf("error = %v, want egress_denied", err)
				}
			}
		})
	}
}

func TestNewLoopbackClientRejectsNonLoopbackAuthorities(t *testing.T) {
	for _, authority := range []string{"10.0.0.5:80", "localhost:80", "127.0.0.2:80", "127.0.0.1", "127.0.0.1:0", "127.0.0.1:65536", "127.0.0.1:080", "[::ffff:127.0.0.1]:80", "example.com:443", ""} {
		t.Run(authority, func(t *testing.T) {
			client, err := NewLoopbackClient(authority)
			if client != nil {
				t.Fatalf("NewLoopbackClient(%q) returned a client", authority)
			}
			if code, ok := CodeOf(err); !ok || code != ErrorCodeEgressDenied {
				t.Fatalf("NewLoopbackClient(%q) = %v, want egress_denied", authority, err)
			}
		})
	}
}

func TestLoopbackClientDialsOnlyItsPinnedAddress(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	authority := strings.TrimPrefix(server.URL, "http://")
	client, err := NewLoopbackClient(authority)
	if err != nil {
		t.Fatalf("NewLoopbackClient: %v", err)
	}
	if client.Transport.(*http.Transport).Proxy != nil {
		t.Fatal("loopback client must not use a proxy")
	}
	request, err := http.NewRequestWithContext(context.Background(), http.MethodGet, server.URL+"/search", http.NoBody)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	response, err := client.Do(request)
	if err != nil {
		t.Fatalf("pinned request: %v", err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d", response.StatusCode)
	}
	_, port, err := net.SplitHostPort(authority)
	if err != nil {
		t.Fatalf("split: %v", err)
	}
	for _, other := range []string{
		"http://127.0.0.1:1/x",
		"http://127.0.0.2:" + port + "/x",
		"http://example.com:" + port + "/x",
	} {
		request, err := http.NewRequestWithContext(context.Background(), http.MethodGet, other, http.NoBody)
		if err != nil {
			t.Fatalf("new request: %v", err)
		}
		response, err := client.Do(request)
		if response != nil {
			_ = response.Body.Close()
		}
		if code, ok := CodeOf(err); !ok || code != ErrorCodeEgressDenied {
			t.Fatalf("Do(%q) = %v, want egress_denied", other, err)
		}
	}
}
