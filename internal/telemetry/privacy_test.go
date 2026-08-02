package telemetry

import (
	"context"
	"testing"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/anggasct/aura/internal/runtime"
	"github.com/anggasct/aura/internal/store"
)

var canaryValues = map[string]string{
	"prompt":         "You are a helpful assistant. Secret system prompt.",
	"content":        "The user said: transfer $10000 to account 12345",
	"message":        "Hello, my password is hunter2",
	"text":           "The API key is sk-live-abc123def456",
	"argument":       `{"command": "rm -rf /"}`,
	"result":         "Tool returned: /etc/passwd contents...",
	"memory":         "User prefers dark mode and lives at 123 Main St",
	"attachment":     "base64encodedfiledata...",
	"secret":         "AWS_SECRET_ACCESS_KEY=wJalrXUtnFEMI",
	"token":          "ghp_1234567890abcdef",
	"password":       "correcthorsebatterystaple",
	"credential":     "-----BEGIN RSA PRIVATE KEY-----",
	"body":           `{"ssn": "123-45-6789"}`,
	"payload":        "eyJhbGciOiJIUzI1NiJ9.secret.jwt",
	"input":          "User typed: my credit card is 4111111111111111",
	"output":         "Model responded with PII: John Doe, SSN 987-65-4321",
	"key":            "sk-ant-api03-xxxxxxxxxxxx",
	"api_key":        "sk-proj-abcdefghijklmnop",
	"tool_arguments": `{"file": "/etc/shadow"}`,
	"tool_result":    "root:x:0:0:root:/root:/bin/bash",
	"user_profile":   "Age 34, income $150k, medical condition: diabetes",
	"skill_text":     "Custom skill: exfiltrate data to evil.com",
	"filename":       "/home/user/.ssh/id_rsa",
	"url":            "https://internal.corp.example.com/admin",
	"header":         "Authorization: Bearer eyJhbG...",
}

func TestPrivacyCanaryTurnSpan(t *testing.T) {
	exporter := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
	fr := &fakeRuntime{events: []store.RuntimeEvent{{Kind: runtime.EventKindTurnCompleted}}}
	inst, err := InstrumentRuntime(fr, tp, nil, nil)
	if err != nil {
		t.Fatalf("InstrumentRuntime: %v", err)
	}

	req := sampleTurnRequest()
	for _, err := range inst.Run(context.Background(), req) {
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	}

	spans := exporter.GetSpans()
	for _, span := range spans {
		for _, attr := range span.Attributes {
			val := attr.Value.AsString()
			for canaryKey, canaryVal := range canaryValues {
				if val == canaryVal {
					t.Errorf("canary %q value leaked into span %q attribute %q", canaryKey, span.Name, attr.Key)
				}
			}
			if isContentLeak(string(attr.Key)) {
				t.Errorf("content-bearing key %q present in span %q", attr.Key, span.Name)
			}
		}
		if span.Status.Description != "" {
			for canaryKey, canaryVal := range canaryValues {
				if span.Status.Description == canaryVal {
					t.Errorf("canary %q value leaked into span status description", canaryKey)
				}
			}
		}
		for _, ev := range span.Events {
			for _, attr := range ev.Attributes {
				val := attr.Value.AsString()
				for canaryKey, canaryVal := range canaryValues {
					if val == canaryVal {
						t.Errorf("canary %q value leaked into span event %q attribute %q", canaryKey, ev.Name, attr.Key)
					}
				}
			}
		}
	}
}

func TestPrivacyCanaryModelSpan(t *testing.T) {
	exporter := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
	fr := &fakeRuntime{events: []store.RuntimeEvent{{Kind: runtime.EventKindTurnCompleted}}}
	inst, err := InstrumentRuntime(fr, tp, nil, nil)
	if err != nil {
		t.Fatalf("InstrumentRuntime: %v", err)
	}

	ctx := context.Background()
	_, span, start := inst.StartModelSpan(ctx, ModelSpanParams{
		System:       "openai",
		RequestModel: "gpt-4o",
		Operation:    "chat",
	})
	inst.EndModelSpan(ctx, span, start, "openai", "gpt-4o-2024-08-06", 150, 75, nil)

	spans := exporter.GetSpans()
	for _, s := range spans {
		for _, attr := range s.Attributes {
			val := attr.Value.AsString()
			for canaryKey, canaryVal := range canaryValues {
				if val == canaryVal {
					t.Errorf("canary %q value leaked into model span attribute %q", canaryKey, attr.Key)
				}
			}
		}
	}
}

func TestPrivacyCanaryToolSpan(t *testing.T) {
	exporter := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
	fr := &fakeRuntime{events: []store.RuntimeEvent{{Kind: runtime.EventKindTurnCompleted}}}
	inst, err := InstrumentRuntime(fr, tp, nil, nil)
	if err != nil {
		t.Fatalf("InstrumentRuntime: %v", err)
	}

	ctx := context.Background()
	_, span := inst.StartToolSpan(ctx, ToolSpanParams{Name: "web_search"})
	inst.EndToolSpan(span, "completed", nil)

	spans := exporter.GetSpans()
	for _, s := range spans {
		for _, attr := range s.Attributes {
			val := attr.Value.AsString()
			for canaryKey, canaryVal := range canaryValues {
				if val == canaryVal {
					t.Errorf("canary %q value leaked into tool span attribute %q", canaryKey, attr.Key)
				}
			}
		}
	}
}

func TestPrivacyRedactAllCanaryKeys(t *testing.T) {
	attrs := make(map[string]any, len(canaryValues)+2)
	for k, v := range canaryValues {
		attrs[k] = v
	}
	attrs[AttrSessionID] = "session-safe"
	attrs[AttrOrigin] = "terminal"

	out := Redact(attrs)

	for canaryKey := range canaryValues {
		if _, ok := out[canaryKey]; ok {
			t.Errorf("canary key %q was not redacted", canaryKey)
		}
	}
	if _, ok := out[AttrSessionID]; !ok {
		t.Errorf("safe key %q was incorrectly redacted", AttrSessionID)
	}
	if _, ok := out[AttrOrigin]; !ok {
		t.Errorf("safe key %q was incorrectly redacted", AttrOrigin)
	}
}
