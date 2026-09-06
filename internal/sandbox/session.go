package sandbox

import (
	"context"
	"strings"
)

// SessionRequest is the start contract for a long-lived contained child. The
// confinement fields match the one-shot SandboxRequest contract: the same
// allowlisted working directory, path roots, environment, limits, and
// approval grant binding. It carries no whole-session deadline and no output
// spool: per-exchange deadlines live on each Request call, and child stdout
// and stderr are persistent bounded pipes rather than collected output.
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

// spec builds the containment Spec the session child runs under. The limits
// carry the session's whole-lifetime resource bounds; the per-exchange
// deadline is applied by Request.
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

// sessionAPI is the platform-specific session engine behind the exported
// Session surface. The Linux implementation drives the confined child;
// elsewhere Start fails closed before any engine exists.
type sessionAPI interface {
	request(ctx context.Context, payload []byte) ([]byte, error)
	close(ctx context.Context) error
}

// Session is a long-lived confined child process with persistent stdio
// pipes. A zero value is not usable; construct one with Start.
//
// A session never runs unconfined: Start applies the same mandatory
// isolation stack as the one-shot Run primitive and fails closed with
// capability_unavailable when the host cannot provide it.
type Session struct {
	sessionAPI
}

// Start launches command with args inside a newly contained child process
// and returns the session handle. The child is a re-execution of this binary
// under the same confinement stack as Run: fresh user and network
// namespaces, Landlock filesystem confinement, seccomp syscall allowlist,
// rlimits, and a cgroup bounding memory and process count for the session's
// whole lifetime. Every Request exchange is newline-delimited: a payload of
// zero bytes sends an empty line, and the response is the child's next
// newline-terminated line. Message encoding and richer framing stay the
// caller's concern.
//
// Start fails closed: when a mandatory kernel primitive is missing or any
// isolation setup fails, the error carries capability_unavailable or
// sandbox_init_failed and no process is left behind.
func Start(ctx context.Context, req *SessionRequest) (*Session, error) {
	if err := req.validate(); err != nil {
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

// Close terminates the session: the whole child process group receives
// SIGTERM, then SIGKILL after the grace period, the child is reaped, and the
// cgroup is released. Close is idempotent; later calls return nil.
func (s *Session) Close(ctx context.Context) error {
	return s.close(ctx)
}

// Request performs one exchange with the session child: it writes payload
// followed by a newline to the child's stdin, then reads the next
// newline-terminated response line, bounded by Limits.MaxOutputBytes when
// set. Concurrent Request calls are serialized — one exchange at a time.
//
// A per-request deadline bounds the exchange: the sooner of ctx's deadline
// (if any) and Limits.Timeout, freshly applied per call. A deadline miss
// returns sandbox_timeout; a response beyond the output bound returns
// sandbox_output_exceeded. Both discard the exchange and leave the session
// usable for a subsequent valid request. A child that has exited, or a
// response stream that can no longer be aligned to the exchange boundary,
// returns session_broken; every later Request fails closed and Close still
// reaps.
func (s *Session) Request(ctx context.Context, payload []byte) ([]byte, error) {
	return s.request(ctx, payload)
}
