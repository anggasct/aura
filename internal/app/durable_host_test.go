package app

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/config"
	"github.com/anggasct/aura/internal/store"
)

func openDatabaseFile(t *testing.T, dbPath string) *sql.DB {
	t.Helper()
	db, err := store.OpenDB(t.Context(), dbPath)
	if err != nil {
		t.Fatalf("open the database file: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
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
			options := durableHostOptions(nil, nil, config.Runtime{ShutdownTimeout: config.Duration(tc.shutdown)})
			if options.StopGrace != tc.grace {
				t.Fatalf("stop grace for a shutdown timeout of %s = %s, want %s", tc.shutdown, options.StopGrace, tc.grace)
			}
		})
	}
}

type scriptedRunHost struct {
	early chan struct{}
	held  chan struct{}
	err   error
}

func newScriptedRunHost(err error) *scriptedRunHost {
	return &scriptedRunHost{early: make(chan struct{}), held: make(chan struct{}), err: err}
}

func (h *scriptedRunHost) Run(ctx context.Context) error {
	select {
	case <-h.early:
		return h.err
	case <-ctx.Done():
	}
	<-h.held
	return h.err
}

func startScriptedHostStage(t *testing.T, host *scriptedRunHost) (*durableHostStage, *syncBuffer) {
	t.Helper()
	logs := &syncBuffer{}
	stage := &durableHostStage{host: host, logger: slog.New(slog.NewJSONHandler(logs, nil))}
	if err := stage.start(t.Context()); err != nil {
		t.Fatalf("start: %v", err)
	}
	return stage, logs
}

func TestDurableHostStageReportsHowItsRunEnded(t *testing.T) {
	hostFailed := errors.New("a handler did not return within the grace")
	t.Run("a stop returns the error of the host it stopped", func(t *testing.T) {
		host := newScriptedRunHost(hostFailed)
		close(host.held)
		stage, _ := startScriptedHostStage(t, host)
		if err := stage.stop(t.Context()); !errors.Is(err, hostFailed) {
			t.Fatalf("stop = %v, want the host's error", err)
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
		var coded *Error
		if err := stage.stop(ctx); !errors.As(err, &coded) || coded.Code != ErrorCodeStopTimeout {
			t.Fatalf("stop = %v, want %s", err, ErrorCodeStopTimeout)
		}
	})
	t.Run("a host that ends before the stop is logged once and the stop succeeds", func(t *testing.T) {
		host := newScriptedRunHost(hostFailed)
		stage, logs := startScriptedHostStage(t, host)
		close(host.early)
		<-stage.done
		if err := stage.stop(t.Context()); err != nil {
			t.Fatalf("stop = %v, want nil", err)
		}
		if n := strings.Count(logs.String(), `"level":"ERROR"`); n != 1 {
			t.Fatalf("error lines = %d, want 1 in %s", n, logs.String())
		}
		if !strings.Contains(logs.String(), hostFailed.Error()) {
			t.Fatalf("the error line does not carry the host's error: %s", logs.String())
		}
	})
}
