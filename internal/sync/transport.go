package sync

import (
	"context"
	"net/url"
	"strings"

	"github.com/anggasct/aura/internal/config"
	"github.com/anggasct/aura/internal/egress"
	"github.com/anggasct/aura/internal/secret"
)

type Transport struct {
	Scheme      string
	Host        string
	Destination egress.Destination
	Secret      secret.Reference
	KnownHosts  secret.Reference
}

func parseSecretRef(raw string) (secret.Reference, error) {
	switch {
	case strings.HasPrefix(raw, "env://"):
		name := strings.TrimPrefix(raw, "env://")
		if strings.TrimSpace(name) == "" {
			return secret.Reference{}, Errorf(ErrorCodeCredentialInvalid, "secret reference has no name")
		}
		return secret.Reference{Env: name}, nil
	case strings.HasPrefix(raw, "file://"):
		name := strings.TrimPrefix(raw, "file://")
		if strings.TrimSpace(name) == "" || !strings.HasPrefix(name, "/") {
			return secret.Reference{}, Errorf(ErrorCodeCredentialInvalid, "secret file reference must be absolute")
		}
		return secret.Reference{File: name}, nil
	default:
		return secret.Reference{}, Errorf(ErrorCodeCredentialInvalid, "secret reference must use env:// or file://")
	}
}

func ValidateTransportShape(cfg *config.Sync) error {
	if cfg == nil {
		return errNilArgument("cfg")
	}
	u, err := url.Parse(cfg.Remote)
	if err != nil {
		return Errorf(ErrorCodeTransportUnsafe, "remote is not a valid URL")
	}
	if u.User != nil {
		if u.Scheme != "ssh" {
			return Errorf(ErrorCodeTransportUnsafe, "remote must not embed credentials")
		}
		if _, hasPassword := u.User.Password(); hasPassword {
			return Errorf(ErrorCodeTransportUnsafe, "remote must not embed a password")
		}
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return Errorf(ErrorCodeTransportUnsafe, "remote must not carry query or fragment")
	}
	if _, err := parseSecretRef(cfg.TransportSecretRef); err != nil {
		return err
	}
	if _, err := parseSecretRef(cfg.KnownHostsRef); err != nil {
		return err
	}
	switch u.Scheme {
	case "https":
		if err := egress.ValidateDestinationShape("https://" + u.Host); err != nil {
			return Errorf(ErrorCodeEgressDenied, "remote destination is not permitted")
		}
	case "ssh":
		if u.Port() != "" {
			if err := egress.ValidateDestinationShape("https://" + u.Host); err != nil {
				return Errorf(ErrorCodeEgressDenied, "remote destination is not permitted")
			}
		} else {
			if err := egress.ValidateDestinationShape("https://" + u.Hostname()); err != nil {
				return Errorf(ErrorCodeEgressDenied, "remote destination is not permitted")
			}
		}
	default:
		return Errorf(ErrorCodeTransportUnsafe, "remote scheme must be ssh or https")
	}
	return nil
}

func ResolveTransport(ctx context.Context, cfg *config.Sync, resolver egress.Resolver) (Transport, error) {
	if ctx == nil {
		return Transport{}, errNilArgument("ctx")
	}
	if cfg == nil {
		return Transport{}, errNilArgument("cfg")
	}
	u, err := url.Parse(cfg.Remote)
	if err != nil {
		return Transport{}, Errorf(ErrorCodeTransportUnsafe, "remote is not a valid URL")
	}
	if u.User != nil {
		if u.Scheme != "ssh" {
			return Transport{}, Errorf(ErrorCodeTransportUnsafe, "remote must not embed credentials")
		}
		if _, hasPassword := u.User.Password(); hasPassword {
			return Transport{}, Errorf(ErrorCodeTransportUnsafe, "remote must not embed a password")
		}
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return Transport{}, Errorf(ErrorCodeTransportUnsafe, "remote must not carry query or fragment")
	}
	transport := Transport{Scheme: u.Scheme, Host: u.Hostname()}
	transport.Secret, err = parseSecretRef(cfg.TransportSecretRef)
	if err != nil {
		return Transport{}, err
	}
	transport.KnownHosts, err = parseSecretRef(cfg.KnownHostsRef)
	if err != nil {
		return Transport{}, err
	}
	switch u.Scheme {
	case "https":
		destination, err := egress.Validate(ctx, "https://"+u.Host, resolver)
		if err != nil {
			return Transport{}, Errorf(ErrorCodeEgressDenied, "remote destination is not permitted")
		}
		transport.Destination = destination
	case "ssh":
		if u.Port() != "" {
			if _, err := egress.Validate(ctx, "https://"+u.Host, resolver); err != nil {
				return Transport{}, Errorf(ErrorCodeEgressDenied, "remote destination is not permitted")
			}
		} else {
			if _, err := egress.Validate(ctx, "https://"+u.Hostname(), resolver); err != nil {
				return Transport{}, Errorf(ErrorCodeEgressDenied, "remote destination is not permitted")
			}
		}
	default:
		return Transport{}, Errorf(ErrorCodeTransportUnsafe, "remote scheme must be ssh or https")
	}
	return transport, nil
}
