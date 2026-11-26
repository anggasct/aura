package connector

import (
	"context"

	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/sandbox"
)

const (
	connectorMemoryBytes  = 512 << 20
	connectorMaxProcesses = 256
	connectorMaxOpenFiles = 256
	connectorFileBytes    = 64 << 20
)

type SandboxStart func(ctx context.Context, req *sandbox.SessionRequest) (*sandbox.Session, error)

type sandboxStarter struct {
	start SandboxStart
}

func NewSandboxStarter(start SandboxStart) Starter {
	if start == nil {
		start = sandbox.Start
	}
	return &sandboxStarter{start: start}
}

func (s *sandboxStarter) StartSession(ctx context.Context, req *SessionRequest) (Session, error) {
	if req == nil {
		return nil, Errorf(ErrConfigInvalid, "session request must not be nil")
	}
	session, err := s.start(ctx, &sandbox.SessionRequest{
		RequestID:   req.RequestID,
		SessionID:   "plugin-connector-" + req.Name,
		PrincipalID: "plugin-connector-" + req.Name,
		ToolName:    "plugin-connector",
		Profile:     sandbox.ProfileConnector,
		Executable:  req.Executable,
		Arguments:   append([]string(nil), req.Arguments...),
		WorkingDir:  req.WorkingDir,
		Environment: map[string]string{},
		Limits: sandbox.Limits{
			Timeout:        req.Timeout,
			MaxOutputBytes: req.MaxOutputBytes,
			MemoryBytes:    connectorMemoryBytes,
			MaxProcesses:   connectorMaxProcesses,
			MaxOpenFiles:   connectorMaxOpenFiles,
			FileBytes:      connectorFileBytes,
		},
		SetupTimeout: req.SetupTimeout,
	})
	if err != nil {
		return nil, translateSandboxError(err)
	}
	return &sandboxSession{session: session}, nil
}

type sandboxSession struct {
	session *sandbox.Session
}

func (s *sandboxSession) Request(ctx context.Context, payload []byte) ([]byte, error) {
	if s.session == nil {
		return nil, Errorf(ErrBackendUnavailable, "the connector session is not available")
	}
	out, err := s.session.Request(ctx, payload)
	if err != nil {
		return nil, translateSandboxError(err)
	}
	return out, nil
}

func (s *sandboxSession) Close(ctx context.Context) error {
	if s.session == nil {
		return nil
	}
	return s.session.Close(ctx)
}

func translateSandboxError(err error) error {
	if fault.HasCode(err, sandbox.ErrorCodeSandboxOutputExceeded) {
		return Errorf(ErrRPCLimit, "the connector message exceeds the size bound")
	}
	return Errorf(ErrBackendUnavailable, "the connector session exchange failed")
}
