package runtimeadk

import (
	"strings"
	"testing"

	"github.com/anggasct/aura/internal/runtime"
	runtimeingress "github.com/anggasct/aura/internal/runtime/ingress"
	"github.com/anggasct/aura/internal/vision"
)

func testImageEnvelope(t *testing.T, alt string) []byte {
	t.Helper()
	part := &vision.ImagePart{
		Kind:             vision.PartKindImageRef,
		ArtifactID:       "art-test-1",
		SourceDigest:     "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		DerivedDigest:    "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		MIME:             "image/png",
		Width:            8,
		Height:           8,
		EncodedBytes:     128,
		TransformVersion: "v1",
		Transform:        vision.TransformGeometry{Width: 8, Height: 8, Kernel: "lanczos3"},
		Provenance: vision.Provenance{
			Source:     "terminal:owner",
			ExternalID: "message:1",
			SessionID:  "sess-1",
			TurnID:     "turn-1",
			IngestedAt: "2026-10-03T00:00:00Z",
		},
		AltText: alt,
		Trust:   vision.TrustUntrustedExternal,
	}
	raw, err := vision.MarshalPart(part)
	if err != nil {
		t.Fatalf("MarshalPart: %v", err)
	}
	return raw
}

func TestContentFromPartsAttachesImageReference(t *testing.T) {
	envelope := testImageEnvelope(t, "a red square")
	req := &runtime.TurnRequest{
		Parts: []runtimeingress.InputPart{
			{Text: "what is this"},
			{Image: envelope},
		},
	}
	content, err := contentFromParts(req)
	if err != nil {
		t.Fatalf("contentFromParts: %v", err)
	}
	if len(content.Parts) != 3 {
		t.Fatalf("parts = %d, want 3 (text, image ref, alt text)", len(content.Parts))
	}
	if content.Parts[0].Text != "what is this" {
		t.Fatalf("order broken: %+v", content.Parts[0])
	}
	file := content.Parts[1].FileData
	if file == nil || file.FileURI != "artifact://art-test-1" || file.MIMEType != "image/png" {
		t.Fatalf("file reference = %+v", file)
	}
	if content.Parts[1].PartMetadata[vision.PartMetadataKey] != string(envelope) {
		t.Fatal("image envelope metadata is missing")
	}
	if content.Parts[2].Text != "a red square" {
		t.Fatalf("alt text = %q", content.Parts[2].Text)
	}
}

func TestContentFromPartsRejectsMalformedImage(t *testing.T) {
	req := &runtime.TurnRequest{
		Parts: []runtimeingress.InputPart{{Image: []byte(`{"kind":"bogus"}`)}},
	}
	if _, err := contentFromParts(req); err == nil {
		t.Fatal("expected malformed image rejection")
	}
}

func TestContentFromPartsImageOnlyTurn(t *testing.T) {
	req := &runtime.TurnRequest{
		Parts: []runtimeingress.InputPart{{Image: testImageEnvelope(t, "")}},
	}
	content, err := contentFromParts(req)
	if err != nil {
		t.Fatalf("contentFromParts: %v", err)
	}
	if len(content.Parts) != 1 || content.Parts[0].FileData == nil {
		t.Fatalf("parts = %+v", content.Parts)
	}
}

func TestContentFromPartsKeepsBytesOut(t *testing.T) {
	envelope := testImageEnvelope(t, "")
	req := &runtime.TurnRequest{
		Parts: []runtimeingress.InputPart{{Image: envelope}},
	}
	content, err := contentFromParts(req)
	if err != nil {
		t.Fatalf("contentFromParts: %v", err)
	}
	if content.Parts[0].InlineData != nil {
		t.Fatal("raw bytes must not enter the canonical turn content")
	}
	if strings.Contains(string(envelope), "art-test-1") && content.Parts[0].FileData.FileURI == "" {
		t.Fatal("reference must carry the artifact id")
	}
}
