package durabletest_test

import (
	"testing"

	"github.com/anggasct/aura/internal/platform/durable/durabletest"
)

func TestPortConformance(t *testing.T) {
	durabletest.ExerciseRuntime(t, durabletest.NewFake())
}

func TestPortReuse(t *testing.T) {
	durabletest.ExerciseReuse(t, durabletest.NewFake())
}
