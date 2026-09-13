package cli

import (
	"context"
	"net/http"
	"strings"

	"github.com/anggasct/aura/internal/config"
	"github.com/anggasct/aura/internal/egress"
	"github.com/anggasct/aura/internal/mcp"
	"github.com/anggasct/aura/internal/sandbox"
	"github.com/anggasct/aura/internal/secret"
	"github.com/anggasct/aura/internal/telemetry"
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

type sandboxSessionAdapter struct {
	session *sandbox.Session
}

func (a *sandboxSessionAdapter) Request(ctx context.Context, payload []byte) ([]byte, error) {
	if a.session == nil {
		return nil, mcp.Errorf(mcp.ErrServerUnavailable, "contained session is not available")
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
	code, ok := sandbox.CodeOf(err)
	if !ok {
		return mcp.Errorf(mcp.ErrServerUnavailable, "contained session exchange failed")
	}
	switch code {
	case sandbox.ErrorCodeSandboxOutputExceeded:
		return mcp.Errorf(mcp.ErrMessageTooLarge, "contained session response exceeds the message bound")
	default:
		return mcp.Errorf(mcp.ErrServerUnavailable, "contained session exchange failed")
	}
}

func newMCPSessionStarter(
	start func(ctx context.Context, req *sandbox.SessionRequest) (*sandbox.Session, error),
) mcp.SessionStarter {
	return mcpSessionStarterFunc(func(ctx context.Context, req *mcp.ContainedSessionRequest) (mcp.ContainedSession, error) {
		if req == nil {
			return nil, mcp.Errorf(mcp.ErrConfigInvalid, "session request must not be nil")
		}
		environment := make(map[string]string, len(req.Environment))
		for key, value := range req.Environment {
			environment[key] = value
		}
		session, err := start(ctx, &sandbox.SessionRequest{
			RequestID:   req.RequestID,
			SessionID:   "mcp-stdio-" + req.ServerName,
			PrincipalID: "mcp-stdio-" + req.ServerName,
			ToolName:    "mcp-stdio-server",
			Executable:  req.Executable,
			Arguments:   append([]string(nil), req.Arguments...),
			WorkingDir:  req.WorkingDir,
			Environment: environment,
			Limits: sandbox.Limits{
				Timeout:        req.Timeout,
				MaxOutputBytes: req.MaxOutputBytes,
			},
		})
		if err != nil {
			return nil, translateSessionError(err)
		}
		return &sandboxSessionAdapter{session: session}, nil
	})
}

type mcpSessionStarterFunc func(ctx context.Context, req *mcp.ContainedSessionRequest) (mcp.ContainedSession, error)

func (f mcpSessionStarterFunc) StartSession(ctx context.Context, req *mcp.ContainedSessionRequest) (mcp.ContainedSession, error) {
	return f(ctx, req)
}

func mcpRecorderObserver(recorder *telemetry.MCPRecorder) mcp.Observer {
	if recorder == nil {
		return nil
	}
	return func(ctx context.Context, observation *mcp.Observation) {
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
