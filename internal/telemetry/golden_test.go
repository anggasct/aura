package telemetry

import (
	"testing"
)

func TestSemconvVersionPinned(t *testing.T) {
	if SemconvVersion != "1.30.0" {
		t.Errorf("SemconvVersion = %q, want pinned 1.30.0; upgrading requires golden attribute/cardinality review", SemconvVersion)
	}
}

func TestSpanNamesPinned(t *testing.T) {
	spans := map[string]string{
		"SpanTurn":  SpanTurn,
		"SpanModel": SpanModel,
		"SpanTool":  SpanTool,
	}
	want := map[string]string{
		"SpanTurn":  "turn",
		"SpanModel": "gen_ai.client.operation",
		"SpanTool":  "tool.execute",
	}
	for name, got := range spans {
		if got != want[name] {
			t.Errorf("%s = %q, want %q; renaming is a breaking change for dashboards", name, got, want[name])
		}
	}
}

func TestMetricNamesPinned(t *testing.T) {
	metrics := map[string]string{
		"MetricTurnsTotal":    MetricTurnsTotal,
		"MetricTurnDuration":  MetricTurnDuration,
		"MetricModelDuration": MetricModelDuration,
		"MetricExportErrors":  MetricExportErrors,
		"MetricDroppedSpans":  MetricDroppedSpans,
	}
	want := map[string]string{
		"MetricTurnsTotal":    "runtime.turns.total",
		"MetricTurnDuration":  "runtime.turn.duration",
		"MetricModelDuration": "gen_ai.client.operation.duration",
		"MetricExportErrors":  "telemetry.export.errors",
		"MetricDroppedSpans":  "telemetry.dropped_spans.total",
	}
	for name, got := range metrics {
		if got != want[name] {
			t.Errorf("%s = %q, want %q; renaming is a breaking change for dashboards", name, got, want[name])
		}
	}
}

func TestTurnSpanAllowedAttrs(t *testing.T) {
	allowed := AllowedSpanAttrs(SpanTurn)
	want := []string{
		AttrSessionID,
		AttrTurnID,
		AttrOrigin,
		AttrTerminalKind,
		AttrSemconvVersion,
	}
	assertAttrSet(t, "turn span", allowed, want)
}

func TestModelSpanAllowedAttrs(t *testing.T) {
	allowed := AllowedSpanAttrs(SpanModel)
	want := []string{
		AttrGenAISystem,
		AttrGenAIRequestModel,
		AttrGenAIResponseModel,
		AttrGenAIOperationName,
		AttrGenAIUsageInputCount,
		AttrGenAIUsageOutputCount,
	}
	assertAttrSet(t, "model span", allowed, want)
}

func TestToolSpanAllowedAttrs(t *testing.T) {
	allowed := AllowedSpanAttrs(SpanTool)
	want := []string{
		AttrToolName,
		AttrToolStatus,
	}
	assertAttrSet(t, "tool span", allowed, want)
}

func TestMetricLabelsBounded(t *testing.T) {
	cases := []struct {
		metric string
		want   []string
	}{
		{MetricTurnsTotal, []string{AttrOrigin, AttrTerminalKind}},
		{MetricTurnDuration, []string{AttrOrigin, AttrTerminalKind}},
		{MetricModelDuration, []string{AttrGenAISystem, AttrGenAIOperationName}},
		{MetricExportErrors, nil},
		{MetricDroppedSpans, nil},
	}
	for _, tc := range cases {
		got := AllowedMetricLabels(tc.metric)
		if len(got) != len(tc.want) {
			t.Errorf("%s labels = %v, want %v", tc.metric, got, tc.want)
			continue
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Errorf("%s label[%d] = %q, want %q", tc.metric, i, got[i], tc.want[i])
			}
		}
	}
}

func TestMetricLabelsExcludeHighCardinality(t *testing.T) {
	highCardinality := []string{AttrSessionID, AttrTurnID}
	for metric, labels := range map[string][]string{
		MetricTurnsTotal:    AllowedMetricLabels(MetricTurnsTotal),
		MetricTurnDuration:  AllowedMetricLabels(MetricTurnDuration),
		MetricModelDuration: AllowedMetricLabels(MetricModelDuration),
	} {
		for _, label := range labels {
			for _, hc := range highCardinality {
				if label == hc {
					t.Errorf("%s has high-cardinality label %q; session/turn IDs belong in traces, not metrics", metric, hc)
				}
			}
		}
	}
}

func TestUnknownSpanHasNoAllowedAttrs(t *testing.T) {
	if got := AllowedSpanAttrs("nonexistent"); got != nil {
		t.Errorf("AllowedSpanAttrs(nonexistent) = %v, want nil", got)
	}
}

func assertAttrSet(t *testing.T, name string, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s allowed attrs = %v, want %v", name, got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("%s attr[%d] = %q, want %q", name, i, got[i], want[i])
		}
	}
}
