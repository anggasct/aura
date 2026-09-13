package cli

import (
	"context"
	"net/http"
	"strings"

	"github.com/anggasct/aura/internal/config"
	"github.com/anggasct/aura/internal/egress"
	"github.com/anggasct/aura/internal/mcp"
	"github.com/anggasct/aura/internal/secret"
)

type mcpEndpointPolicy struct {
	resolver egress.Resolver
}

func (p mcpEndpointPolicy) ValidateEndpoint(ctx context.Context, rawURL string) error {
	if ctx == nil {
		return egress.Errorf(egress.ErrorCodeEgressDenied, "context must not be nil")
	}
	if config.IsLoopbackBaseURL(rawURL) {
		if err := config.ValidateBaseURL(rawURL); err != nil {
			return egress.Errorf(egress.ErrorCodeEgressDenied, "loopback endpoint is not valid")
		}
		return nil
	}
	_, err := egress.Validate(ctx, rawURL, p.resolver)
	return err
}

type mcpSecretResolver struct{}

func (mcpSecretResolver) ResolveSecret(_ context.Context, ref string) (string, error) {
	var source secret.Reference
	switch {
	case strings.HasPrefix(ref, "env://"):
		source.Env = strings.TrimPrefix(ref, "env://")
	case strings.HasPrefix(ref, "file://"):
		source.File = strings.TrimPrefix(ref, "file://")
	default:
		return "", mcp.Errorf(mcp.ErrConfigInvalid, "credential reference must use env:// or file://")
	}
	token, err := source.Resolve()
	if err != nil || strings.TrimSpace(token) == "" {
		return "", mcp.Errorf(mcp.ErrAuthRequired, "credential is unavailable")
	}
	return token, nil
}

func mcpHTTPClient(resolver egress.Resolver) *http.Client {
	return egress.NewClient(resolver)
}
