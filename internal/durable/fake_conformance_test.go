package durable_test

import (
	"testing"

	"github.com/anggasct/aura/internal/durable"
	"github.com/anggasct/aura/internal/durable/durabletest"
)

func TestPortConformance(t *testing.T) {
	durabletest.ExerciseRuntime(t, durable.NewFake())
}
