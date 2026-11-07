package codeexec

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/anggasct/aura/internal/approval"
	"github.com/anggasct/aura/internal/capability/capabilitytest"
	"github.com/anggasct/aura/internal/effect"
	"github.com/anggasct/aura/internal/store"
	"github.com/anggasct/aura/internal/store/storetest"
	"github.com/anggasct/aura/internal/telemetry"
	"github.com/anggasct/aura/internal/toolbroker"
	"github.com/anggasct/aura/internal/tools"
)

const (
	auditSessionID = "session-1"
	auditToolKey   = "code_execute@v1"
)

type recordingHandler struct {
	records *recordedLogs
	prefix  []slog.Attr
	groups  []string
}

type recordedLogs struct {
	mu    sync.Mutex
	lines []string
}

func (l *recordedLogs) add(line string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, line)
}

func (l *recordedLogs) all() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.lines...)
}

func (h *recordingHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *recordingHandler) Handle(_ context.Context, record slog.Record) error {
	var line strings.Builder
	line.WriteString(record.Message)
	for _, attr := range h.prefix {
		writeAttr(&line, "", attr)
	}
	record.Attrs(func(attr slog.Attr) bool {
		writeAttr(&line, strings.Join(h.groups, "."), attr)
		return true
	})
	h.records.add(line.String())
	return nil
}

func (h *recordingHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	next := *h
	next.prefix = append(append([]slog.Attr(nil), h.prefix...), attrs...)
	return &next
}

func (h *recordingHandler) WithGroup(name string) slog.Handler {
	next := *h
	next.groups = append(append([]string(nil), h.groups...), name)
	return &next
}

func writeAttr(line *strings.Builder, group string, attr slog.Attr) {
	value := attr.Value.Resolve()
	key := attr.Key
	if group != "" {
		key = group + "." + key
	}
	if value.Kind() == slog.KindGroup {
		for _, member := range value.Group() {
			writeAttr(line, key, member)
		}
		return
	}
	fmt.Fprintf(line, " %s=%v", key, value.Any())
}

type auditHarness struct {
	t         *testing.T
	logs      *recordedLogs
	reader    *sdkmetric.ManualReader
	spans     *tracetest.InMemoryExporter
	journal   *effect.Journal
	broker    *toolbroker.Broker
	artifacts store.ArtifactStore
	quota     func() int64
	sequence  uint64
	observed  observations
}

type observations struct {
	mu    sync.Mutex
	items []Observation
}

func (o *observations) add(observation Observation) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.items = append(o.items, observation)
}

func (o *observations) all() []Observation {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]Observation(nil), o.items...)
}

func newAuditHarness(t *testing.T, runner *Runner, artifactRoot string, artifactQuota int64) *auditHarness {
	t.Helper()
	harness := &auditHarness{t: t, logs: &recordedLogs{}, reader: sdkmetric.NewManualReader(), spans: tracetest.NewInMemoryExporter()}
	logger := slog.New(&recordingHandler{records: harness.logs})
	meters := sdkmetric.NewMeterProvider(sdkmetric.WithReader(harness.reader))
	tracers := sdktrace.NewTracerProvider(sdktrace.WithSyncer(harness.spans))
	t.Cleanup(func() { _ = meters.Shutdown(context.Background()) })
	toolRecorder, err := telemetry.NewToolRecorder(tracers, meters)
	if err != nil {
		t.Fatal(err)
	}
	codeRecorder, err := telemetry.NewCodeRecorder(meters)
	if err != nil {
		t.Fatal(err)
	}
	runner.observer = func(ctx context.Context, observation *Observation) {
		harness.observed.add(*observation)
		codeRecorder.Record(ctx, &telemetry.CodeObservation{
			Runtime: observation.Runtime, ResultClass: string(observation.Class), Phase: string(observation.Phase),
			Compiled: observation.Compiled, BuildDuration: observation.Build.Duration, RunDuration: observation.Run.Duration,
			BuildCPU: observation.Build.CPUTime, RunCPU: observation.Run.CPUTime,
			OutputBytes: observation.StdoutBytes + observation.StderrBytes, ArtifactBytes: observation.ArtifactBytes,
		})
	}
	db := storetest.OpenMigrated(t)
	now := time.Now().UTC()
	if err := store.NewSessionService(db).Create(t.Context(), &store.Session{ID: auditSessionID, OwnerID: "owner-1", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	harness.artifacts = store.NewArtifactStore(db, artifactRoot, artifactQuota)
	harness.quota = func() int64 {
		var total int64
		if err := db.QueryRowContext(t.Context(), "SELECT COALESCE(SUM(size_bytes), 0) FROM blob").Scan(&total); err != nil {
			t.Fatal(err)
		}
		return total
	}
	runner.artifacts = storeSink{store: harness.artifacts}
	journal, err := effect.NewJournal(db, effect.Options{Logger: logger})
	if err != nil {
		t.Fatal(err)
	}
	executor, err := effect.NewExecutor(journal)
	if err != nil {
		t.Fatal(err)
	}
	broker, err := toolbroker.New(&toolbroker.Options{
		HostCapabilities: capabilitytest.Host(t), Effects: executor, Artifacts: harness.artifacts,
		Adapters:       map[string]toolbroker.Adapter{auditToolKey: runner.Adapter()},
		ArgumentChecks: map[string]toolbroker.ArgumentCheck{auditToolKey: runner.ArgumentCheck},
		ApprovalDecider: func(context.Context, *toolbroker.ApprovalPrompt) (bool, error) {
			return true, nil
		},
		Observer: func(ctx context.Context, observation toolbroker.Observation) {
			toolRecorder.Record(ctx, &telemetry.ToolObservation{
				Name: observation.ToolName + "@" + observation.ToolVersion, Status: string(observation.Class),
				PolicyOutcome: observation.PolicyOutcome, Approval: observation.Approval, Executor: observation.Executor,
				Duration: observation.Duration, OutputBytes: observation.OutputBytes,
			})
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	harness.journal, harness.broker = journal, broker
	return harness
}

func (h *auditHarness) call(arguments any) (toolbroker.ToolResult, error) {
	h.t.Helper()
	encoded, err := json.Marshal(arguments)
	if err != nil {
		h.t.Fatal(err)
	}
	h.sequence++
	return h.broker.Execute(h.t.Context(), &toolbroker.ToolRequest{
		RequestID: fmt.Sprintf("call-%d", h.sequence), TurnID: "turn-1", SessionID: auditSessionID, PrincipalID: "owner-1",
		ToolName: tools.CodeExecuteToolName, ToolVersion: "v1", Arguments: encoded, Capabilities: []string{"code-interpreter"},
		Trust: approval.TrustOwnerInput, IdempotencyKey: fmt.Sprintf("key-%d", h.sequence), EventSequence: h.sequence,
		EventInvocation: "inv-1", EventBranch: "main", EventAuthor: "agent",
	})
}

func (h *auditHarness) telemetryText() string {
	h.t.Helper()
	var text strings.Builder
	var resource metricdata.ResourceMetrics
	if err := h.reader.Collect(h.t.Context(), &resource); err != nil {
		h.t.Fatal(err)
	}
	addSet := func(set attribute.Set) {
		for _, kv := range set.ToSlice() {
			fmt.Fprintf(&text, " %s=%s", kv.Key, kv.Value.Emit())
		}
	}
	for _, scope := range resource.ScopeMetrics {
		for _, m := range scope.Metrics {
			text.WriteString("\nmetric " + m.Name + " " + m.Description + " " + m.Unit)
			switch data := m.Data.(type) {
			case metricdata.Sum[int64]:
				for _, dp := range data.DataPoints {
					addSet(dp.Attributes)
				}
			case metricdata.Histogram[float64]:
				for _, dp := range data.DataPoints {
					addSet(dp.Attributes)
				}
			case metricdata.Histogram[int64]:
				for _, dp := range data.DataPoints {
					addSet(dp.Attributes)
				}
			}
		}
	}
	for _, span := range h.spans.GetSpans() {
		text.WriteString("\nspan " + span.Name + " " + span.Status.Description)
		for _, kv := range span.Attributes {
			fmt.Fprintf(&text, " %s=%s", kv.Key, kv.Value.Emit())
		}
		for _, event := range span.Events {
			text.WriteString(" event " + event.Name)
			for _, kv := range event.Attributes {
				fmt.Fprintf(&text, " %s=%s", kv.Key, kv.Value.Emit())
			}
		}
	}
	return text.String()
}

func (h *auditHarness) assertMarkerFree(markers ...string) {
	h.t.Helper()
	logs := h.logs.all()
	if len(logs) == 0 {
		h.t.Fatal("no log record was captured, so the check proves nothing")
	}
	text := h.telemetryText()
	if !strings.Contains(text, "code.runs.total") || !strings.Contains(text, "tools.calls.total") {
		h.t.Fatalf("telemetry did not record the run: %s", text)
	}
	for _, marker := range markers {
		for _, line := range logs {
			if strings.Contains(line, marker) {
				h.t.Errorf("marker %q appears in a log record: %s", marker, line)
			}
		}
		if strings.Contains(text, marker) {
			h.t.Errorf("marker %q appears in telemetry", marker)
		}
	}
}
