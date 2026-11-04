package mcp

import (
	"context"
	"errors"
	"slices"

	"github.com/anggasct/aura/internal/config"
)

type TrustState string

const (
	TrustStateApproved       TrustState = "approved"
	TrustStateRequired       TrustState = "trust_required"
	TrustStateDigestChanged  TrustState = "digest_changed"
	reviewDigestPrefixLength            = 12
)

type ServerReview struct {
	Name      string
	Transport string
	State     TrustState
	Digest    string
}

func (r *ServerReview) DigestPrefix() string {
	if len(r.Digest) <= reviewDigestPrefixLength {
		return r.Digest
	}
	return r.Digest[:reviewDigestPrefixLength]
}

type ApprovalChange string

const (
	ApprovalChangeApproved       ApprovalChange = "approved"
	ApprovalChangeToolSurface    ApprovalChange = "tool_surface_approved"
	ApprovalChangeAlreadyCurrent ApprovalChange = "already_approved"
)

type ApprovalResult struct {
	Change ApprovalChange
	Review ServerReview
	Tools  []string
}

func ReviewServer(ctx context.Context, registry TrustRegistry, serverCfg *config.MCPServer) (ServerReview, error) {
	if registry == nil || serverCfg == nil {
		return ServerReview{}, Errorf(ErrConfigInvalid, "trust registry and server configuration are required")
	}
	digest, err := ComputeConfigDigest(serverCfg)
	if err != nil {
		return ServerReview{}, err
	}
	review := ServerReview{Name: serverCfg.Name, Transport: serverCfg.Transport, State: TrustStateRequired, Digest: digest}
	record, err := registry.GetTrust(ctx, serverCfg.Name)
	if errors.Is(err, ErrTrustNotFound) {
		return review, nil
	}
	if err != nil {
		return ServerReview{}, err
	}
	switch {
	case record.ConfigDecision != TrustDecisionApproved:
		review.State = TrustStateRequired
	case record.ConfigDigest != digest:
		review.State = TrustStateDigestChanged
	case record.Decision == TrustDecisionPending && record.Digest != "":
		review.State = TrustStateDigestChanged
	default:
		review.State = TrustStateApproved
	}
	return review, nil
}

func ReviewServers(ctx context.Context, registry TrustRegistry, servers []config.MCPServer) ([]ServerReview, error) {
	reviews := make([]ServerReview, 0, len(servers))
	for i := range servers {
		review, err := ReviewServer(ctx, registry, &servers[i])
		if err != nil {
			return nil, err
		}
		reviews = append(reviews, review)
	}
	return reviews, nil
}

func ApproveServer(ctx context.Context, registry TrustRegistry, serverCfg *config.MCPServer) (ApprovalResult, error) {
	review, err := ReviewServer(ctx, registry, serverCfg)
	if err != nil {
		return ApprovalResult{}, err
	}
	switch review.State {
	case TrustStateApproved:
		return ApprovalResult{Change: ApprovalChangeAlreadyCurrent, Review: review}, nil
	case TrustStateRequired:
		return approveConfig(ctx, registry, serverCfg, review)
	case TrustStateDigestChanged:
	}
	record, err := registry.GetTrust(ctx, serverCfg.Name)
	if err != nil {
		return ApprovalResult{}, err
	}
	if record.ConfigDigest != review.Digest {
		return approveConfig(ctx, registry, serverCfg, review)
	}
	if err := registry.Approve(ctx, serverCfg.Name, record.Digest); err != nil {
		return ApprovalResult{}, err
	}
	review.State = TrustStateApproved
	return ApprovalResult{Change: ApprovalChangeToolSurface, Review: review, Tools: slices.Sorted(slices.Values(record.Tools))}, nil
}

func approveConfig(ctx context.Context, registry TrustRegistry, serverCfg *config.MCPServer, review ServerReview) (ApprovalResult, error) {
	if err := registry.ApproveConfig(ctx, serverCfg.Name, review.Digest); err != nil {
		return ApprovalResult{}, err
	}
	review.State = TrustStateApproved
	return ApprovalResult{Change: ApprovalChangeApproved, Review: review}, nil
}

func RevokeServer(ctx context.Context, registry TrustRegistry, serverName string) (bool, error) {
	if registry == nil {
		return false, Errorf(ErrConfigInvalid, "trust registry is required")
	}
	return registry.Revoke(ctx, serverName)
}
