package mcp

import (
	"context"
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
