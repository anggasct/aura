package egress

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
)

type ErrorCode string

const (
	ErrorCodeEgressDenied ErrorCode = "egress_denied"
)

type Error struct {
	Code   ErrorCode
	Detail string
}

func (e *Error) Error() string {
	return fmt.Sprintf("%s: %s", e.Code, e.Detail)
}

func CodeOf(err error) (ErrorCode, bool) {
	var target *Error
	if !errors.As(err, &target) {
		return "", false
	}
	return target.Code, true
}

func Errorf(code ErrorCode, format string, args ...any) error {
	return &Error{Code: code, Detail: fmt.Sprintf(format, args...)}
}

// Resolver resolves host names to IP addresses. The real resolver uses the
// system DNS; tests inject a controlled resolver to exercise rebinding.
type Resolver interface {
	LookupIP(ctx context.Context, host string) ([]net.IP, error)
}

type systemResolver struct{}

func (systemResolver) LookupIP(ctx context.Context, host string) ([]net.IP, error) {
	return net.DefaultResolver.LookupIP(ctx, "ip", host)
}

// cloudMetadataIPs are the RFC-unspecified metadata endpoints that must
// never be reachable from brokered web access.
var cloudMetadataIPs = []net.IP{
	net.ParseIP("169.254.169.254"),
	net.ParseIP("169.254.169.253"),
}

// Destination is a validated outbound target: the original host plus the
// concrete IP that was checked and must be dialed.
type Destination struct {
	Host string
	IP   net.IP
}

// Validate resolves and validates a destination. Loopback, private,
// link-local, unspecified, multicast, and cloud-metadata addresses are
// rejected by default. The returned IP is the one the dialer must pin, so a
// second DNS lookup cannot redirect to a different address.
func Validate(ctx context.Context, raw string, resolver Resolver) (Destination, error) {
	if resolver == nil {
		resolver = systemResolver{}
	}
	u, err := url.Parse(raw)
	if err != nil {
		return Destination{}, Errorf(ErrorCodeEgressDenied, "invalid destination %q", raw)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return Destination{}, Errorf(ErrorCodeEgressDenied, "scheme %q is not allowed", u.Scheme)
	}
	if u.Host == "" {
		return Destination{}, Errorf(ErrorCodeEgressDenied, "destination has no host")
	}
	host := u.Hostname()
	if strings.TrimSpace(host) == "" {
		return Destination{}, Errorf(ErrorCodeEgressDenied, "destination has no hostname")
	}
	if u.User != nil {
		return Destination{}, Errorf(ErrorCodeEgressDenied, "credentials in the destination URL are not allowed")
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return Destination{}, Errorf(ErrorCodeEgressDenied, "query and fragment are not allowed in the destination URL")
	}

	// A literal IP is validated directly; hostnames are resolved here, once,
	// so the caller dials the checked address (DNS-rebinding defense).
	ip := net.ParseIP(host)
	if ip != nil {
		if err := rejectIP(ip); err != nil {
			return Destination{}, Errorf(ErrorCodeEgressDenied, "%v", err)
		}
		return Destination{Host: host, IP: ip}, nil
	}
	ips, err := resolver.LookupIP(ctx, host)
	if err != nil {
		return Destination{}, Errorf(ErrorCodeEgressDenied, "cannot resolve %q: %v", host, err)
	}
	if len(ips) == 0 {
		return Destination{}, Errorf(ErrorCodeEgressDenied, "no addresses for %q", host)
	}
	for _, ip := range ips {
		if err := rejectIP(ip); err != nil {
			return Destination{}, Errorf(ErrorCodeEgressDenied, "%q resolves to a rejected address: %v", host, err)
		}
	}
	return Destination{Host: host, IP: ips[0]}, nil
}

func rejectIP(ip net.IP) error {
	for _, blocked := range cloudMetadataIPs {
		if ip.Equal(blocked) {
			return fmt.Errorf("cloud metadata address %s", ip)
		}
	}
	switch {
	case ip.IsLoopback():
		return fmt.Errorf("loopback address %s", ip)
	case ip.IsPrivate():
		return fmt.Errorf("private address %s", ip)
	case ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast():
		return fmt.Errorf("link-local address %s", ip)
	case ip.IsUnspecified():
		return fmt.Errorf("unspecified address %s", ip)
	case ip.IsMulticast():
		return fmt.Errorf("multicast address %s", ip)
	}
	return nil
}

// PinnedDialer resolves the destination through Validate and dials the
// checked IP, never a re-resolved name. This closes the DNS-rebinding
// window between validation and connection.
type PinnedDialer struct {
	Resolver Resolver
}

func (d PinnedDialer) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	dest, err := Validate(ctx, "https://"+host, d.Resolver)
	if err != nil {
		return nil, err
	}
	dialer := &net.Dialer{}
	return dialer.DialContext(ctx, network, net.JoinHostPort(dest.IP.String(), port))
}

// NewClient returns an HTTP client whose transport pins validated IPs and
// whose redirect policy revalidates every hop, so a redirect to a private
// destination is rejected after the first allowed request.
func NewClient(resolver Resolver) *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			DialContext: PinnedDialer{Resolver: resolver}.DialContext,
		},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if _, err := Validate(req.Context(), req.URL.String(), resolver); err != nil {
				return err
			}
			if len(via) >= 10 {
				return errors.New("too many redirects")
			}
			return nil
		},
	}
}
