package app

import (
	"testing"

	"github.com/anggasct/aura/internal/config"
)

func TestNewToolExecutorRefusesANilEventBus(t *testing.T) {
	executor, err := newToolExecutor(&toolExecutorInput{result: &config.LoadResult{}, wiring: &toolWiring{}})
	if executor != nil {
		t.Fatalf("newToolExecutor returned %v without an event bus", executor)
	}
	if code, ok := CodeOf(err); !ok || code != ErrorCodeInvalidArgument {
		t.Fatalf("newToolExecutor error = %v, want code %q", err, ErrorCodeInvalidArgument)
	}
}

func TestBuildTurnRuntimeRefusesANilEventBus(t *testing.T) {
	b := &build{}
	err := b.buildTurnRuntime()
	if code, ok := CodeOf(err); !ok || code != ErrorCodeInvalidArgument {
		t.Fatalf("buildTurnRuntime error = %v, want code %q", err, ErrorCodeInvalidArgument)
	}
	if b.engine != nil || b.executor != nil {
		t.Fatal("buildTurnRuntime built part of the runtime without an event bus")
	}
}
