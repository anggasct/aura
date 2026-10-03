package vision

import (
	"bytes"
	"context"
	"testing"
	"time"
)

func TestObserveBucketsAndZeroesDimensions(t *testing.T) {
	var got *Observation
	svc, err := NewService(ptrLimits(), WithStore(newMemoryStore()), WithObserver(func(_ context.Context, o *Observation) { got = o }))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Ingest(t.Context(), &IngestRequest{
		Content:    bytes.NewReader(pngBytes(t, 300, 200)),
		SessionID:  "sess-1",
		Provenance: Provenance{Source: "terminal", ExternalID: "line:1", TurnID: "turn-1"},
	}); err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if got == nil {
		t.Fatal("no observation")
	}
	if got.Operation != "ingest" || got.Result != "ok" {
		t.Errorf("operation/result = %q/%q", got.Operation, got.Result)
	}
	if got.MIME != MIMEPNG {
		t.Errorf("mime = %q", got.MIME)
	}
	if got.Width != 0 || got.Height != 0 {
		t.Errorf("dimensions leaked: %dx%d", got.Width, got.Height)
	}
	if got.EncodedBytes != 1<<16 && got.EncodedBytes != 1<<18 && got.EncodedBytes != 1<<20 {
		t.Errorf("bytes not bucketed: %d", got.EncodedBytes)
	}
	if got.Images != 1 {
		t.Errorf("images = %d, want 1", got.Images)
	}
	if got.Version != "v1" {
		t.Errorf("version = %q", got.Version)
	}
	if got.Protocol != "" {
		t.Errorf("ingest protocol = %q, want empty", got.Protocol)
	}
}

func TestObserveFailureRedacts(t *testing.T) {
	var got *Observation
	svc, err := NewService(ptrLimits(), WithStore(newMemoryStore()), WithObserver(func(_ context.Context, o *Observation) { got = o }))
	if err != nil {
		t.Fatal(err)
	}
	_, err = svc.Ingest(t.Context(), &IngestRequest{
		Content:    bytes.NewReader(pngBytes(t, 8, 8)),
		SessionID:  "sess-1",
		Provenance: Provenance{Source: "terminal", ExternalID: "line:1", TurnID: "turn-1"},
	})
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	_ = time.Now
	if got == nil || got.Result != "ok" {
		t.Fatalf("observation = %+v", got)
	}
	var failed *Observation
	svc2, err := NewService(ptrLimits(), WithStore(newMemoryStore()), WithObserver(func(_ context.Context, o *Observation) { failed = o }))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc2.Ingest(t.Context(), &IngestRequest{
		Content:    bytes.NewReader([]byte("not an image")),
		SessionID:  "sess-1",
		Provenance: Provenance{Source: "terminal", ExternalID: "line:1", TurnID: "turn-1"},
	}); err == nil {
		t.Fatal("expected failure")
	}
	if failed == nil {
		t.Fatal("no failure observation")
	}
	if failed.MIME != "" {
		t.Errorf("failure mime = %q, want empty", failed.MIME)
	}
	if failed.Width != 0 || failed.Height != 0 {
		t.Errorf("failure dimensions leaked")
	}
	if failed.Result == "" || failed.Result == "ok" {
		t.Errorf("failure result = %q", failed.Result)
	}
}

func TestSanitizeBoundsCardinality(t *testing.T) {
	if got := sanitizeMIME("image/png; charset=x"); got != "" {
		t.Errorf("mime with params = %q, want empty (strict)", got)
	}
	if got := sanitizeMIME(MIMEPNG); got != MIMEPNG {
		t.Errorf("mime png = %q", got)
	}
	if got := sanitizeOperation("exfiltrate"); got != "ingest" {
		t.Errorf("operation hostile = %q", got)
	}
	if got := sanitizeProtocol("evil://channel"); got != "" {
		t.Errorf("protocol hostile = %q", got)
	}
}
