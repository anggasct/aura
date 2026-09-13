package cli

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/config"
	"github.com/anggasct/aura/internal/egress"
	"github.com/anggasct/aura/internal/mcp"
	"github.com/anggasct/aura/internal/telemetry"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
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
		Server: "docs", Transport: "stdio", ProtocolVersion: "2025-03-26", Tool: "search",
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
	for _, scope := range rm.ScopeMetrics {
		for _, point := range scope.Metrics {
			if point.Name != telemetry.MetricMCPCallsTotal {
				continue
			}
			sum, ok := point.Data.(metricdata.Sum[int64])
			if !ok {
				t.Fatalf("calls total is %T, want Sum[int64]", point.Data)
			}
			if len(sum.DataPoints) == 0 {
				t.Fatal("calls total has no data points")
			}
			for _, dp := range sum.DataPoints {
				protocol, ok := dp.Attributes.Value(telemetry.AttrMCPProtocol)
				if !ok || protocol.AsString() == "" {
					t.Errorf("calls total data point missing protocol: %v", dp.Attributes)
				}
			}
		}
	}
}

type bridgeEchoInput struct {
	Message string `json:"message"`
}

type bridgeEchoOutput struct {
	Reply string `json:"reply"`
}

func TestMCPRecorderBridgeCarriesProtocolAndSize(t *testing.T) {
	ctx := t.Context()
	server := sdk.NewServer(&sdk.Implementation{Name: "bridge-server", Version: "1.0.0"}, nil)
	sdk.AddTool(server, &sdk.Tool{Name: "echo", Description: "echoes"}, func(_ context.Context, _ *sdk.CallToolRequest, in bridgeEchoInput) (*sdk.CallToolResult, bridgeEchoOutput, error) {
		return nil, bridgeEchoOutput{Reply: in.Message}, nil
	})
	clientTransport, serverTransport := sdk.NewInMemoryTransports()
	go func() {
		_ = server.Run(ctx, serverTransport)
	}()
	reader := sdkmetric.NewManualReader()
	recorder, err := telemetry.NewMCPRecorder(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)))
	if err != nil {
		t.Fatalf("NewMCPRecorder(): %v", err)
	}
	client, err := mcp.NewClient(&config.MCPServer{
		Name:           "bridge-e2e",
		Transport:      config.MCPTransportStdio,
		RequestTimeout: config.Duration(5 * time.Second),
		StartupTimeout: config.Duration(5 * time.Second),
	}, nil, mcp.WithObserver(mcpRecorderObserver(recorder)))
	if err != nil {
		t.Fatalf("NewClient(): %v", err)
	}
	defer func() { _ = client.Close() }()
	if err := client.Connect(ctx, clientTransport); err != nil {
		t.Fatalf("Connect(): %v", err)
	}
	tools, err := client.DiscoverTools(ctx)
	if err != nil {
		t.Fatalf("DiscoverTools(): %v", err)
	}
	if len(tools) != 1 {
		t.Fatalf("tools = %d, want 1", len(tools))
	}
	args, err := json.Marshal(bridgeEchoInput{Message: "hello"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if _, err := client.CallTool(ctx, "echo", args); err != nil {
		t.Fatalf("CallTool(): %v", err)
	}
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("Collect(): %v", err)
	}
	protocolSeen := false
	for _, scope := range rm.ScopeMetrics {
		for _, point := range scope.Metrics {
			if point.Name != telemetry.MetricMCPCallsTotal {
				continue
			}
			sum, ok := point.Data.(metricdata.Sum[int64])
			if !ok {
				continue
			}
			for _, dp := range sum.DataPoints {
				if v, ok := dp.Attributes.Value(telemetry.AttrMCPProtocol); ok && v.AsString() != "" {
					if !mcp.IsSupportedProtocolVersion(v.AsString()) {
						t.Errorf("protocol %q is not supported", v.AsString())
					}
					protocolSeen = true
				}
			}
		}
	}
	if !protocolSeen {
		t.Error("no recorded operation carries protocol version")
	}
	var sizeSum float64
	var sizeCount uint64
	for _, scope := range rm.ScopeMetrics {
		for _, point := range scope.Metrics {
			if point.Name != telemetry.MetricMCPResponseSize {
				continue
			}
			hist, ok := point.Data.(metricdata.Histogram[float64])
			if !ok {
				continue
			}
			for _, dp := range hist.DataPoints {
				sizeSum += dp.Sum
				sizeCount += dp.Count
			}
		}
	}
	if sizeCount == 0 {
		t.Error("response size histogram has no points")
	}
	if sizeSum <= 0 {
		t.Errorf("response size sum = %v, want > 0 for non-empty discovery and call payloads", sizeSum)
	}
}
