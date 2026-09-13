package cli

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/anggasct/aura/internal/egress"
	"github.com/anggasct/aura/internal/mcp"
	"github.com/anggasct/aura/internal/telemetry"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

type stubEgressResolver struct {
	ips []net.IP
	err error
}

var _ egress.Resolver = stubEgressResolver{}

func (s stubEgressResolver) LookupIP(_ context.Context, _ string) ([]net.IP, error) {
	if s.err != nil {
		return nil, s.err
	}
	return s.ips, nil
}

func TestMCPEndpointPolicyLoopback(t *testing.T) {
	policy := mcpEndpointPolicy{}
	for _, raw := range []string{
		"http://127.0.0.1:8080/mcp",
		"http://localhost:8080/mcp",
		"https://127.0.0.1/mcp",
	} {
		if err := policy.ValidateEndpoint(t.Context(), raw); err != nil {
			t.Errorf("loopback %q rejected: %v", raw, err)
		}
	}
	for _, raw := range []string{
		"http://127.0.0.1:8080/mcp?token=x",
		"https://user@127.0.0.1/mcp",
		"http://example.com/mcp",
		"",
	} {
		if err := policy.ValidateEndpoint(t.Context(), raw); err == nil {
			t.Errorf("endpoint %q accepted", raw)
		}
	}
	var nilCtx context.Context
	if err := policy.ValidateEndpoint(nilCtx, "http://127.0.0.1/mcp"); err == nil {
		t.Error("nil context accepted")
	}
}

func TestMCPEndpointPolicyRemote(t *testing.T) {
	public := stubEgressResolver{ips: []net.IP{net.ParseIP("93.184.216.34")}}
	policy := mcpEndpointPolicy{resolver: public}
	if err := policy.ValidateEndpoint(t.Context(), "https://example.com/mcp"); err != nil {
		t.Errorf("public https rejected: %v", err)
	}
	if err := policy.ValidateEndpoint(t.Context(), "http://example.com/mcp"); err == nil {
		t.Error("plain http accepted for remote host")
	}
	private := stubEgressResolver{ips: []net.IP{net.ParseIP("10.0.0.5")}}
	if err := (mcpEndpointPolicy{resolver: private}).ValidateEndpoint(t.Context(), "https://internal.example/mcp"); err == nil {
		t.Error("private address accepted")
	}
}

func TestMCPSecretResolver(t *testing.T) {
	t.Setenv("MCP_CLI_TEST_TOKEN", "token-value")
	resolver := mcpSecretResolver{}
	token, err := resolver.ResolveSecret(t.Context(), "env://MCP_CLI_TEST_TOKEN")
	if err != nil || token != "token-value" {
		t.Errorf("env resolve = %q, %v", token, err)
	}
	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte("file-value"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	token, err = resolver.ResolveSecret(t.Context(), "file://"+path)
	if err != nil || token != "file-value" {
		t.Errorf("file resolve = %q, %v", token, err)
	}
	for _, ref := range []string{"secret://key", "env://MCP_CLI_MISSING_TOKEN"} {
		if _, err := resolver.ResolveSecret(t.Context(), ref); err == nil {
			t.Errorf("ref %q accepted", ref)
		}
	}
}

func TestMCPHTTPClientValidates(t *testing.T) {
	client := mcpHTTPClient(stubEgressResolver{ips: []net.IP{net.ParseIP("93.184.216.34")}})
	if client == nil {
		t.Fatal("client missing")
	}
	if client.CheckRedirect == nil {
		t.Error("redirect validation missing")
	}
}

func TestMCPRecorderObserverBridge(t *testing.T) {
	if observer := mcpRecorderObserver(nil); observer != nil {
		t.Error("nil recorder produced an observer")
	}
	reader := sdkmetric.NewManualReader()
	recorder, err := telemetry.NewMCPRecorder(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)))
	if err != nil {
		t.Fatalf("NewMCPRecorder(): %v", err)
	}
	observer := mcpRecorderObserver(recorder)
	if observer == nil {
		t.Fatal("observer missing")
	}
	observer(t.Context(), &mcp.Observation{
		Server: "docs", Transport: "stdio", Tool: "search",
		Result: "tool_call", ResultCode: "ok", SizeBytes: 64,
	})
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("Collect(): %v", err)
	}
	names := map[string]bool{}
	for _, scope := range rm.ScopeMetrics {
		for _, point := range scope.Metrics {
			names[point.Name] = true
		}
	}
	if !names[telemetry.MetricMCPCallsTotal] {
		t.Error("bridged observation produced no metric")
	}
}
