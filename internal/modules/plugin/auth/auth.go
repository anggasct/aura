package auth

import (
	"context"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"slices"
	"time"

	"github.com/anggasct/aura/internal/config"
)

const (
	KindNone         = "none"
	KindStaticSecret = "static_secret"
	KindOAuth2       = "oauth2"

	DefaultHeader = "Authorization"

	LoginTimeout = 5 * time.Minute
)

type Strategy interface {
	Ready(ctx context.Context) error
	Authorize(ctx context.Context, req *http.Request) error
}

type Interactive interface {
	Strategy
	Login(ctx context.Context, opts *LoginOptions) error
}

type LoginOptions struct {
	Paste   bool
	Input   io.Reader
	Prompt  func(authorizeURL string)
	Timeout time.Duration
}

type SecretResolver interface {
	ResolveSecret(ctx context.Context, ref string) (string, error)
}

type Redactor interface {
	RegisterSecret(value string)
}

type Spec struct {
	PluginID        string
	Kind            string
	Fingerprint     string
	Header          string
	SecretRef       string
	AuthorizeURL    string
	TokenURL        string
	RedirectURI     string
	Scopes          []string
	ClientIDRef     string
	ClientSecretRef string
	AuthorizeParams map[string]string
	AccessOnly      bool
	Issuer          string
}

var authorizeParamValues = map[string][]string{
	"access_type": {"offline", "online"},
	"prompt":      {"consent", "select_account", "none"},
}

func ValidAuthorizeParam(key, value string) bool {
	return slices.Contains(authorizeParamValues[key], value)
}

type Options struct {
	Dir        string
	Secrets    SecretResolver
	HTTPClient *http.Client
	Redactor   Redactor
	Now        func() time.Time
	Listen     func(ctx context.Context, address string) (net.Listener, error)
}

type factory func(r *Registry, spec *Spec) (Strategy, error)

type Registry struct {
	dir       string
	secrets   SecretResolver
	tokenHTTP *http.Client
	redactor  Redactor
	now       func() time.Time
	listen    func(ctx context.Context, address string) (net.Listener, error)
	factories map[string]factory
}

func NewRegistry(opts *Options) (*Registry, error) {
	if opts == nil || opts.Secrets == nil || opts.HTTPClient == nil || opts.Redactor == nil {
		return nil, Errorf(ErrInvalid, "a secret resolver, an http client and a redactor are required")
	}
	if !filepath.IsAbs(opts.Dir) {
		return nil, Errorf(ErrInvalid, "the secrets directory must be absolute")
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	listen := opts.Listen
	if listen == nil {
		listen = func(ctx context.Context, address string) (net.Listener, error) {
			var config net.ListenConfig
			return config.Listen(ctx, "tcp", address)
		}
	}
	return &Registry{
		dir:       filepath.Clean(opts.Dir),
		secrets:   opts.Secrets,
		tokenHTTP: newTokenClient(opts.HTTPClient),
		redactor:  opts.Redactor,
		now:       now,
		listen:    listen,
		factories: map[string]factory{
			KindNone:         newNone,
			KindStaticSecret: newStaticSecret,
			KindOAuth2:       newOAuth2,
		},
	}, nil
}

func (r *Registry) Strategy(spec *Spec) (Strategy, error) {
	if spec == nil {
		return nil, Errorf(ErrInvalid, "an auth spec is required")
	}
	if !config.ValidPluginID(spec.PluginID) {
		return nil, Errorf(ErrInvalid, "the plugin id is not valid")
	}
	build, known := r.factories[spec.Kind]
	if !known {
		return nil, Errorf(ErrInvalid, "the auth kind is not supported")
	}
	return build(r, spec)
}

func (r *Registry) register(values ...string) {
	for _, value := range values {
		if value != "" {
			r.redactor.RegisterSecret(value)
		}
	}
}
