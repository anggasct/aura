package app

import (
	"context"
	"testing"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/anggasct/aura/internal/modules/workflow"
	"github.com/anggasct/aura/internal/platform/telemetry"
	"github.com/anggasct/aura/internal/platform/toolbroker"
)

func TestToolObserverReadsTheStepCall(t *testing.T) {
	exporter := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })
	recorder, err := telemetry.NewToolRecorder(tp, nil)
	if err != nil {
		t.Fatalf("NewToolRecorder: %v", err)
	}
	observe := toolObserver(recorder)
	observation := toolbroker.Observation{ToolName: "read_file", ToolVersion: "v1", Class: toolbroker.ResultOK}

	observe(workflow.WithStepCall(t.Context(), &workflow.StepCall{RunID: "run-7", StepID: "step-3"}), observation)
	observe(t.Context(), observation)

	spans := exporter.GetSpans()
	if len(spans) != 2 {
		t.Fatalf("spans = %d, want 2", len(spans))
	}
	attrs := func(index int) map[string]string {
		values := map[string]string{}
		for _, kv := range spans[index].Attributes {
			values[string(kv.Key)] = kv.Value.String()
		}
		return values
	}
	stepped := attrs(0)
	if stepped[telemetry.AttrWorkflowRunID] != "run-7" || stepped[telemetry.AttrWorkflowStepID] != "step-3" {
		t.Errorf("span with a step call carries %v, want run-7 and step-3", stepped)
	}
	bare := attrs(1)
	for _, key := range []string{telemetry.AttrWorkflowRunID, telemetry.AttrWorkflowStepID} {
		if _, present := bare[key]; present {
			t.Errorf("span without a step call carries %s", key)
		}
	}
}
