package durable_test

import (
	"testing"

	"github.com/anggasct/aura/internal/platform/durable"
	"github.com/anggasct/aura/internal/platform/durable/durabletest"
)

func TestPortConformance(t *testing.T) {
	durabletest.ExerciseRuntime(t, durable.NewFake())
}

func TestPortReuse(t *testing.T) {
	durabletest.ExerciseReuse(t, durable.NewFake())
}
