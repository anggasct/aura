package harness

import (
	"context"
	"errors"
	"slices"
	"sync"
)

type ShutdownResource interface {
	Close(context.Context) error
}

type ShutdownReport struct {
	Closed   []string
	Failures []string
	TimedOut bool
	Clean    bool
}

type ShutdownCoordinator struct {
	mu        sync.Mutex
	resources map[string]ShutdownResource
	closing   bool
}

func NewShutdownCoordinator() *ShutdownCoordinator {
	return &ShutdownCoordinator{resources: make(map[string]ShutdownResource)}
}

func (c *ShutdownCoordinator) Register(name string, resource ShutdownResource) error {
	if c == nil || name == "" || resource == nil {
		return invalidArgument("shutdown resource registration is invalid")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closing {
		return codedError(ErrorCodeShutdownTimeout, "shutdown has already started", nil)
	}
	if _, exists := c.resources[name]; exists {
		return invalidArgument("shutdown resource is already registered")
	}
	c.resources[name] = resource
	return nil
}

func (c *ShutdownCoordinator) Shutdown(ctx context.Context) (ShutdownReport, error) {
	if c == nil || ctx == nil {
		return ShutdownReport{}, invalidArgument("shutdown coordinator and context must not be nil")
	}
	c.mu.Lock()
	if c.closing {
		c.mu.Unlock()
		return ShutdownReport{Clean: true}, nil
	}
	c.closing = true
	names := make([]string, 0, len(c.resources))
	for name := range c.resources {
		names = append(names, name)
	}
	slices.Sort(names)
	resources := make(map[string]ShutdownResource, len(c.resources))
	for name, resource := range c.resources {
		resources[name] = resource
	}
	c.mu.Unlock()

	report := ShutdownReport{}
	var failures []error
	for _, name := range names {
		if err := ctx.Err(); err != nil {
			report.TimedOut = true
			failures = append(failures, codedError(ErrorCodeShutdownTimeout, "shutdown deadline elapsed", err))
			break
		}
		if err := resources[name].Close(ctx); err != nil {
			report.Failures = append(report.Failures, name)
			failures = append(failures, codedError(ErrorCodeShutdownTimeout, "shutdown resource failed: "+name, err))
			if ctx.Err() != nil {
				report.TimedOut = true
				break
			}
			continue
		}
		report.Closed = append(report.Closed, name)
	}
	report.Clean = len(failures) == 0 && len(report.Closed) == len(names)
	if len(failures) == 0 {
		return report, nil
	}
	return report, errors.Join(failures...)
}
