package codeexec

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"regexp"
	"strings"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/anggasct/aura/internal/effect"
	"github.com/anggasct/aura/internal/sandbox"
)

var sha256Hex = regexp.MustCompile(`^[0-9a-f]{64}$`)

type receiptView struct {
	ToolName     string      `json:"tool_name"`
	Class        string      `json:"class"`
	OutputDigest string      `json:"output_digest"`
	Audit        auditRecord `json:"audit"`
}

func succeededIntents(t *testing.T, harness *auditHarness) []effect.Intent {
	t.Helper()
	intents, err := harness.journal.ListByState(t.Context(), effect.StateSucceeded, 0)
	if err != nil {
		t.Fatal(err)
	}
	return intents
}

func TestEffectReceiptCarriesTheAuditRecord(t *testing.T) {
	cases := []struct {
		name         string
		runtime      string
		results      []sandbox.Result
		outputs      map[string]string
		declared     []string
		wantClass    ResultClass
		wantPhase    Phase
		wantArtifact int
		wantRefused  int
	}{
		{
			name: "python run with two outputs and a missing one", runtime: "python",
			results:   []sandbox.Result{{Stdout: "done"}},
			outputs:   map[string]string{"a.txt": "alpha", "b.csv": "x,y\n1,2\n"},
			declared:  []string{"a.txt", "b.csv", "missing.txt"},
			wantClass: ClassOK, wantPhase: PhaseRun, wantArtifact: 2, wantRefused: 1,
		},
		{
			name: "javascript run that exits non-zero", runtime: "javascript",
			results:   []sandbox.Result{{ExitCode: 2, Stderr: "boom"}},
			wantClass: ClassExitNonzero, wantPhase: PhaseRun,
		},
		{
			name: "go program that fails to build", runtime: "go",
			results:   []sandbox.Result{{ExitCode: 1, Stderr: "syntax error"}},
			wantClass: ClassExitNonzero, wantPhase: PhaseBuild,
		},
		{
			name: "go program that runs", runtime: "go",
			results:   []sandbox.Result{{}, {Stdout: "hi"}},
			wantClass: ClassOK, wantPhase: PhaseRun,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rig := newRig(t, nil)
			rig.stub.script = tc.results
			if len(tc.outputs) > 0 {
				rig.stub.before = writesOutputs(t, tc.outputs)
			}
			harness := newAuditHarness(t, rig.runner, t.TempDir(), 1<<20)
			arguments := map[string]any{"runtime": tc.runtime, "source": "code"}
			if len(tc.declared) > 0 {
				arguments["output_files"] = tc.declared
			}
			result, err := harness.call(arguments)
			if err != nil {
				t.Fatal(err)
			}
			intents := succeededIntents(t, harness)
			if len(intents) != 1 {
				t.Fatalf("succeeded intents = %d, want 1", len(intents))
			}
			intent := &intents[0]
			if intent.Operation != "code_execute" || intent.Classification != effect.ClassificationEffectful || !sha256Hex.MatchString(intent.RequestDigest) {
				t.Fatalf("intent = %+v", intent)
			}
			var receipt receiptView
			if err := json.Unmarshal(intent.ProviderReceipt, &receipt); err != nil {
				t.Fatalf("receipt %s: %v", intent.ProviderReceipt, err)
			}
			sum := sha256.Sum256(result.Output)
			if receipt.OutputDigest != hex.EncodeToString(sum[:]) {
				t.Errorf("output digest %s does not bind the returned result", receipt.OutputDigest)
			}
			audit := receipt.Audit
			bundle, err := rig.store.Bundle(tc.runtime)
			if err != nil {
				t.Fatal(err)
			}
			if !sha256Hex.MatchString(audit.RequestDigest) || audit.Runtime != tc.runtime || audit.RuntimeVersion != bundle.Runtime.Version ||
				audit.ResultClass != tc.wantClass || audit.Phase != tc.wantPhase {
				t.Errorf("audit = %+v", audit)
			}
			var returned encodedResult
			if err := json.Unmarshal(result.Output, &returned); err != nil {
				t.Fatal(err)
			}
			if returned.ResultClass != audit.ResultClass {
				t.Errorf("result class %s differs from the audited %s", returned.ResultClass, audit.ResultClass)
			}
			var returnedDigests, auditedDigests []string
			var returnedBytes int64
			for _, artifact := range returned.Artifacts {
				if artifact.Refused == "" {
					returnedDigests = append(returnedDigests, artifact.Digest)
					returnedBytes += artifact.Size
				}
			}
			for _, artifact := range audit.Artifacts {
				auditedDigests = append(auditedDigests, artifact.Digest)
				if !sha256Hex.MatchString(artifact.Digest) || artifact.ArtifactID == "" {
					t.Errorf("audited artifact = %+v", artifact)
				}
			}
			if strings.Join(returnedDigests, ",") != strings.Join(auditedDigests, ",") || len(auditedDigests) != tc.wantArtifact {
				t.Errorf("audited digests = %v, returned digests = %v, want %d", auditedDigests, returnedDigests, tc.wantArtifact)
			}
			if audit.ArtifactBytes != returnedBytes || audit.ArtifactsRefused != tc.wantRefused {
				t.Errorf("audited bytes = %d (returned %d), refused = %d (want %d)", audit.ArtifactBytes, returnedBytes, audit.ArtifactsRefused, tc.wantRefused)
			}
		})
	}
}

func TestAuditRecordNeverCarriesProgramContent(t *testing.T) {
	outcome := Outcome{
		Class: ClassExitNonzero, Runtime: "python", RuntimeVersion: "1", Phase: PhaseRun,
		Stdout: "stdout-content", Stderr: "stderr-content",
		Artifacts: []ArtifactEntry{{Name: "secret-name.txt", ArtifactID: "art-1", Digest: strings.Repeat("a", 64), Size: 3}, {Name: "refused-name.txt", Refused: RefusalAbsent}},
	}
	audit, err := newAudit(strings.Repeat("b", 64), &outcome)
	if err != nil {
		t.Fatal(err)
	}
	for _, content := range []string{"stdout-content", "stderr-content", "secret-name", "refused-name"} {
		if strings.Contains(string(audit), content) {
			t.Errorf("audit record carries %q: %s", content, audit)
		}
	}
}

func TestHarnessDetectsMarkersInLogsAndTelemetry(t *testing.T) {
	logs := &recordedLogs{}
	logger := slog.New(&recordingHandler{records: logs}).With("component", "probe").WithGroup("outer")
	logger.InfoContext(t.Context(), "static message", "plain", "value-in-log", slog.Group("inner", "key", "grouped-value"))
	joined := strings.Join(logs.all(), "\n")
	for _, want := range []string{"static message", "component=probe", "outer.plain=value-in-log", "outer.inner.key=grouped-value"} {
		if !strings.Contains(joined, want) {
			t.Errorf("record %q does not hold %q", joined, want)
		}
	}
	reader := sdkmetric.NewManualReader()
	meters := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = meters.Shutdown(context.Background()) })
	counter, err := meters.Meter("probe").Int64Counter("probe.total")
	if err != nil {
		t.Fatal(err)
	}
	counter.Add(t.Context(), 1, metric.WithAttributes(attribute.String("label", "metric-value")))
	exporter := tracetest.NewInMemoryExporter()
	_, span := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter)).Tracer("probe").Start(t.Context(), "probe-span")
	span.SetAttributes(attribute.String("attr", "span-value"))
	span.End()
	harness := &auditHarness{t: t, reader: reader, spans: exporter}
	text := harness.telemetryText()
	for _, want := range []string{"label=metric-value", "attr=span-value", "probe-span"} {
		if !strings.Contains(text, want) {
			t.Errorf("telemetry text %q does not hold %q", text, want)
		}
	}
}
