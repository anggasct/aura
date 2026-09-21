package child

import (
	stdcontext "context"
	"time"
)

type StartRequest struct {
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
