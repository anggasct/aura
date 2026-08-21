package harness

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func descriptorFixture() Descriptor {
	return Descriptor{
		Name:           "files.read",
		Version:        "1.0.0",
		SchemaDigest:   SchemaDigest([]byte(`{"type":"object"}`)),
		Capability:     "workspace.read",
		Trust:          TrustDerivedUntrusted,
		Risk:           RiskReadOnly,
		ScopeSummary:   "workspace files",
		MaxResultBytes: 1024,
	}
}

func TestCatalogBoundsAndProgressiveSelection(t *testing.T) {
	first := descriptorFixture()
	second := first
	second.Name = "files.symbols"
	catalog, err := NewCatalog([]Descriptor{first, second}, 1)
	if err != nil {
		t.Fatalf("NewCatalog: %v", err)
	}
	visible := catalog.Visible([]string{"workspace.read"}, 10)
	if len(visible) != 1 {
		t.Fatalf("visible descriptors = %d, want 1", len(visible))
	}
	if _, err := catalog.Select("files.read", []string{"workspace.write"}); err == nil {
		t.Fatal("Select accepted a missing capability")
	} else if code, ok := CodeOf(err); !ok || code != ErrorCodeCapabilityUnavailable {
		t.Fatalf("CodeOf(%v) = %q, %v; want capability_unavailable", err, code, ok)
	}
}

func TestCatalogRejectsInvalidDescriptor(t *testing.T) {
	descriptor := descriptorFixture()
	descriptor.SchemaDigest = "not-a-digest"
	if _, err := NewCatalog([]Descriptor{descriptor}, 1); err == nil {
		t.Fatal("NewCatalog accepted invalid descriptor")
	} else if code, ok := CodeOf(err); !ok || code != ErrorCodeCatalogInvalid {
		t.Fatalf("CodeOf(%v) = %q, %v; want catalog_invalid", err, code, ok)
	}
}

type fakeProvider struct {
	profile ProviderProfile
	result  ProviderResult
	err     error
}

func (p *fakeProvider) Profile() ProviderProfile { return p.profile }

func (p *fakeProvider) Invoke(context.Context, *ProviderRequest) (ProviderResult, error) {
	return p.result, p.err
}

func TestInvokeProviderEnforcesProfileAndResultBounds(t *testing.T) {
	descriptor := descriptorFixture()
	provider := &fakeProvider{
		profile: ProviderProfile{Name: "workspace", Capability: "workspace.read", BuildProfile: "core", MaxResultBytes: 1024},
		result:  ProviderResult{State: StateSucceeded, Output: json.RawMessage(`{"ok":true}`)},
	}
	result, err := InvokeProvider(context.Background(), provider, &ProviderRequest{
		Descriptor: descriptor,
		Arguments:  json.RawMessage(`{"path":"main.go"}`),
		Scope:      "workspace-1",
	})
	if err != nil {
		t.Fatalf("InvokeProvider: %v", err)
	}
	if result.State != StateSucceeded {
		t.Fatalf("result state = %q, want succeeded", result.State)
	}
	provider.result.Output = json.RawMessage(strings.Repeat("x", 1025))
	if _, err := InvokeProvider(context.Background(), provider, &ProviderRequest{
		Descriptor: descriptor,
		Arguments:  json.RawMessage(`{}`),
		Scope:      "workspace-1",
	}); err == nil {
		t.Fatal("InvokeProvider accepted oversized result")
	} else if code, ok := CodeOf(err); !ok || code != ErrorCodeResultTooLarge {
		t.Fatalf("CodeOf(%v) = %q, %v; want result_too_large", err, code, ok)
	}
}

type fakeProviderSession struct {
	closed bool
	err    error
	wait   bool
}

func (s *fakeProviderSession) Close(ctx context.Context) error {
	s.closed = true
	if s.wait {
		<-ctx.Done()
		return ctx.Err()
	}
	return s.err
}

func TestSessionRegistryQuiescesAndRejectsLateRegistration(t *testing.T) {
	registry := NewSessionRegistry()
	session := &fakeProviderSession{}
	if err := registry.Register("provider-1", session); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if err := registry.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if !session.closed || registry.Count() != 0 {
		t.Fatalf("session closed=%v count=%d, want closed and empty", session.closed, registry.Count())
	}
	if err := registry.Register("late", &fakeProviderSession{}); err == nil {
		t.Fatal("Register accepted a late provider session")
	}
}

func TestSessionRegistryReturnsCloseFailures(t *testing.T) {
	registry := NewSessionRegistry()
	if err := registry.Register("provider-1", &fakeProviderSession{err: errors.New("close failed")}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if err := registry.Close(context.Background()); err == nil {
		t.Fatal("Close returned nil after provider failure")
	}
	if registry.Count() != 1 {
		t.Fatalf("provider session count = %d, want failed session retained", registry.Count())
	}
}

func TestSessionRegistryReportsShutdownTimeout(t *testing.T) {
	registry := NewSessionRegistry()
	session := &fakeProviderSession{wait: true}
	if err := registry.Register("provider-1", session); err != nil {
		t.Fatalf("Register: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := registry.Close(ctx); err == nil {
		t.Fatal("Close returned nil after context cancellation")
	} else if code, ok := CodeOf(err); !ok || code != ErrorCodeShutdownTimeout {
		t.Fatalf("CodeOf(%v) = %q, %v; want shutdown_timeout", err, code, ok)
	}
	if registry.Count() != 1 {
		t.Fatalf("provider session count = %d, want timed-out session retained", registry.Count())
	}
	session.wait = false
	if err := registry.Close(context.Background()); err != nil {
		t.Fatalf("Close retry: %v", err)
	}
	if registry.Count() != 0 {
		t.Fatalf("provider session count after retry = %d, want 0", registry.Count())
	}
}
