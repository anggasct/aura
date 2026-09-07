package sandbox

import (
	"context"
	"strings"
)

type SessionRequest struct {
	RequestID       string
	PrincipalID     string
	SessionID       string
	ToolName        string
	Executable      string
	Arguments       []string
	WorkingDir      string
	ReadOnlyPaths   []string
	ReadWritePaths  []string
	Environment     map[string]string
	Capabilities    []string
	Limits          Limits
	ApprovalGrantID string
}

type grantValidator interface {
	validateSessionGrant(req *SessionRequest) error
}

var defaultGrantValidator grantValidator

func validateSessionGrant(v grantValidator, req *SessionRequest) error {
	if v == nil {
		if req.ApprovalGrantID != "" {
			return Errorf(ErrorCodeApprovalInvalid, "request carries an approval grant but no validator is configured to enforce it")
		}
		return nil
	}
	return v.validateSessionGrant(req)
}

func (r *SessionRequest) validate() error {
	if r == nil {
		return Errorf(ErrorCodeInvalidArgument, "request must not be nil")
	}
	if strings.TrimSpace(r.WorkingDir) == "" {
		return Errorf(ErrorCodeInvalidArgument, "working directory must not be empty")
	}
	if strings.TrimSpace(r.Executable) == "" {
		return Errorf(ErrorCodeInvalidArgument, "executable must not be empty")
	}
	if r.Limits.Timeout <= 0 {
		return Errorf(ErrorCodeInvalidArgument, "timeout must be positive")
	}
	return nil
}

func (r *SessionRequest) spec() *Spec {
	environment := make([]string, 0, len(r.Environment))
	for key, value := range r.Environment {
		environment = append(environment, key+"="+value)
	}
	return &Spec{
		WorkingDir:     r.WorkingDir,
		ReadOnlyPaths:  sortedClone(r.ReadOnlyPaths),
		ReadWritePaths: sortedClone(r.ReadWritePaths),
		AllowEnv:       environment,
		Limits:         r.Limits,
	}
}

type sessionAPI interface {
	request(ctx context.Context, payload []byte) ([]byte, error)
	close(ctx context.Context) error
}

type Session struct {
	sessionAPI
}

func Start(ctx context.Context, req *SessionRequest) (*Session, error) {
	if err := req.validate(); err != nil {
		return nil, err
	}
	if err := validateSessionGrant(defaultGrantValidator, req); err != nil {
		return nil, err
	}
	primitives, err := Negotiate()
	if err != nil {
		return nil, err
	}
	if err := Require(primitives); err != nil {
		return nil, err
	}
	return startSession(ctx, req, req.spec())
}

func (s *Session) Close(ctx context.Context) error {
	return s.close(ctx)
}

func (s *Session) Request(ctx context.Context, payload []byte) ([]byte, error) {
	return s.request(ctx, payload)
}
