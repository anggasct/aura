package telemetry

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestMCPRecorderEmitsCallMetrics(t *testing.T) {
	reader := newTestManualReader()
	recorder, err := NewMCPRecorder(newTestMeterProvider(reader))
	if err != nil {
		t.Fatalf("NewMCPRecorder(): %v", err)
	}
	ctx := context.Background()
	recorder.Record(ctx, &MCPObservation{Server: "docs", Transport: "streamable_http", Tool: "search", Outcome: "tool_call", Code: "ok", SizeBytes: 512, Duration: 20 * time.Millisecond})
	recorder.Record(ctx, &MCPObservation{Server: "docs", Transport: "stdio", Outcome: "failed", Code: "mcp_auth_required", Duration: time.Millisecond})
	recorder.Record(ctx, nil)
	names := map[string]bool{}
	for _, scope := range collectMetrics(t, reader).ScopeMetrics {
		for _, point := range scope.Metrics {
			names[point.Name] = true
		}
	}
	for _, want := range []string{MetricMCPCallsTotal, MetricMCPCallDuration, MetricMCPResponseSize} {
		if !names[want] {
			t.Errorf("metric %q missing", want)
		}
	}
}

func TestMCPRecorderIgnoresNil(t *testing.T) {
	reader := newTestManualReader()
	recorder, err := NewMCPRecorder(newTestMeterProvider(reader))
	if err != nil {
		t.Fatalf("NewMCPRecorder(): %v", err)
	}
	var nilRecorder *MCPRecorder
	nilRecorder.Record(context.Background(), &MCPObservation{Outcome: "ok"})
	recorder.Record(context.Background(), nil)
}

func TestMCPMetricLabelsExcludeHighCardinality(t *testing.T) {
	for metric, labels := range map[string][]string{
		MetricMCPCallsTotal:   AllowedMetricLabels(MetricMCPCallsTotal),
		MetricMCPCallDuration: AllowedMetricLabels(MetricMCPCallDuration),
		MetricMCPResponseSize: AllowedMetricLabels(MetricMCPResponseSize),
	} {
		for _, label := range labels {
			if label == AttrSessionID || label == AttrTurnID {
				t.Errorf("metric %q carries high-cardinality label %q", metric, label)
			}
			if isContentLeak(label) {
				t.Errorf("metric %q label %q trips the content blocklist", metric, label)
			}
		}
	}
}

func TestMCPObservationCarriesNoContent(t *testing.T) {
	blocked := []string{
		"content", "body", "payload", "token", "secret", "credential",
		"arguments", "query", "prompt", "message", "text", "output",
		"result", "error", "detail",
	}
	observed := map[string]bool{}
	for _, field := range reflect.VisibleFields(reflect.TypeFor[MCPObservation]()) {
		name := strings.ToLower(field.Name)
		observed[name] = true
		for _, denied := range blocked {
			if strings.Contains(name, denied) {
				t.Errorf("MCPObservation field %q can carry content", field.Name)
			}
		}
	}
	for _, required := range []string{"server", "transport", "tool", "outcome", "code", "sizebytes", "duration"} {
		if !observed[required] {
			t.Errorf("MCPObservation lost required field %q", required)
		}
	}
}
