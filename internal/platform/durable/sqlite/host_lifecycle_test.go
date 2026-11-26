package sqlite

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/platform/fault"
)

type syncLogBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncLogBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncLogBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func TestDurableHostStopGraceFitsTheShutdownTimeout(t *testing.T) {
	cases := []struct {
		shutdown, grace time.Duration
	}{
		{30 * time.Second, 10 * time.Second},
		{10 * time.Second, 5 * time.Second},
		{time.Second, 500 * time.Millisecond},
		{time.Millisecond, time.Millisecond},
	}
	for _, tc := range cases {
		t.Run(tc.shutdown.String(), func(t *testing.T) {
			options := HostOptions(nil, nil, tc.shutdown)
			if options.StopGrace != tc.grace {
				t.Fatalf("stop grace for a shutdown timeout of %s = %s, want %s", tc.shutdown, options.StopGrace, tc.grace)
			}
		})
	}
}

type scriptedRunHost struct {
	early      chan struct{}
	held       chan struct{}
	err        error
	acquireErr error
	ran        atomic.Bool
}

func newScriptedRunHost(err error) *scriptedRunHost {
	return &scriptedRunHost{early: make(chan struct{}), held: make(chan struct{}), err: err}
}

func (h *scriptedRunHost) Acquire(context.Context) error {
	return h.acquireErr
}

func (h *scriptedRunHost) Run(ctx context.Context) error {
	h.ran.Store(true)
	select {
	case <-h.early:
		return h.err
	case <-ctx.Done():
	}
	<-h.held
	return h.err
}

func startScriptedHostStage(t *testing.T, host *scriptedRunHost) (*HostStage, *syncLogBuffer) {
	t.Helper()
	logs := &syncLogBuffer{}
	stage := NewHostStage(host, nil, slog.New(slog.NewJSONHandler(logs, nil)))
	if err := stage.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	return stage, logs
}

func TestDurableHostStageReportsHowItsRunEnded(t *testing.T) {
	hostFailed := errors.New("a handler did not return within the grace")
	t.Run("a stop returns the error of the host it stopped", func(t *testing.T) {
		host := newScriptedRunHost(hostFailed)
		close(host.held)
		stage, _ := startScriptedHostStage(t, host)
		if err := stage.Stop(t.Context()); !errors.Is(err, hostFailed) {
			t.Fatalf("Stop = %v, want the host's error", err)
		}
	})
	t.Run("a stop gives up when its own context ends", func(t *testing.T) {
		host := newScriptedRunHost(nil)
		stage, _ := startScriptedHostStage(t, host)
		t.Cleanup(func() {
			close(host.held)
			<-stage.done
		})
		ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
		defer cancel()
		if err := stage.Stop(ctx); !fault.HasCode(err, ErrorCodeStopTimeout) {
			t.Fatalf("Stop = %v, want %s", err, ErrorCodeStopTimeout)
		}
	})
	t.Run("a host that ends before the stop: the stop returns its error", func(t *testing.T) {
		host := newScriptedRunHost(hostFailed)
		stage, _ := startScriptedHostStage(t, host)
		close(host.early)
		<-stage.done
		if err := stage.Stop(t.Context()); !errors.Is(err, hostFailed) {
			t.Fatalf("Stop = %v, want the host's error", err)
		}
	})
}

func TestDurableHostStageFailsItsStartWithoutALease(t *testing.T) {
	host := newScriptedRunHost(nil)
	host.acquireErr = errors.New("the lease was refused")
	logs := &syncLogBuffer{}
	stage := NewHostStage(host, nil, slog.New(slog.NewJSONHandler(logs, nil)))
	if err := stage.Start(t.Context()); !errors.Is(err, host.acquireErr) {
		t.Fatalf("Start = %v, want the lease error", err)
	}
	if host.ran.Load() {
		t.Fatal("the stage ran a host that holds no lease")
	}
	if logs.String() != "" {
		t.Fatalf("a start that failed logged %s", logs.String())
	}
}
