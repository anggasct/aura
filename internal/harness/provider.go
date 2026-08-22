package harness

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/anggasct/aura/internal/capability"
)

const maxProviderArgumentBytes = 1 << 20

type ProviderProfile struct {
	Name           string
	Capability     string
	BuildProfile   string
	MaxResultBytes int
	NetworkAllowed bool
}

type ProviderRequest struct {
	Descriptor Descriptor
	Arguments  json.RawMessage
	Scope      string
}

type ProviderResult struct {
	State     State
	Output    json.RawMessage
	ErrorCode string
}

type Provider interface {
	Profile() ProviderProfile
	Invoke(context.Context, *ProviderRequest) (ProviderResult, error)
}

func InvokeProvider(ctx context.Context, provider Provider, request *ProviderRequest) (ProviderResult, error) {
	if ctx == nil || provider == nil || request == nil {
		return ProviderResult{}, invalidArgument("provider invocation requires context, provider, and request")
	}
	profile := provider.Profile()
	if strings.TrimSpace(profile.Name) == "" || strings.TrimSpace(profile.Capability) == "" || profile.MaxResultBytes <= 0 || !capability.Profile(profile.BuildProfile).Valid() {
		return ProviderResult{}, codedError(ErrorCodeProviderUnavailable, "provider profile is invalid", nil)
	}
	if request.Descriptor.Capability != profile.Capability {
		return ProviderResult{}, codedError(ErrorCodeCapabilityUnavailable, "provider profile does not satisfy descriptor capability", nil)
	}
	if request.Descriptor.MaxResultBytes > profile.MaxResultBytes {
		return ProviderResult{}, codedError(ErrorCodeCapabilityUnavailable, "provider result bound is below descriptor bound", nil)
	}
	if strings.TrimSpace(request.Scope) == "" {
		return ProviderResult{}, invalidArgument("provider request scope must not be empty")
	}
	if len(request.Arguments) == 0 || len(request.Arguments) > maxProviderArgumentBytes || !json.Valid(request.Arguments) {
		return ProviderResult{}, invalidArgument("provider arguments must be valid JSON")
	}
	if err := ctx.Err(); err != nil {
		return ProviderResult{}, err
	}
	result, err := provider.Invoke(ctx, request)
	if err != nil {
		return ProviderResult{}, codedError(ErrorCodeProviderFailed, "provider invocation failed", err)
	}
	if !validState(result.State) {
		return ProviderResult{}, codedError(ErrorCodeProviderFailed, "provider returned an invalid state", nil)
	}
	if len(result.Output) > profile.MaxResultBytes {
		return ProviderResult{}, codedError(ErrorCodeResultTooLarge, "provider result exceeds its bound", nil)
	}
	if len(result.Output) > 0 && !json.Valid(result.Output) {
		return ProviderResult{}, codedError(ErrorCodeProviderFailed, "provider returned invalid JSON", nil)
	}
	return result, nil
}
