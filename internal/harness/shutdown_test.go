package harness

import (
	"context"
	"testing"
)

type closeProbe struct {
	done  chan struct{}
	block bool
}

func (p *closeProbe) Close(ctx context.Context) error {
	if p.block {
		<-ctx.Done()
		return ctx.Err()
	}
	close(p.done)
	return nil
}

func TestShutdownCoordinatorClosesResourcesInStableOrder(t *testing.T) {
	coordinator := NewShutdownCoordinator()
	first := &closeProbe{done: make(chan struct{})}
	second := &closeProbe{done: make(chan struct{})}
	if err := coordinator.Register("http-bodies", second); err != nil {
		t.Fatalf("Register second: %v", err)
	}
	if err := coordinator.Register("provider-sessions", first); err != nil {
		t.Fatalf("Register first: %v", err)
	}
	report, err := coordinator.Shutdown(context.Background())
	if err != nil || !report.Clean {
		t.Fatalf("Shutdown report=%+v err=%v, want clean", report, err)
	}
	if len(report.Closed) != 2 || report.Closed[0] != "http-bodies" || report.Closed[1] != "provider-sessions" {
		t.Fatalf("closed resources = %v, want stable order", report.Closed)
	}
	if err := coordinator.Register("late", &closeProbe{done: make(chan struct{})}); err == nil {
		t.Fatal("Register accepted a resource after shutdown")
	}
}

func TestShutdownCoordinatorReportsNonCleanTimeout(t *testing.T) {
	coordinator := NewShutdownCoordinator()
	if err := coordinator.Register("child-process", &closeProbe{block: true}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	report, err := coordinator.Shutdown(ctx)
	if err == nil || report.Clean || !report.TimedOut {
		t.Fatalf("Shutdown report=%+v err=%v, want non-clean timeout", report, err)
	}
	if code, ok := CodeOf(err); !ok || code != ErrorCodeShutdownTimeout {
		t.Fatalf("CodeOf(%v) = %q, %v; want shutdown_timeout", err, code, ok)
	}
}
