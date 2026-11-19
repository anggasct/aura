package app

import (
	"testing"

	appconfig "github.com/anggasct/aura/internal/app/config"
	"github.com/anggasct/aura/internal/platform/fault"
)

func TestNewToolExecutorRefusesANilEventBus(t *testing.T) {
	executor, err := newToolExecutor(&toolExecutorInput{result: &appconfig.LoadResult{}, wiring: &toolWiring{}})
	if executor != nil {
		t.Fatalf("newToolExecutor returned %v without an event bus", executor)
	}
	if !fault.HasCode(err, ErrorCodeInvalidArgument) {
		t.Fatalf("newToolExecutor error = %v, want code %q", err, ErrorCodeInvalidArgument)
	}
}

func TestBuildTurnRuntimeRefusesANilEventBus(t *testing.T) {
	b := &build{}
	err := b.buildTurnRuntime()
	if !fault.HasCode(err, ErrorCodeInvalidArgument) {
		t.Fatalf("buildTurnRuntime error = %v, want code %q", err, ErrorCodeInvalidArgument)
	}
	if b.engine != nil || b.executor != nil {
		t.Fatal("buildTurnRuntime built part of the runtime without an event bus")
	}
}
