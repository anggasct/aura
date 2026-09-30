package vision

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

func ptrLimits() *Limits {
	limits := testLimits()
	return &limits
}

func testLimits() Limits {
	return Limits{
		MaxEncodedBytes:      20971520,
		MaxPixels:            40000000,
		MaxDimension:         8192,
		DecodeTimeout:        5 * time.Second,
		MaxDecodeConcurrency: 4,
		MaxTransformMemory:   268435456,
		StripMetadata:        true,
		OrientNormalize:      true,
		TransformVersion:     "v1",
		Now:                  time.Now,
	}
}

type memoryStore struct {
	mu   sync.Mutex
	refs map[string][]byte
	puts int
}

func newMemoryStore() *memoryStore {
	return &memoryStore{refs: map[string][]byte{}}
}

func (m *memoryStore) Put(ctx context.Context, r io.Reader, meta *ArtifactMetadata) (ArtifactRef, error) {
	if ctx == nil {
		return ArtifactRef{}, errNilArgument("context must not be nil")
	}
	if meta == nil {
		return ArtifactRef{}, errNilArgument("artifact metadata must not be nil")
	}
	raw, err := io.ReadAll(r)
	if err != nil {
		return ArtifactRef{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	id := meta.ID
	if id == "" {
		id = "art-test"
	}
	sum := sha256.Sum256(raw)
	m.refs[id] = raw
	m.puts++
	return ArtifactRef{ID: id, BlobDigest: hex.EncodeToString(sum[:]), SizeBytes: int64(len(raw))}, nil
}

func pngBytes(t *testing.T, width, height int) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, width, height))
	for y := range height {
		for x := range width {
			img.Set(x, y, color.NRGBA{R: uint8(x), G: uint8(y), B: 128, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func jpegBytes(t *testing.T, width, height int) []byte {
	t.Helper()
	img := image.NewYCbCr(image.Rect(0, 0, width, height), image.YCbCrSubsampleRatio420)
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 80}); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func gifBytes(t *testing.T, frames int) []byte {
	t.Helper()
	palette := color.Palette{color.Black, color.White}
	g := &gif.GIF{}
	for range frames {
		frame := image.NewPaletted(image.Rect(0, 0, 8, 8), palette)
		g.Image = append(g.Image, frame)
		g.Delay = append(g.Delay, 10)
	}
	var buf bytes.Buffer
	if err := gif.EncodeAll(&buf, g); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestIngestPNG(t *testing.T) {
	store := newMemoryStore()
	svc, err := NewService(ptrLimits(), WithStore(store))
	if err != nil {
		t.Fatal(err)
	}
	raw := pngBytes(t, 16, 12)
	part, err := svc.Ingest(t.Context(), &IngestRequest{
		Content:      bytes.NewReader(raw),
		DeclaredMIME: MIMEPNG,
		Filename:     "shot.png",
		SessionID:    "sess-1",
		Provenance:   Provenance{Source: "terminal", ExternalID: "line:1", TurnID: "turn-1"},
	})
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if part.MIME != MIMEPNG || part.Width != 16 || part.Height != 12 {
		t.Errorf("part = %+v", part)
	}
	if part.Kind != PartKindImageRef || part.Trust != TrustUntrustedExternal {
		t.Errorf("part kind/trust = %+v", part)
	}
	if !strings.HasPrefix(part.SourceDigest, "sha256:") || !strings.HasPrefix(part.DerivedDigest, "sha256:") {
		t.Errorf("digests = %+v", part)
	}
	if store.puts != 2 {
		t.Errorf("puts = %d, want 2 (source + derived)", store.puts)
	}
	encoded, err := MarshalPart(&part)
	if err != nil {
		t.Fatalf("MarshalPart: %v", err)
	}
	if strings.Contains(string(encoded), "iVBOR") {
		t.Error("serialized part embeds base64 pixels")
	}
	roundTrip, err := UnmarshalPart(encoded)
	if err != nil {
		t.Fatalf("UnmarshalPart: %v", err)
	}
	if roundTrip != part {
		t.Errorf("round trip = %+v, want %+v", roundTrip, part)
	}
}

func TestIngestJPEGAndGIF(t *testing.T) {
	store := newMemoryStore()
	svc, err := NewService(ptrLimits(), WithStore(store))
	if err != nil {
		t.Fatal(err)
	}
	jpegPart, err := svc.Ingest(t.Context(), &IngestRequest{
		Content:      bytes.NewReader(jpegBytes(t, 10, 8)),
		DeclaredMIME: MIMEJPEG,
		SessionID:    "sess-1",
		Provenance:   Provenance{Source: "terminal", TurnID: "turn-1"},
	})
	if err != nil {
		t.Fatalf("jpeg Ingest: %v", err)
	}
	if jpegPart.MIME != MIMEJPEG {
		t.Errorf("mime = %q", jpegPart.MIME)
	}
	gifPart, err := svc.Ingest(t.Context(), &IngestRequest{
		Content:      bytes.NewReader(gifBytes(t, 1)),
		DeclaredMIME: MIMEgif,
		SessionID:    "sess-1",
		Provenance:   Provenance{Source: "terminal", TurnID: "turn-1"},
	})
	if err != nil {
		t.Fatalf("gif Ingest: %v", err)
	}
	if gifPart.MIME != MIMEgif {
		t.Errorf("mime = %q", gifPart.MIME)
	}
}

func TestHostileCorpus(t *testing.T) {
	store := newMemoryStore()
	svc, err := NewService(ptrLimits(), WithStore(store))
	if err != nil {
		t.Fatal(err)
	}
	rawPNG := pngBytes(t, 8, 8)
	cases := map[string]struct {
		raw      []byte
		declared string
		code     ErrorCode
	}{
		"empty":           {[]byte{}, "", ErrorCodeVisionInvalid},
		"text":            {[]byte("hello world, not an image"), "", ErrorCodeVisionFormatUnsupported},
		"spoofed mime":    {rawPNG, MIMEJPEG, ErrorCodeVisionInvalid},
		"truncated png":   {rawPNG[:20], MIMEPNG, ErrorCodeVisionDecodeFailed},
		"truncated jpeg":  {[]byte{0xFF, 0xD8, 0xFF, 0xE0}, MIMEJPEG, ErrorCodeVisionDecodeFailed},
		"heic magic":      {[]byte("....ftypheic...."), "", ErrorCodeVisionFormatUnsupported},
		"bmp magic":       {[]byte("BM................"), "", ErrorCodeVisionFormatUnsupported},
		"tiff magic":      {[]byte("II*\x00........"), "", ErrorCodeVisionFormatUnsupported},
		"multi frame gif": {gifBytes(t, 2), MIMEgif, ErrorCodeVisionLimitExceeded},
		"svg text":        {[]byte("<svg xmlns='http://www.w3.org/2000/svg'></svg>"), "", ErrorCodeVisionFormatUnsupported},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			puts := store.puts
			_, err := svc.Ingest(t.Context(), &IngestRequest{
				Content:      bytes.NewReader(tc.raw),
				DeclaredMIME: tc.declared,
				SessionID:    "sess-1",
				Provenance:   Provenance{Source: "terminal", TurnID: "turn-1"},
			})
			if err == nil {
				t.Fatal("expected error")
			}
			if code, ok := CodeOf(err); !ok || code != tc.code {
				t.Errorf("code = %v, %v (%v), want %q", code, ok, err, tc.code)
			}
			if store.puts != puts {
				t.Errorf("hostile input wrote %d artifacts", store.puts-puts)
			}
		})
	}
}

func TestLimitsEnforced(t *testing.T) {
	limits := testLimits()
	limits.MaxEncodedBytes = 64
	limits.MaxPixels = 16
	limits.MaxDimension = 4
	svc, err := NewService(&limits, WithStore(newMemoryStore()))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Ingest(t.Context(), &IngestRequest{
		Content:    bytes.NewReader(pngBytes(t, 16, 16)),
		SessionID:  "sess-1",
		Provenance: Provenance{Source: "terminal", TurnID: "turn-1"},
	}); err == nil {
		t.Error("expected limit error")
	} else if code, ok := CodeOf(err); !ok || code != ErrorCodeVisionLimitExceeded {
		t.Errorf("code = %v (%v)", code, err)
	}
}

func TestDeterminism(t *testing.T) {
	raw := pngBytes(t, 12, 10)
	var first, second ImagePart
	for i, dest := range []*ImagePart{&first, &second} {
		svc, err := NewService(ptrLimits(), WithStore(newMemoryStore()))
		if err != nil {
			t.Fatal(err)
		}
		part, err := svc.Ingest(t.Context(), &IngestRequest{
			Content:    bytes.NewReader(raw),
			SessionID:  "sess-1",
			Provenance: Provenance{Source: "terminal", ExternalID: "x", TurnID: "turn-1"},
		})
		if err != nil {
			t.Fatal(err)
		}
		*dest = part
		_ = i
	}
	first.Provenance.IngestedAt = ""
	second.Provenance.IngestedAt = ""
	first.ArtifactID = ""
	second.ArtifactID = ""
	if first != second {
		t.Errorf("ingest is not deterministic:\n%+v\n%+v", first, second)
	}
}

func TestMetadataStripped(t *testing.T) {
	svc, err := NewService(ptrLimits(), WithStore(newMemoryStore()))
	if err != nil {
		t.Fatal(err)
	}
	part, err := svc.Ingest(t.Context(), &IngestRequest{
		Content:    bytes.NewReader(pngBytes(t, 8, 8)),
		SessionID:  "sess-1",
		Provenance: Provenance{Source: "terminal", TurnID: "turn-1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(part)
	if err != nil {
		t.Fatal(err)
	}
	for _, leak := range []string{"GPS", "EXIF", "XMP", "iVBOR", "/9j/"} {
		if strings.Contains(string(raw), leak) {
			t.Errorf("part metadata leaks %q", leak)
		}
	}
}

func TestPartValidation(t *testing.T) {
	part := ImagePart{
		Kind:             PartKindImageRef,
		ArtifactID:       "art-1",
		SourceDigest:     "sha256:abc",
		DerivedDigest:    "sha256:def",
		MIME:             MIMEPNG,
		Width:            4,
		Height:           4,
		EncodedBytes:     100,
		TransformVersion: "v1",
		Transform:        TransformGeometry{Width: 4, Height: 4, Kernel: "nearest"},
		Provenance:       Provenance{Source: "terminal", TurnID: "turn-1"},
		Trust:            TrustUntrustedExternal,
	}
	if err := ValidatePart(&part); err != nil {
		t.Fatalf("ValidatePart: %v", err)
	}
	bad := part
	bad.Kind = "image_ref.v2"
	if err := ValidatePart(&bad); err == nil {
		t.Error("expected kind rejection")
	}
	bad = part
	bad.MIME = "image/heic"
	if err := ValidatePart(&bad); err == nil {
		t.Error("expected mime rejection")
	}
	bad = part
	bad.Transform.Upscaled = true
	if err := ValidatePart(&bad); err == nil {
		t.Error("expected upscale rejection")
	}
	if _, err := UnmarshalPart([]byte(`{"kind":"image_ref.v1","unknown_field":1}`)); err == nil {
		t.Error("expected unknown field rejection")
	}
	if _, err := UnmarshalPart([]byte(`{"kind":"image_ref.v1"}`)); err == nil {
		t.Error("expected incomplete rejection")
	}
}

func TestAltTextBounds(t *testing.T) {
	svc, err := NewService(ptrLimits(), WithStore(newMemoryStore()))
	if err != nil {
		t.Fatal(err)
	}
	_, err = svc.Ingest(t.Context(), &IngestRequest{
		Content:    bytes.NewReader(pngBytes(t, 4, 4)),
		AltText:    strings.Repeat("a", 513),
		SessionID:  "sess-1",
		Provenance: Provenance{Source: "terminal", TurnID: "turn-1"},
	})
	if err == nil {
		t.Error("expected alt text rejection")
	}
}

func TestNilGuards(t *testing.T) {
	svc, err := NewService(ptrLimits(), WithStore(newMemoryStore()))
	if err != nil {
		t.Fatal(err)
	}
	var nilCtx context.Context
	if _, err := svc.Ingest(nilCtx, &IngestRequest{Content: bytes.NewReader([]byte("x")), SessionID: "s"}); err == nil {
		t.Error("expected nil context rejection")
	}
	if _, err := svc.Ingest(t.Context(), nil); err == nil {
		t.Error("expected nil request rejection")
	}
	if _, err := svc.Ingest(t.Context(), &IngestRequest{SessionID: "s"}); err == nil {
		t.Error("expected nil content rejection")
	}
	if _, err := NewService(ptrLimits()); err != nil {
		t.Fatalf("store is optional at construction: %v", err)
	}
	if _, err := NewService((&Limits{})); err == nil {
		t.Error("expected limits rejection")
	}
}

func TestDecodeConcurrencyBound(t *testing.T) {
	limits := testLimits()
	limits.MaxDecodeConcurrency = 1
	svc, err := NewService(&limits, WithStore(newMemoryStore()))
	if err != nil {
		t.Fatal(err)
	}
	svc.sem <- struct{}{}
	defer func() { <-svc.sem }()
	_, err = svc.Ingest(t.Context(), &IngestRequest{
		Content:    bytes.NewReader(pngBytes(t, 4, 4)),
		SessionID:  "sess-1",
		Provenance: Provenance{Source: "terminal", TurnID: "turn-1"},
	})
	if code, ok := CodeOf(err); !ok || code != ErrorCodeVisionLimitExceeded {
		t.Errorf("code = %v (%v), want vision_limit_exceeded", code, err)
	}
}

func TestObserverRedaction(t *testing.T) {
	var got *Observation
	svc, err := NewService(ptrLimits(), WithStore(newMemoryStore()), WithObserver(func(_ context.Context, o *Observation) { got = o }))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Ingest(t.Context(), &IngestRequest{
		Content:    bytes.NewReader(pngBytes(t, 8, 8)),
		SessionID:  "sess-1",
		Provenance: Provenance{Source: "terminal", TurnID: "turn-1"},
	}); err != nil {
		t.Fatal(err)
	}
	if got == nil || got.Result != "ok" || got.Operation != "ingest" {
		t.Fatalf("observation = %+v", got)
	}
	if got.Width != 0 || got.Height != 0 {
		t.Errorf("observation carries raw dimensions: %+v", got)
	}
}
