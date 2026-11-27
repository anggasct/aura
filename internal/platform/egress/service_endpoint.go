package egress

import (
	"errors"
	"net/netip"
	"net/url"
	"path"
	"strconv"
	"strings"
)

const (
	serviceLoopbackIPv4 = "127.0.0.1"
	serviceLoopbackIPv6 = "::1"
)

type ServiceEndpoint struct {
	Scheme   string
	Host     string
	Port     string
	Path     string
	Loopback bool
}

func (e ServiceEndpoint) Authority() string {
	return joinHostPort(e.Host, e.Port)
}

func (e ServiceEndpoint) URL() string {
	return e.Scheme + "://" + e.Authority() + e.Path
}

func joinHostPort(host, port string) string {
	if strings.Contains(host, ":") {
		return "[" + host + "]:" + port
	}
	return host + ":" + port
}

func ParseServiceEndpoint(raw string) (ServiceEndpoint, error) {
	if raw == "" || strings.TrimSpace(raw) != raw {
		return ServiceEndpoint{}, errors.New("must be set and carry no surrounding whitespace")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return ServiceEndpoint{}, errors.New("is not a valid URL")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return ServiceEndpoint{}, errors.New("scheme must be http or https")
	}
	switch {
	case u.Opaque != "" || u.Host == "":
		return ServiceEndpoint{}, errors.New("must have a host")
	case u.User != nil:
		return ServiceEndpoint{}, errors.New("must not embed credentials")
	case u.RawQuery != "" || u.ForceQuery || u.Fragment != "":
		return ServiceEndpoint{}, errors.New("must not carry a query or fragment")
	}
	host := u.Hostname()
	if host == "" || strings.ContainsAny(host, "%\\") {
		return ServiceEndpoint{}, errors.New("host is not valid")
	}
	port, err := servicePort(u)
	if err != nil {
		return ServiceEndpoint{}, err
	}
	endpointPath, err := servicePath(u)
	if err != nil {
		return ServiceEndpoint{}, err
	}
	endpoint := ServiceEndpoint{Scheme: u.Scheme, Host: host, Port: port, Path: endpointPath}
	if host == serviceLoopbackIPv4 || host == serviceLoopbackIPv6 {
		endpoint.Loopback = true
		return endpoint, nil
	}
	if err := rejectLoopbackLookalike(host); err != nil {
		return ServiceEndpoint{}, err
	}
	if u.Scheme != "https" {
		return ServiceEndpoint{}, errors.New("must use https unless the host is exactly 127.0.0.1 or ::1")
	}
	endpoint.Host = strings.ToLower(host)
	if _, err := netip.ParseAddr(host); err == nil {
		if err := ValidateLiteralDestination(endpoint.URL()); err != nil {
			return ServiceEndpoint{}, errors.New("address is not a permitted destination")
		}
	}
	return endpoint, nil
}

func servicePort(u *url.URL) (string, error) {
	port := u.Port()
	if port == "" {
		if u.Scheme == "https" {
			return "443", nil
		}
		return "80", nil
	}
	number, err := strconv.Atoi(port)
	if err != nil || number < 1 || number > 65535 || strconv.Itoa(number) != port {
		return "", errors.New("port must be between 1 and 65535")
	}
	return port, nil
}

func servicePath(u *url.URL) (string, error) {
	escaped := strings.TrimRight(u.EscapedPath(), "/")
	if escaped == "" {
		return "", nil
	}
	if strings.Contains(escaped, "%") || strings.Contains(escaped, "//") || path.Clean(escaped) != escaped {
		return "", errors.New("path must be plain and normalized")
	}
	return escaped, nil
}

func rejectLoopbackLookalike(host string) error {
	name := strings.ToLower(strings.TrimSuffix(host, "."))
	if name == "localhost" || strings.HasSuffix(name, ".localhost") {
		return errors.New("must use the literal 127.0.0.1 or ::1 for a local server, not a loopback name")
	}
	if addr, err := netip.ParseAddr(name); err == nil {
		if addr.Unmap().IsLoopback() {
			return errors.New("loopback address must be written exactly as 127.0.0.1 or ::1")
		}
		return nil
	}
	last := name[strings.LastIndexByte(name, '.')+1:]
	if last == "" || isNumericLabel(last) {
		return errors.New("host form is not accepted")
	}
	return nil
}

func isNumericLabel(label string) bool {
	if strings.HasPrefix(strings.ToLower(label), "0x") {
		return true
	}
	for _, r := range label {
		if r < '0' || r > '9' {
			return false
		}
	}
	return label != ""
}
