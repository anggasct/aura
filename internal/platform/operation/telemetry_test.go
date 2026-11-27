package operation_test

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/anggasct/aura/internal/platform/operation"
	"github.com/anggasct/aura/internal/platform/operation/operationtest"
	"github.com/anggasct/aura/internal/platform/telemetry"
)

const sensitiveSubject = "never-in-telemetry-value"

type observedInvocations struct {
	reader *sdkmetric.ManualReader
	logs   *bytes.Buffer
	rig    *operationtest.Rig
}

func newObservedInvocations(t *testing.T) *observedInvocations {
	t.Helper()
	reader := sdkmetric.NewManualReader()
	recorder, err := telemetry.NewOperationRecorder(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)))
	if err != nil {
		t.Fatalf("NewOperationRecorder: %v", err)
	}
	logs := &bytes.Buffer{}
	rig := operationtest.NewRig(t, &operationtest.Options{
		Telemetry: recorder,
		Logger:    slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
	})
	return &observedInvocations{reader: reader, logs: logs, rig: rig}
}

func (o *observedInvocations) counts(t *testing.T) map[attribute.Set]int64 {
	t.Helper()
	var collected metricdata.ResourceMetrics
	if err := o.reader.Collect(t.Context(), &collected); err != nil {
		t.Fatalf("Collect: %v", err)
	}
	counts := map[attribute.Set]int64{}
	for _, scope := range collected.ScopeMetrics {
		for _, point := range scope.Metrics {
			if point.Name != telemetry.MetricOperationInvocationsTotal {
				continue
			}
			sum, ok := point.Data.(metricdata.Sum[int64])
			if !ok {
				t.Fatalf("%s is a %T, want an int64 sum", point.Name, point.Data)
			}
			for _, dataPoint := range sum.DataPoints {
				counts[dataPoint.Attributes] += dataPoint.Value
			}
		}
	}
	return counts
}

func invocationAttributes(id, surface, risk, outcome string) attribute.Set {
	return attribute.NewSet(
		attribute.String(telemetry.AttrOperationID, id),
		attribute.String(telemetry.AttrOperationSurface, surface),
		attribute.String(telemetry.AttrOperationRisk, risk),
		attribute.String(telemetry.AttrOperationOutcome, outcome),
	)
}

func TestOperationTelemetry(t *testing.T) {
	observed := newObservedInvocations(t)
	rig := observed.rig
	rig.Stubs.FailWith(operationtest.External, errBoom)
	ctx := t.Context()
	invoke := func(id string, c *operation.Call) { _, _ = rig.Registry.Invoke(ctx, id, c) }

	invoke(operationtest.Read, call(operation.SurfaceCLI, "", "", sensitiveSubject))
	invoke(operationtest.Change, call(operation.SurfaceAgent, "key-1", "digest-1", sensitiveSubject))
	invoke(operationtest.Host, call(operation.SurfaceAgent, "key-2", "digest-2", sensitiveSubject))
	invoke(operationtest.External, call(operation.SurfaceAgent, "key-3", "digest-3", sensitiveSubject))
	invoke("stub.ghost", call(operation.SurfaceCLI, "", "", sensitiveSubject))
	if _, err := rig.Registry.Preview(ctx, operationtest.Delete, call(operation.SurfaceAgent, "key-4", "digest-4", sensitiveSubject)); err != nil {
		t.Fatalf("Preview: %v", err)
	}

	want := map[attribute.Set]int64{
		invocationAttributes(operationtest.Read, "cli", "read", "ok"):                   1,
		invocationAttributes(operationtest.Change, "agent", "change", "ok"):             1,
		invocationAttributes(operationtest.Host, "agent", "change", "refused"):          1,
		invocationAttributes(operationtest.External, "agent", "change", "failed"):       1,
		invocationAttributes("unknown", "", "", "refused"):                              1,
		invocationAttributes(operationtest.Delete, "agent", "destructive", "previewed"): 1,
	}
	got := observed.counts(t)
	if len(got) != len(want) {
		t.Fatalf("invocation points = %d, want %d: %v", len(got), len(want), got)
	}
	for attributes, count := range want {
		if got[attributes] != count {
			t.Errorf("invocations %v = %d, want %d", attributes.ToSlice(), got[attributes], count)
		}
	}

	records := strings.Split(strings.TrimSpace(observed.logs.String()), "\n")
	if len(records) != 6 {
		t.Fatalf("log records = %d, want one per invocation: %s", len(records), observed.logs.String())
	}
	for _, line := range records {
		if strings.Contains(line, sensitiveSubject) {
			t.Errorf("a log record carries the operation input: %s", line)
		}
		var record map[string]any
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("decode a log record: %v", err)
		}
		if record["msg"] != "operation invoked" || record["component"] != "operation" || record["level"] != "INFO" {
			t.Errorf("record = %v, want the operation invoked info record", record)
		}
		for _, key := range []string{"operation", "surface", "risk", "status", "latency_ms"} {
			if _, present := record[key]; !present {
				t.Errorf("record %v lacks %q", record, key)
			}
		}
	}
}

func TestOperationTelemetryCountsBrokerCalls(t *testing.T) {
	observed := newObservedInvocations(t)
	outcome := observed.rig.Execute(t.Context(), operationtest.Request(operationtest.ToolOf(operationtest.Change), sensitiveSubject))
	if outcome.Err != nil {
		t.Fatalf("Execute: %v", outcome.Err)
	}
	counts := observed.counts(t)
	if got := counts[invocationAttributes(operationtest.Change, "agent", "change", "ok")]; got != 1 {
		t.Errorf("agent invocations through the broker = %d, want 1 (%v)", got, counts)
	}
	if strings.Contains(observed.logs.String(), sensitiveSubject) {
		t.Errorf("a log record carries the operation input: %s", observed.logs.String())
	}
}
