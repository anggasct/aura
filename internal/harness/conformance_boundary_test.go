package harness

import (
	"context"
	"errors"
	"iter"
	"net"
	"testing"

	"github.com/anggasct/aura/internal/approval"
	"github.com/anggasct/aura/internal/runtime"
	"github.com/anggasct/aura/internal/sandbox"
	"github.com/anggasct/aura/internal/store"
)

type compositionRuntime struct{}

func (compositionRuntime) Run(context.Context, *runtime.TurnRequest) iter.Seq2[store.RuntimeEvent, error] {
	return func(func(store.RuntimeEvent, error) bool) {}
}

type compositionBroker struct{}

func (compositionBroker) Evaluate(context.Context, *approval.ToolRequest) (approval.PolicyDecision, error) {
	return approval.PolicyDecision{Outcome: approval.OutcomeDeny}, nil
}

type compositionResolver map[string][]net.IP

func (r compositionResolver) LookupIP(_ context.Context, host string) ([]net.IP, error) {
	ips, ok := r[host]
	if !ok {
		return nil, errors.New("host not found")
	}
	return ips, nil
}

func TestCompositionRequiresOneRuntimeAndBrokerBoundary(t *testing.T) {
	base := CompositionConfig{Runtime: compositionRuntime{}, Broker: compositionBroker{}}
	if _, err := NewComposition(context.Background(), &CompositionConfig{Broker: base.Broker}); err == nil {
		t.Fatal("composition accepted a nil runtime")
	}
	if _, err := NewComposition(context.Background(), &CompositionConfig{Runtime: base.Runtime}); err == nil {
		t.Fatal("composition accepted a nil broker")
	}
	composition, err := NewComposition(context.Background(), &base)
	if err != nil {
		t.Fatalf("NewComposition: %v", err)
	}
	if composition.Runtime == nil || composition.Broker == nil {
		t.Fatalf("composition lost an authoritative boundary: %+v", composition)
	}
}

func TestCompositionProfilesFailClosed(t *testing.T) {
	descriptor := descriptorFixture()
	provider := &fakeProvider{profile: ProviderProfile{
		Name:           "workspace",
		Capability:     descriptor.Capability,
		BuildProfile:   "core",
		MaxResultBytes: descriptor.MaxResultBytes,
	}}
	base := CompositionConfig{Runtime: compositionRuntime{}, Broker: compositionBroker{}}
	allPrimitives := sandbox.Primitives{UserNamespace: true, Seccomp: true, CgroupV2: true, Landlock: true, ProcessGroups: true}
	cases := []struct {
		name string
		cfg  CompositionConfig
		code ErrorCode
	}{
		{name: "sandbox", cfg: CompositionConfig{Runtime: base.Runtime, Broker: base.Broker, RequireSandbox: true}, code: ErrorCodeCapabilityUnavailable},
		{name: "provider", cfg: CompositionConfig{Runtime: base.Runtime, Broker: base.Broker, RequireProvider: true}, code: ErrorCodeProviderUnavailable},
		{name: "egress", cfg: CompositionConfig{
			Runtime: base.Runtime, Broker: base.Broker,
			NetworkDestination: "https://internal.example",
			NetworkResolver:    compositionResolver{"internal.example": {net.ParseIP("10.0.0.2")}},
		}, code: ErrorCodeCapabilityUnavailable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewComposition(context.Background(), &tc.cfg)
			if code, ok := CodeOf(err); !ok || code != tc.code {
				t.Fatalf("NewComposition = %v, code=%q ok=%v; want %q", err, code, ok, tc.code)
			}
		})
	}

	composition, err := NewComposition(context.Background(), &CompositionConfig{
		Runtime:            base.Runtime,
		Broker:             base.Broker,
		RequireSandbox:     true,
		SandboxPrimitives:  allPrimitives,
		Provider:           provider,
		Descriptor:         &descriptor,
		RequireProvider:    true,
		NetworkDestination: "https://public.example",
		NetworkResolver:    compositionResolver{"public.example": {net.ParseIP("93.184.216.34")}},
	})
	if err != nil {
		t.Fatalf("NewComposition(available profile): %v", err)
	}
	if composition == nil {
		t.Fatal("available profile returned a nil composition")
	}
}
