package mcp

import (
	"context"
	"slices"
	"time"

	"github.com/anggasct/aura/internal/config"
	"github.com/anggasct/aura/internal/health"
)

const (
	healthComponentLegacySSE = "mcp_client"
	healthCodeLegacySSE      = "mcp_legacy_sse"
)

type LegacySSEChecker struct {
	servers []config.MCPServer
	now     func() time.Time
}

func NewLegacySSEChecker(servers []config.MCPServer) *LegacySSEChecker {
	return &LegacySSEChecker{servers: servers}
}

func (c *LegacySSEChecker) Check(ctx context.Context) []health.Finding {
	if ctx == nil {
		return nil
	}
	now := time.Now().UTC()
	if c.now != nil {
		now = c.now().UTC()
	}
	var findings []health.Finding
	for i := range c.servers {
		if c.servers[i].Transport != config.MCPTransportLegacySSE {
			continue
		}
		findings = append(findings, health.Finding{
			ID:        healthCodeLegacySSE + ":" + c.servers[i].Name,
			Component: healthComponentLegacySSE,
			Scope:     c.servers[i].Name,
			Code:      healthCodeLegacySSE,
			Status:    health.StatusDegraded,
			Severity:  health.SeverityWarning,
			Detail:    "server uses the legacy SSE compatibility transport",
			CheckedAt: now,
			FirstSeen: now,
			LastSeen:  now,
		})
	}
	return findings
}

const (
	healthComponentServers  = "mcp"
	RemediationReviewServer = "review-mcp-server"
)

type ServerChecker struct {
	source  func() []ServerStatus
	servers []config.MCPServer
	now     func() time.Time
}

func NewServerChecker(servers []config.MCPServer, source func() []ServerStatus) *ServerChecker {
	return &ServerChecker{source: source, servers: slices.Clone(servers)}
}

func findingCodeFor(code ErrorCode) string {
	switch code {
	case ErrTrustRequired, ErrAuthRequired, ErrCapabilityUnavailable:
		return string(code)
	default:
		return string(ErrServerUnavailable)
	}
}

func (c *ServerChecker) Check(ctx context.Context) []health.Finding {
	if ctx == nil || c.source == nil {
		return nil
	}
	now := time.Now().UTC()
	if c.now != nil {
		now = c.now().UTC()
	}
	var findings []health.Finding
	for _, status := range c.source() {
		code := status.Code
		var detail string
		switch {
		case code != "":
			detail = "server " + status.Name + " is not running (" + string(code) + ")"
		case !status.Connected:
			code = ErrServerUnavailable
			detail = "server " + status.Name + " lost its connection"
		default:
			continue
		}
		findingCode := findingCodeFor(code)
		findings = append(findings, health.Finding{
			ID:          findingCode + ":" + status.Name,
			Component:   healthComponentServers,
			Scope:       status.Name,
			Code:        findingCode,
			Status:      health.StatusDegraded,
			Severity:    health.SeverityWarning,
			Detail:      detail,
			Remediation: RemediationReviewServer,
			CheckedAt:   now,
			FirstSeen:   now,
			LastSeen:    now,
		})
	}
	if len(findings) == 0 {
		findings = append(findings, health.Finding{
			Component: healthComponentServers,
			Code:      "ok",
			Status:    health.StatusUp,
			Detail:    "all configured servers are running",
			CheckedAt: now,
		})
	}
	legacy := &LegacySSEChecker{servers: c.servers, now: c.now}
	return append(findings, legacy.Check(ctx)...)
}
