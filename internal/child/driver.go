package child

import (
	stdcontext "context"
	"time"

	"github.com/anggasct/aura/internal/durable"
)

const HandlerName = "child.run"

type StartRequest struct {
	Handler    string
	DurableKey string
	Payload    []byte
}

type Execution struct {
	Handler   string
	Key       string
	Payload   []byte
	StartedAt time.Time
}

type Launcher interface {
	Start(ctx stdcontext.Context, req StartRequest) (key string, err error)
}

type durableLauncher struct {
	runtime durable.Runtime
}

func AdaptRuntime(runtime durable.Runtime) (Launcher, error) {
	if runtime == nil {
		return nil, Errorf(ErrorCodeChildUnavailable, "child durable runtime must not be nil")
	}
	return &durableLauncher{runtime: runtime}, nil
}

func (l *durableLauncher) Start(ctx stdcontext.Context, req StartRequest) (string, error) {
	if ctx == nil {
		return "", Errorf(ErrorCodeInvalidArgument, "context must not be nil")
	}
	if req.Handler == "" || req.DurableKey == "" {
		return "", Errorf(ErrorCodeInvalidArgument, "durable handler and key must not be empty")
	}
	ref, err := l.runtime.Start(ctx, durable.StartRequest{Handler: req.Handler, Key: req.DurableKey, Payload: req.Payload})
	if err != nil {
		return "", err
	}
	if ref.Key == "" {
		return "", Errorf(ErrorCodeChildInvalid, "child durable runtime returned an empty key")
	}
	return ref.Key, nil
}

type Driver struct {
	service *Service
	starter *Starter
}

func NewDriver(service *Service, launcher Launcher, ledger BudgetLedger, clocks Clock) (*Driver, error) {
	if service == nil {
		return nil, errNilArgument("service")
	}
	starter, err := NewStarter(service, launcher, ledger, clocks)
	if err != nil {
		return nil, err
	}
	return &Driver{service: service, starter: starter}, nil
}

func (d *Driver) RunChild(ctx stdcontext.Context, spec *Spec, now time.Time) (Spawn, bool, error) {
	if ctx == nil {
		return Spawn{}, false, Errorf(ErrorCodeInvalidArgument, "context must not be nil")
	}
	return d.starter.StartChild(ctx, spec, now)
}
