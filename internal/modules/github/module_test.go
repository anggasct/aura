package github

import (
	"testing"

	"github.com/anggasct/aura/internal/platform/durable/durabletest"
	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/secret/secrettest"
	"github.com/anggasct/aura/internal/platform/toolbroker"
)

func TestModuleConstructionModes(t *testing.T) {
	if _, err := New(nil); !fault.HasCode(err, ErrorCodeInvalidArgument) {
		t.Fatalf("New(nil) = %v, want %s", err, ErrorCodeInvalidArgument)
	}
	empty, err := New(&ModuleDeps{Config: &Config{}})
	if err != nil {
		t.Fatalf("New(empty): %v", err)
	}
	if len(empty.Tools()) != 0 || empty.Adapter() != nil {
		t.Fatalf("empty module tools=%d adapter=%v", len(empty.Tools()), empty.Adapter())
	}

	toolsOnly, err := New(&ModuleDeps{
		Config: &Config{Credentials: []Credential{{Name: "ci", Ref: secrettest.Ref("env://GITHUB_TOKEN")}}},
	})
	if err != nil {
		t.Fatalf("New(toolsOnly): %v", err)
	}
	if len(toolsOnly.Tools()) != 3 || toolsOnly.Adapter() != nil {
		t.Fatalf("toolsOnly tools=%d adapter=%v", len(toolsOnly.Tools()), toolsOnly.Adapter())
	}

	if _, err := New(&ModuleDeps{Correlations: &fakeCorrelations{}}); !fault.HasCode(err, ErrorCodeInvalidArgument) {
		t.Fatalf("New(correlations without runtime) = %v, want %s", err, ErrorCodeInvalidArgument)
	}
	if _, err := New(&ModuleDeps{Runtime: durabletest.NewFake()}); !fault.HasCode(err, ErrorCodeInvalidArgument) {
		t.Fatalf("New(runtime without correlations) = %v, want %s", err, ErrorCodeInvalidArgument)
	}

	full, err := New(&ModuleDeps{
		Config:       &Config{Credentials: []Credential{{Name: "ci", Ref: secrettest.Ref("env://GITHUB_TOKEN")}}},
		Correlations: &fakeCorrelations{},
		Runtime:      durabletest.NewFake(),
	})
	if err != nil {
		t.Fatalf("New(full): %v", err)
	}
	if len(full.Tools()) != 3 || full.Adapter() == nil {
		t.Fatalf("full tools=%d adapter=%v", len(full.Tools()), full.Adapter())
	}

	if _, err := New(&ModuleDeps{
		Config: &Config{Credentials: []Credential{
			{Name: "ci", Ref: secrettest.Ref("env://A")},
			{Name: "ci", Ref: secrettest.Ref("env://B")},
		}},
	}); !fault.HasCode(err, toolbroker.ResultInvalidArgument) {
		t.Fatalf("duplicate credentials err = %v, want %s", err, toolbroker.ResultInvalidArgument)
	}
}
