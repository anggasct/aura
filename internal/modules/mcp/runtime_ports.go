package mcp

import (
	"context"
	"maps"
	"net/http"
	"slices"
	"strings"

	"github.com/anggasct/aura/internal/platform/egress"
	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/sandbox"
	"github.com/anggasct/aura/internal/platform/secret"
	"github.com/anggasct/aura/internal/platform/telemetry"
)

type egressEndpointPolicy struct {
	resolver egress.Resolver
}

func NewEndpointPolicy(resolver egress.Resolver) EndpointPolicy {
	return egressEndpointPolicy{resolver: resolver}
}

func (p egressEndpointPolicy) ValidateEndpoint(ctx context.Context, rawURL string) error {
	if ctx == nil {
		return egress.Errorf(egress.ErrorCodeEgressDenied, "context must not be nil")
	}
	if egress.IsLoopbackBaseURL(rawURL) {
		if err := egress.ValidateBaseURL(rawURL); err != nil {
			return egress.Errorf(egress.ErrorCodeEgressDenied, "loopback endpoint is not valid")
		}
		return nil
	}
	_, err := egress.Validate(ctx, rawURL, p.resolver)
	return err
}

type defaultSecretResolver struct{}

func NewSecretResolver() SecretResolver {
	return defaultSecretResolver{}
}

func (defaultSecretResolver) ResolveSecret(_ context.Context, ref string) (string, error) {
	parsed, err := secret.ParseRef(ref)
	if err != nil || parsed.IsZero() {
		return "", Errorf(ErrConfigInvalid, "credential reference must use env:// or file://")
	}
	token, err := parsed.Reference().Resolve()
	if err != nil || strings.TrimSpace(token) == "" {
		return "", Errorf(ErrAuthRequired, "credential is unavailable")
	}
	return token, nil
}

func NewHTTPClient(resolver egress.Resolver) *http.Client {
	return egress.NewClient(resolver)
}

type SandboxSessionStart func(ctx context.Context, req *sandbox.SessionRequest) (*sandbox.Session, error)

type sandboxSessionAdapter struct {
	session *sandbox.Session
}

func (a *sandboxSessionAdapter) Request(ctx context.Context, payload []byte) ([]byte, error) {
	if a.session == nil {
		return nil, Errorf(ErrServerUnavailable, "contained session is not available")
	}
	out, err := a.session.Request(ctx, payload)
	if err != nil {
		return nil, translateSessionError(err)
	}
	return out, nil
}

func (a *sandboxSessionAdapter) Close(ctx context.Context) error {
	if a.session == nil {
		return nil
	}
	return a.session.Close(ctx)
}

func translateSessionError(err error) error {
	if fault.HasCode(err, sandbox.ErrorCodeSandboxOutputExceeded) {
		return Errorf(ErrMessageTooLarge, "contained session response exceeds the message bound")
	}
	return Errorf(ErrServerUnavailable, "contained session exchange failed")
}

type sessionStarterFunc func(ctx context.Context, req *ContainedSessionRequest) (ContainedSession, error)

func (f sessionStarterFunc) StartSession(ctx context.Context, req *ContainedSessionRequest) (ContainedSession, error) {
	return f(ctx, req)
}

func NewSandboxSessionStarter(start SandboxSessionStart) SessionStarter {
	if start == nil {
		start = sandbox.Start
	}
	return sessionStarterFunc(func(ctx context.Context, req *ContainedSessionRequest) (ContainedSession, error) {
		if req == nil {
			return nil, Errorf(ErrConfigInvalid, "session request must not be nil")
		}
		environment := make(map[string]string, len(req.Environment))
		maps.Copy(environment, req.Environment)
		session, err := start(ctx, &sandbox.SessionRequest{
			RequestID:   req.RequestID,
			SessionID:   "mcp-stdio-" + req.ServerName,
			PrincipalID: "mcp-stdio-" + req.ServerName,
			ToolName:    "mcp-stdio-server",
			Executable:  req.Executable,
			Arguments:   slices.Clone(req.Arguments),
			WorkingDir:  req.WorkingDir,
			Environment: environment,
			Limits: sandbox.Limits{
				Timeout:        req.Timeout,
				MaxOutputBytes: req.MaxOutputBytes,
			},
			SetupTimeout: req.SetupTimeout,
		})
		if err != nil {
			return nil, translateSessionError(err)
		}
		return &sandboxSessionAdapter{session: session}, nil
	})
}

func NewTelemetryObserver(recorder *telemetry.MCPRecorder) Observer {
	if recorder == nil {
		return nil
	}
	return func(ctx context.Context, observation *Observation) {
		if observation == nil {
			return
		}
		recorder.Record(ctx, &telemetry.MCPObservation{
			Server:    observation.Server,
			Transport: observation.Transport,
			Protocol:  observation.ProtocolVersion,
			Tool:      observation.Tool,
			Count:     observation.Count,
			Outcome:   observation.Result,
			Code:      observation.ResultCode,
			SizeBytes: observation.SizeBytes,
			Duration:  observation.Duration,
		})
	}
}
