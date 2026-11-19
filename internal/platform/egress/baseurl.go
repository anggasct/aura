package egress

import (
	"errors"
	"net"
	"net/url"
	"strings"
)

func ValidateBaseURL(raw string) error {
	if raw == "" {
		return nil
	}
	u, err := url.Parse(raw)
	if err != nil {
		return errors.New("is not a valid URL")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return errors.New("scheme must be http or https")
	}
	if u.Host == "" {
		return errors.New("missing host")
	}
	if u.User != nil {
		return errors.New("user info is not allowed")
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return errors.New("query and fragment are not allowed")
	}
	if u.Scheme == "http" && !isLoopbackURL(u) {
		return errors.New("https is required for non-loopback endpoints")
	}
	return nil
}

func IsLoopbackBaseURL(raw string) bool {
	if raw == "" {
		return false
	}
	u, err := url.Parse(raw)
	return err == nil && isLoopbackURL(u)
}

func isLoopbackURL(u *url.URL) bool {
	host := strings.TrimSuffix(u.Hostname(), ".")
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
