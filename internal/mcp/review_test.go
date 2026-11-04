package mcp

import (
	"slices"
	"testing"

	"github.com/anggasct/aura/internal/config"
)

func reviewFixture() *config.MCPServer {
	return &config.MCPServer{
		Name:        "docs",
		Transport:   config.MCPTransportStdio,
		Command:     "/usr/bin/docs-server",
		Args:        []string{"--secret-flag=hunter2"},
		Environment: map[string]string{"API_KEY": "hunter2"},
	}
}

func TestReviewServerStates(t *testing.T) {
	for name, factory := range trustRegistryFactories(t) {
		t.Run(name, func(t *testing.T) {
			ctx := t.Context()
			registry := factory()
			cfg := reviewFixture()

			review, err := ReviewServer(ctx, registry, cfg)
			if err != nil || review.State != TrustStateRequired {
				t.Fatalf("unknown server = %+v, %v", review, err)
			}
			if len(review.DigestPrefix()) != reviewDigestPrefixLength {
				t.Errorf("digest prefix = %q", review.DigestPrefix())
			}

			if _, err := ApproveServer(ctx, registry, cfg); err != nil {
				t.Fatalf("ApproveServer(): %v", err)
			}
			review, err = ReviewServer(ctx, registry, cfg)
			if err != nil || review.State != TrustStateApproved {
				t.Fatalf("approved server = %+v, %v", review, err)
			}

			changed := *cfg
			changed.Args = []string{"--other"}
			review, err = ReviewServer(ctx, registry, &changed)
			if err != nil || review.State != TrustStateDigestChanged {
				t.Fatalf("edited server = %+v, %v", review, err)
			}

			if err := registry.SaveSessionTrust(ctx, "docs", "surface-2", nil, []string{"a"}); err != nil {
				t.Fatalf("SaveSessionTrust(): %v", err)
			}
			review, err = ReviewServer(ctx, registry, cfg)
			if err != nil || review.State != TrustStateDigestChanged {
				t.Fatalf("changed tool surface = %+v, %v", review, err)
			}
		})
	}
}

func TestApproveServerChanges(t *testing.T) {
	for name, factory := range trustRegistryFactories(t) {
		t.Run(name, func(t *testing.T) {
			ctx := t.Context()
			registry := factory()
			cfg := reviewFixture()

			result, err := ApproveServer(ctx, registry, cfg)
			if err != nil || result.Change != ApprovalChangeApproved || result.Review.State != TrustStateApproved {
				t.Fatalf("first approval = %+v, %v", result, err)
			}
			result, err = ApproveServer(ctx, registry, cfg)
			if err != nil || result.Change != ApprovalChangeAlreadyCurrent {
				t.Fatalf("repeat approval = %+v, %v", result, err)
			}

			if err := registry.SaveSessionTrust(ctx, "docs", "surface-2", nil, []string{"b", "a"}); err != nil {
				t.Fatalf("SaveSessionTrust(): %v", err)
			}
			result, err = ApproveServer(ctx, registry, cfg)
			if err != nil || result.Change != ApprovalChangeToolSurface || !slices.Equal(result.Tools, []string{"a", "b"}) {
				t.Fatalf("tool surface approval = %+v, %v", result, err)
			}
			if trusted, err := registry.IsTrusted(ctx, "docs", "surface-2"); err != nil || !trusted {
				t.Fatalf("tool surface not trusted after approval: %v, %v", trusted, err)
			}

			changed := *cfg
			changed.Command = "/usr/bin/other"
			result, err = ApproveServer(ctx, registry, &changed)
			if err != nil || result.Change != ApprovalChangeApproved {
				t.Fatalf("approval after edit = %+v, %v", result, err)
			}
			if trusted, err := registry.IsTrusted(ctx, "docs", "surface-2"); err != nil || trusted {
				t.Fatalf("an edited server kept its old tool pin: %v, %v", trusted, err)
			}
		})
	}
}

func TestRevokeServer(t *testing.T) {
	for name, factory := range trustRegistryFactories(t) {
		t.Run(name, func(t *testing.T) {
			ctx := t.Context()
			registry := factory()
			cfg := reviewFixture()
			if removed, err := RevokeServer(ctx, registry, cfg.Name); err != nil || removed {
				t.Fatalf("revoke of unknown server = %v, %v", removed, err)
			}
			if _, err := ApproveServer(ctx, registry, cfg); err != nil {
				t.Fatalf("ApproveServer(): %v", err)
			}
			if removed, err := RevokeServer(ctx, registry, cfg.Name); err != nil || !removed {
				t.Fatalf("revoke = %v, %v", removed, err)
			}
			review, err := ReviewServer(ctx, registry, cfg)
			if err != nil || review.State != TrustStateRequired {
				t.Fatalf("review after revoke = %+v, %v", review, err)
			}
		})
	}
	if _, err := RevokeServer(t.Context(), nil, "docs"); err == nil {
		t.Error("nil registry accepted")
	}
}

func TestReviewRejectsNilInputs(t *testing.T) {
	if _, err := ReviewServer(t.Context(), NewMemoryTrustRegistry(), nil); err == nil {
		t.Error("nil server accepted")
	}
	if _, err := ReviewServer(t.Context(), nil, reviewFixture()); err == nil {
		t.Error("nil registry accepted")
	}
}
