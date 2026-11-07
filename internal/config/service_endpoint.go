package config

import (
	"errors"
	"net/netip"
	"net/url"
	"path"
	"strconv"
	"strings"

	"github.com/anggasct/aura/internal/egress"
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

func parseServiceEndpoint(field, raw string) (ServiceEndpoint, error) {
	if raw == "" || strings.TrimSpace(raw) != raw {
		return ServiceEndpoint{}, errors.New(field + " must be set and carry no surrounding whitespace")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return ServiceEndpoint{}, errors.New(field + " is not a valid URL")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return ServiceEndpoint{}, errors.New(field + " scheme must be http or https")
	}
	switch {
	case u.Opaque != "" || u.Host == "":
		return ServiceEndpoint{}, errors.New(field + " must have a host")
	case u.User != nil:
		return ServiceEndpoint{}, errors.New(field + " must not embed credentials")
	case u.RawQuery != "" || u.ForceQuery || u.Fragment != "":
		return ServiceEndpoint{}, errors.New(field + " must not carry a query or fragment")
	}
	host := u.Hostname()
	if host == "" || strings.ContainsAny(host, "%\\") {
		return ServiceEndpoint{}, errors.New(field + " host is not valid")
	}
	port, err := servicePort(field, u)
	if err != nil {
		return ServiceEndpoint{}, err
	}
	endpointPath, err := servicePath(field, u)
	if err != nil {
		return ServiceEndpoint{}, err
	}
	endpoint := ServiceEndpoint{Scheme: u.Scheme, Host: host, Port: port, Path: endpointPath}
	if host == serviceLoopbackIPv4 || host == serviceLoopbackIPv6 {
		endpoint.Loopback = true
		return endpoint, nil
	}
	if err := rejectLoopbackLookalike(field, host); err != nil {
		return ServiceEndpoint{}, err
	}
	if u.Scheme != "https" {
		return ServiceEndpoint{}, errors.New(field + " must use https unless the host is exactly 127.0.0.1 or ::1")
	}
	endpoint.Host = strings.ToLower(host)
	if _, err := netip.ParseAddr(host); err == nil {
		if err := egress.ValidateLiteralDestination(endpoint.URL()); err != nil {
			return ServiceEndpoint{}, errors.New(field + " address is not a permitted destination")
		}
	}
	return endpoint, nil
}

func servicePort(field string, u *url.URL) (string, error) {
	port := u.Port()
	if port == "" {
		if u.Scheme == "https" {
			return "443", nil
		}
		return "80", nil
	}
	number, err := strconv.Atoi(port)
	if err != nil || number < 1 || number > 65535 || strconv.Itoa(number) != port {
		return "", errors.New(field + " port must be between 1 and 65535")
	}
	return port, nil
}

func servicePath(field string, u *url.URL) (string, error) {
	escaped := strings.TrimRight(u.EscapedPath(), "/")
	if escaped == "" {
		return "", nil
	}
	if strings.Contains(escaped, "%") || strings.Contains(escaped, "//") || path.Clean(escaped) != escaped {
		return "", errors.New(field + " path must be plain and normalized")
	}
	return escaped, nil
}

func rejectLoopbackLookalike(field, host string) error {
	name := strings.ToLower(strings.TrimSuffix(host, "."))
	if name == "localhost" || strings.HasSuffix(name, ".localhost") {
		return errors.New(field + " must use the literal 127.0.0.1 or ::1 for a local server, not a loopback name")
	}
	if addr, err := netip.ParseAddr(name); err == nil {
		if addr.Unmap().IsLoopback() {
			return errors.New(field + " loopback address must be written exactly as 127.0.0.1 or ::1")
		}
		return nil
	}
	last := name[strings.LastIndexByte(name, '.')+1:]
	if last == "" || isNumericLabel(last) {
		return errors.New(field + " host form is not accepted")
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
