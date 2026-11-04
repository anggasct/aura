package mcp

import (
	"testing"
	"time"

	"github.com/anggasct/aura/internal/config"
	"github.com/anggasct/aura/internal/health"
)

func TestServerCheckerReportsPerServerFindings(t *testing.T) {
	now := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	statuses := []ServerStatus{
		{Name: "ok", Connected: true},
		{Name: "untrusted", Code: ErrTrustRequired},
		{Name: "needs-login", Code: ErrAuthRequired},
		{Name: "no-cap", Code: ErrCapabilityUnavailable},
		{Name: "crashed", Code: ErrServerUnavailable},
		{Name: "odd", Code: ErrProtocolUnsupported},
		{Name: "dropped", Connected: false},
	}
	checker := NewServerChecker(nil, func() []ServerStatus { return statuses })
	checker.now = func() time.Time { return now }
	findings := checker.Check(t.Context())

	want := map[string]string{
		"untrusted":   "mcp_trust_required",
		"needs-login": "mcp_auth_required",
		"no-cap":      "mcp_capability_unavailable",
		"crashed":     "mcp_server_unavailable",
		"odd":         "mcp_server_unavailable",
		"dropped":     "mcp_server_unavailable",
	}
	if len(findings) != len(want) {
		t.Fatalf("findings = %d, want %d: %+v", len(findings), len(want), findings)
	}
	for i := range findings {
		finding := &findings[i]
		if finding.Code != want[finding.Scope] {
			t.Errorf("finding[%s].Code = %q, want %q", finding.Scope, finding.Code, want[finding.Scope])
		}
		if finding.Component != "mcp" || finding.Status != health.StatusDegraded || finding.Severity != health.SeverityWarning {
			t.Errorf("finding[%s] = %+v", finding.Scope, finding)
		}
		if finding.Remediation != RemediationReviewServer {
			t.Errorf("finding[%s].Remediation = %q", finding.Scope, finding.Remediation)
		}
		if finding.ID != finding.Code+":"+finding.Scope {
			t.Errorf("finding id = %q", finding.ID)
		}
	}
}

func TestServerCheckerHealthyAndLegacyFindings(t *testing.T) {
	servers := []config.MCPServer{{Name: "old", Transport: config.MCPTransportLegacySSE}}
	checker := NewServerChecker(servers, func() []ServerStatus {
		return []ServerStatus{{Name: "old", Connected: true}}
	})
	findings := checker.Check(t.Context())
	if len(findings) != 2 {
		t.Fatalf("findings = %+v", findings)
	}
	if findings[0].Code != "ok" || findings[0].Status != health.StatusUp {
		t.Errorf("first finding = %+v", findings[0])
	}
	if findings[1].Code != "mcp_legacy_sse" {
		t.Errorf("second finding = %+v", findings[1])
	}
	if got := NewServerChecker(nil, nil).Check(t.Context()); got != nil {
		t.Errorf("checker without a source = %+v", got)
	}
}

func TestServerCheckerDetailsCarryNoErrorText(t *testing.T) {
	checker := NewServerChecker(nil, func() []ServerStatus {
		return []ServerStatus{{Name: "crashed", Code: ErrServerUnavailable}}
	})
	for _, finding := range checker.Check(t.Context()) {
		if finding.Detail != "server crashed is not running (mcp_server_unavailable)" {
			t.Errorf("detail = %q", finding.Detail)
		}
	}
}
