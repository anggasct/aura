package vision

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
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
		return ArtifactRef{}, errNilArgument("context")
	}
	if meta == nil {
		return ArtifactRef{}, errNilArgument("artifact metadata")
	}
	if meta.ID == "" {
		return ArtifactRef{}, errNilArgument("artifact id")
	}
	raw, err := io.ReadAll(r)
	if err != nil {
		return ArtifactRef{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	sum := sha256.Sum256(raw)
	m.refs[meta.ID] = raw
	m.puts++
	return ArtifactRef{ID: meta.ID, BlobDigest: hex.EncodeToString(sum[:]), SizeBytes: int64(len(raw))}, nil
}

func (m *memoryStore) Unlink(ctx context.Context, refID string) error {
	if ctx == nil {
		return errNilArgument("context")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.refs, refID)
	return nil
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
		Provenance:   Provenance{Source: "terminal", ExternalID: "line:1", TurnID: "turn-1"},
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
		Provenance:   Provenance{Source: "terminal", ExternalID: "line:1", TurnID: "turn-1"},
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
				Provenance:   Provenance{Source: "terminal", ExternalID: "line:1", TurnID: "turn-1"},
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
		Provenance: Provenance{Source: "terminal", ExternalID: "line:1", TurnID: "turn-1"},
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
	store := &recordingStore{}
	svc, err := NewService(ptrLimits(), WithStore(store))
	if err != nil {
		t.Fatal(err)
	}
	part, err := svc.Ingest(t.Context(), &IngestRequest{
		Content:    bytes.NewReader(pngBytes(t, 8, 8)),
		SessionID:  "sess-1",
		Provenance: Provenance{Source: "terminal", ExternalID: "line:1", TurnID: "turn-1"},
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
	if len(store.blobs) != 2 {
		t.Fatalf("blobs = %d, want 2", len(store.blobs))
	}
	derived := store.blobs[1]
	for _, leak := range [][]byte{[]byte("Exif"), []byte("GPS"), []byte("XMP"), []byte("iCCP")} {
		if bytes.Contains(derived, leak) {
			t.Errorf("derived bytes retain %q", leak)
		}
	}
	if _, err := png.Decode(bytes.NewReader(derived)); err != nil {
		t.Errorf("derived is not re-encoded PNG: %v", err)
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
		Transform:        TransformGeometry{Width: 4, Height: 4, Kernel: "lanczos3"},
		Provenance:       Provenance{Source: "terminal", ExternalID: "line:1", SessionID: "sess-1", TurnID: "turn-1", IngestedAt: "2026-01-01T00:00:00Z"},
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
	for _, field := range []struct {
		name   string
		mutate func(*ImagePart)
	}{
		{"empty source", func(p *ImagePart) { p.Provenance.Source = "" }},
		{"empty external_id", func(p *ImagePart) { p.Provenance.ExternalID = "" }},
		{"whitespace external_id", func(p *ImagePart) { p.Provenance.ExternalID = "  " }},
		{"empty session", func(p *ImagePart) { p.Provenance.SessionID = "" }},
		{"empty turn", func(p *ImagePart) { p.Provenance.TurnID = "" }},
		{"empty ingested_at", func(p *ImagePart) { p.Provenance.IngestedAt = "" }},
	} {
		bad = part
		field.mutate(&bad)
		if err := ValidatePart(&bad); err == nil {
			t.Errorf("expected %s rejection", field.name)
		} else if code, ok := CodeOf(err); !ok || code != ErrorCodeInvalidArgument {
			t.Errorf("%s code = %v (%v), want invalid_argument", field.name, code, err)
		}
		raw, mErr := MarshalPart(&part)
		if mErr != nil {
			t.Fatalf("MarshalPart: %v", mErr)
		}
		var decoded ImagePart
		if err := json.Unmarshal(raw, &decoded); err != nil {
			t.Fatalf("json.Unmarshal: %v", err)
		}
		field.mutate(&decoded)
		mutated, mErr := json.Marshal(decoded)
		if mErr != nil {
			t.Fatalf("json.Marshal: %v", mErr)
		}
		if _, err := UnmarshalPart(mutated); err == nil {
			t.Errorf("expected UnmarshalPart %s rejection", field.name)
		}
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
		Provenance: Provenance{Source: "terminal", ExternalID: "line:1", TurnID: "turn-1"},
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
		Provenance: Provenance{Source: "terminal", ExternalID: "line:1", TurnID: "turn-1"},
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
		Provenance: Provenance{Source: "terminal", ExternalID: "line:1", TurnID: "turn-1"},
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

func webpLossyBytes(t *testing.T) []byte {
	t.Helper()
	raw, err := base64.StdEncoding.DecodeString("UklGRiIAAABXRUJQVlA4IBYAAAAwAQCdASoBAAEADsD+JaQAA3AAAAAA")
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func webpLosslessBytes(t *testing.T) []byte {
	t.Helper()
	raw, err := base64.StdEncoding.DecodeString("UklGRrIBAABXRUJQVlA4TKUBAAAvSsAYAA8w//M///MfeJAkbXvaSG7m8Q3GfYSBJekwQztm/IcZlgwnmWImn2BK7aFmBtnVir6q//8VOkFE/xm4baTIu8c48ArEo6+B3zFKYln3pqClSCKX0begFTAXFOLXHSyF8cCNcZEG4OywuA4KVVfJCiArU7GAgJI8+lJP/OKMT/fBAjevg1cYB7YVkFuWga2lyPi5I0HFy5YTpWIHg0RZpkniRVW9odHAKOwosWuOGdxIyn2OvaCDvhg/we6TwadPBPbqBV58MsLmMJ8yZnOWk8SRz4N+QoyPL+MnamzMvcE1rHNEr91F9GKZPVUcS9w7PhhH36suB9qPeYb/oLk6cuTiJ0wOK3m5h1cKjW6EVZCYMK7dxcKCBdgP9HkKr9gkAO2P8GKZGWVdIAatQa+1IDpt6qyorVwdy01xdW8Jkfk6xjEXmVQQ+HQdFr6OKhIN34dXWq0+0qr6EJSCeeVLH9+gvGTLyqM65PQ44ihzlTXxQKjKbAvshXgir7Lil9w4L2bvMycmjQcqXaMCO6BlY28i+FOLzbfI1vEqxAhotocAAA==")
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

type recordingStore struct {
	mu    sync.Mutex
	blobs [][]byte
	metas []*ArtifactMetadata
}

func (m *recordingStore) Put(_ context.Context, r io.Reader, meta *ArtifactMetadata) (ArtifactRef, error) {
	raw, err := io.ReadAll(r)
	if err != nil {
		return ArtifactRef{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	id := meta.ID
	if id == "" {
		id = "art-" + string(rune('0'+len(m.blobs)))
	}
	sum := sha256.Sum256(raw)
	m.blobs = append(m.blobs, raw)
	m.metas = append(m.metas, meta)
	return ArtifactRef{ID: id, BlobDigest: hex.EncodeToString(sum[:]), SizeBytes: int64(len(raw))}, nil
}

func (m *recordingStore) Unlink(_ context.Context, refID string) error {
	return nil
}

func TestIngestWebPFormats(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  func(*testing.T) []byte
	}{
		{"lossy", webpLossyBytes},
		{"lossless", webpLosslessBytes},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := tc.raw(t)
			mime, ok := sniffFormat(raw[:clampPrefix(len(raw), sniffPrefixLen)])
			if !ok || mime != MIMEWebP {
				t.Fatalf("sniff = %q, %v", mime, ok)
			}
			sniffed, err := sniffDimensions(mime, raw)
			if err != nil {
				t.Fatalf("sniffDimensions: %v", err)
			}
			store := &recordingStore{}
			svc, err := NewService(ptrLimits(), WithStore(store))
			if err != nil {
				t.Fatal(err)
			}
			part, err := svc.Ingest(t.Context(), &IngestRequest{
				Content:    bytes.NewReader(raw),
				SessionID:  "sess-1",
				Provenance: Provenance{Source: "terminal", ExternalID: "line:1", TurnID: "turn-1"},
			})
			if err != nil {
				t.Fatalf("Ingest: %v", err)
			}
			if part.MIME != MIMEWebP {
				t.Errorf("mime = %q", part.MIME)
			}
			if part.Width != sniffed.width || part.Height != sniffed.height {
				t.Errorf("part dims = %dx%d, sniffed = %dx%d", part.Width, part.Height, sniffed.width, sniffed.height)
			}
			if part.Width <= 0 || part.Height <= 0 {
				t.Errorf("dims = %dx%d", part.Width, part.Height)
			}
			if part.Transform.Kernel != "lanczos3" {
				t.Errorf("kernel = %q", part.Transform.Kernel)
			}
			if len(store.blobs) != 2 {
				t.Fatalf("blobs = %d, want 2", len(store.blobs))
			}
			if store.metas[1].MediaType != MIMEPNG {
				t.Errorf("derived media type = %q, want image/png", store.metas[1].MediaType)
			}
			derived := store.blobs[1]
			if _, err := png.Decode(bytes.NewReader(derived)); err != nil {
				t.Errorf("derived is not PNG: %v", err)
			}
		})
	}
}

func TestSniffVsDecodeWebP(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  func(*testing.T) []byte
	}{
		{"lossy", webpLossyBytes},
		{"lossless", webpLosslessBytes},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := tc.raw(t)
			geometry, err := sniffDimensions(MIMEWebP, raw)
			if err != nil {
				t.Fatalf("sniffDimensions: %v", err)
			}
			img, err := decodeWebP(raw)
			if err != nil {
				t.Fatalf("decodeWebP: %v", err)
			}
			if got := img.Bounds().Dx(); got != geometry.width {
				t.Errorf("width sniffed %d, decoded %d", geometry.width, got)
			}
			if got := img.Bounds().Dy(); got != geometry.height {
				t.Errorf("height sniffed %d, decoded %d", geometry.height, got)
			}
		})
	}
}

func TestWebPOverDimensionRejectedPreDecode(t *testing.T) {
	raw := webpLossyBytes(t)
	oversized := bytes.Clone(raw)
	binary.LittleEndian.PutUint16(oversized[26:28], 9000)
	binary.LittleEndian.PutUint16(oversized[28:30], 9000)
	geometry, err := sniffDimensions(MIMEWebP, oversized)
	if err != nil {
		t.Fatalf("sniffDimensions: %v", err)
	}
	if geometry.width != 9000 || geometry.height != 9000 {
		t.Fatalf("patched geometry = %dx%d, want 9000x9000", geometry.width, geometry.height)
	}
	store := &recordingStore{}
	limits := testLimits()
	limits.MaxDimension = 8192
	svc, err := NewService(&limits, WithStore(store))
	if err != nil {
		t.Fatal(err)
	}
	_, err = svc.Ingest(t.Context(), &IngestRequest{
		Content:    bytes.NewReader(oversized),
		SessionID:  "sess-1",
		Provenance: Provenance{Source: "terminal", ExternalID: "line:1", TurnID: "turn-1"},
	})
	if code, ok := CodeOf(err); !ok || code != ErrorCodeVisionLimitExceeded {
		t.Fatalf("code = %v (%v), want vision_limit_exceeded", code, err)
	}
	if len(store.blobs) != 0 {
		t.Errorf("over-dimension input wrote %d artifacts", len(store.blobs))
	}
}

func TestDecodeDeadlineHoldsSlot(t *testing.T) {
	limits := testLimits()
	limits.MaxDecodeConcurrency = 1
	limits.DecodeTimeout = time.Nanosecond
	svc, err := NewService(&limits, WithStore(&recordingStore{}))
	if err != nil {
		t.Fatal(err)
	}
	_, err = svc.Ingest(t.Context(), &IngestRequest{
		Content:    bytes.NewReader(pngBytes(t, 8, 8)),
		SessionID:  "sess-1",
		Provenance: Provenance{Source: "terminal", ExternalID: "line:1", TurnID: "turn-1"},
	})
	if code, ok := CodeOf(err); !ok || (code != ErrorCodeVisionDecodeFailed && code != ErrorCodeVisionLimitExceeded) {
		t.Fatalf("first ingest code = %v (%v), want decode failure or limit", code, err)
	}
	time.Sleep(200 * time.Millisecond)
	if got := len(svc.sem); got != 0 {
		t.Fatalf("semaphore not drained after abandoned decode, len = %d", got)
	}
	store := &recordingStore{}
	limits2 := testLimits()
	limits2.MaxDecodeConcurrency = 1
	svc2, err := NewService(&limits2, WithStore(store))
	if err != nil {
		t.Fatal(err)
	}
	svc2.sem <- struct{}{}
	_, err = svc2.Ingest(context.Background(), &IngestRequest{
		Content:    bytes.NewReader(pngBytes(t, 4, 4)),
		SessionID:  "sess-1",
		Provenance: Provenance{Source: "terminal", ExternalID: "line:1", TurnID: "turn-1"},
	})
	if code, ok := CodeOf(err); !ok || code != ErrorCodeVisionLimitExceeded {
		t.Fatalf("concurrency code = %v (%v), want vision_limit_exceeded", code, err)
	}
	<-svc2.sem
	if _, err := svc2.Ingest(context.Background(), &IngestRequest{
		Content:    bytes.NewReader(pngBytes(t, 4, 4)),
		SessionID:  "sess-1",
		Provenance: Provenance{Source: "terminal", ExternalID: "line:1", TurnID: "turn-1"},
	}); err != nil {
		t.Fatalf("ingest after drain: %v", err)
	}
}

func TestDecodeCancelHoldsSlot(t *testing.T) {
	limits := testLimits()
	limits.MaxDecodeConcurrency = 1
	svc, err := NewService(&limits, WithStore(&recordingStore{}))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = svc.Ingest(ctx, &IngestRequest{
		Content:    bytes.NewReader(pngBytes(t, 8, 8)),
		SessionID:  "sess-1",
		Provenance: Provenance{Source: "terminal", ExternalID: "line:1", TurnID: "turn-1"},
	})
	if code, ok := CodeOf(err); ok && code != ErrorCodeVisionDecodeFailed {
		t.Fatalf("code = %v (%v), want vision_decode_failed or success", code, err)
	}
	time.Sleep(200 * time.Millisecond)
	if got := len(svc.sem); got != 0 {
		t.Fatalf("semaphore not drained after cancel, len = %d", got)
	}
}

func exifJPEGBytes(t *testing.T, width, height, orientation int) []byte {
	t.Helper()
	base := jpegBytes(t, width, height)
	if len(base) < 2 || base[0] != 0xFF || base[1] != 0xD8 {
		t.Fatal("base jpeg missing SOI")
	}
	exif := []byte{'E', 'x', 'i', 'f', 0, 0, 'I', 'I', 42, 0, 8, 0, 0, 0, 1, 0, 0x12, 0x01, 3, 0, 1, 0, 0, 0}
	orient := make([]byte, 2)
	binary.LittleEndian.PutUint16(orient, uint16(orientation))
	exif = append(exif, orient...)
	exif = append(exif, []byte{0, 0, 0, 0, 0, 0}...)
	segmentLen := len(exif) + 2
	segment := []byte{0xFF, 0xE1, byte(segmentLen >> 8), byte(segmentLen)}
	segment = append(segment, exif...)
	out := append([]byte{0xFF, 0xD8}, segment...)
	out = append(out, base[2:]...)
	return out
}

func TestEXIFOrientationAppliedAndStripped(t *testing.T) {
	raw := exifJPEGBytes(t, 10, 8, 6)
	if got := orientationOf(MIMEJPEG, raw); got != 6 {
		t.Fatalf("orientation = %d, want 6", got)
	}
	store := &recordingStore{}
	svc, err := NewService(ptrLimits(), WithStore(store))
	if err != nil {
		t.Fatal(err)
	}
	part, err := svc.Ingest(t.Context(), &IngestRequest{
		Content:    bytes.NewReader(raw),
		SessionID:  "sess-1",
		Provenance: Provenance{Source: "terminal", ExternalID: "line:1", TurnID: "turn-1"},
	})
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if part.Width != 8 || part.Height != 10 {
		t.Errorf("oriented dims = %dx%d, want 8x10", part.Width, part.Height)
	}
	if len(store.blobs) != 2 {
		t.Fatalf("blobs = %d, want 2", len(store.blobs))
	}
	derived := store.blobs[1]
	if bytes.Contains(derived, []byte("Exif")) || bytes.Contains(derived, []byte("GPS")) {
		t.Error("derived bytes retain EXIF/GPS segments")
	}
	if _, err := jpeg.Decode(bytes.NewReader(derived)); err != nil {
		t.Errorf("derived is not JPEG: %v", err)
	}
	img, err := jpeg.Decode(bytes.NewReader(derived))
	if err != nil {
		t.Fatal(err)
	}
	if img.Bounds().Dx() != 8 || img.Bounds().Dy() != 10 {
		t.Errorf("derived pixels = %dx%d, want 8x10", img.Bounds().Dx(), img.Bounds().Dy())
	}
}

func TestOrientationUnit(t *testing.T) {
	img := image.NewNRGBA(image.Rect(0, 0, 4, 2))
	for y := range 2 {
		for x := range 4 {
			img.Set(x, y, color.NRGBA{R: uint8(x * 10), G: uint8(y * 10), B: 1, A: 255})
		}
	}
	for _, orientation := range []int{1, 2, 3, 4, 5, 6, 7, 8} {
		got := applyOrientation(img, orientation, true)
		wantW, wantH := 4, 2
		if orientation >= 5 {
			wantW, wantH = 2, 4
		}
		if got.Bounds().Dx() != wantW || got.Bounds().Dy() != wantH {
			t.Errorf("orientation %d dims = %dx%d, want %dx%d", orientation, got.Bounds().Dx(), got.Bounds().Dy(), wantW, wantH)
		}
		if got := applyOrientation(img, orientation, false); got.Bounds() != img.Bounds() {
			t.Errorf("orientation %d disabled should not resize", orientation)
		}
	}
	le := exifJPEGBytes(t, 10, 8, 3)
	if got := orientationOf(MIMEJPEG, le); got != 3 {
		t.Errorf("LE orientation = %d, want 3", got)
	}
	if got := orientationOf(MIMEPNG, le); got != 1 {
		t.Errorf("non-jpeg orientation = %d, want 1", got)
	}
	if got := orientationOf(MIMEJPEG, jpegBytes(t, 10, 8)); got != 1 {
		t.Errorf("plain jpeg orientation = %d, want 1", got)
	}
}

func TestPixelBombRejectedPreDecode(t *testing.T) {
	raw := pngBytes(t, 8, 8)
	bomb := bytes.Clone(raw)
	binary.BigEndian.PutUint32(bomb[16:20], 8000)
	binary.BigEndian.PutUint32(bomb[20:24], 8000)
	store := &recordingStore{}
	limits := testLimits()
	limits.MaxDimension = 8192
	limits.MaxPixels = 40000000
	svc, err := NewService(&limits, WithStore(store))
	if err != nil {
		t.Fatal(err)
	}
	_, err = svc.Ingest(t.Context(), &IngestRequest{
		Content:    bytes.NewReader(bomb),
		SessionID:  "sess-1",
		Provenance: Provenance{Source: "terminal", ExternalID: "line:1", TurnID: "turn-1"},
	})
	if code, ok := CodeOf(err); !ok || code != ErrorCodeVisionLimitExceeded {
		t.Fatalf("code = %v (%v), want vision_limit_exceeded", code, err)
	}
	if len(store.blobs) != 0 {
		t.Errorf("bomb wrote %d artifacts", len(store.blobs))
	}
}

func TestBadColorModelRejected(t *testing.T) {
	raw := pngBytes(t, 8, 8)
	bad := bytes.Clone(raw)
	bad[25] = 7
	store := &recordingStore{}
	svc, err := NewService(ptrLimits(), WithStore(store))
	if err != nil {
		t.Fatal(err)
	}
	_, err = svc.Ingest(t.Context(), &IngestRequest{
		Content:    bytes.NewReader(bad),
		SessionID:  "sess-1",
		Provenance: Provenance{Source: "terminal", ExternalID: "line:1", TurnID: "turn-1"},
	})
	if code, ok := CodeOf(err); !ok || code != ErrorCodeVisionDecodeFailed {
		t.Fatalf("code = %v (%v), want vision_decode_failed", code, err)
	}
	if len(store.blobs) != 0 {
		t.Errorf("bad color wrote %d artifacts", len(store.blobs))
	}
}

func TestPolyglotRejected(t *testing.T) {
	raw := pngBytes(t, 8, 8)
	polyglot := append(bytes.Clone(raw[:8]), []byte("<html><script>alert(1)</script>")...)
	store := &recordingStore{}
	svc, err := NewService(ptrLimits(), WithStore(store))
	if err != nil {
		t.Fatal(err)
	}
	_, err = svc.Ingest(t.Context(), &IngestRequest{
		Content:    bytes.NewReader(polyglot),
		SessionID:  "sess-1",
		Provenance: Provenance{Source: "terminal", ExternalID: "line:1", TurnID: "turn-1"},
	})
	if err == nil {
		t.Fatal("expected polyglot rejection")
	}
	if len(store.blobs) != 0 {
		t.Errorf("polyglot wrote %d artifacts", len(store.blobs))
	}
	gifPolyglot := append([]byte("GIF89a"), []byte("<script>")...)
	_, err = svc.Ingest(t.Context(), &IngestRequest{
		Content:    bytes.NewReader(gifPolyglot),
		SessionID:  "sess-1",
		Provenance: Provenance{Source: "terminal", ExternalID: "line:1", TurnID: "turn-1"},
	})
	if err == nil {
		t.Fatal("expected gif polyglot rejection")
	}
}

type quotaStore struct {
	recordingStore
	failOn int
	calls  int
}

type causeStore struct {
	recordingStore
	cause error
}

func (m *causeStore) Put(_ context.Context, _ io.Reader, _ *ArtifactMetadata) (ArtifactRef, error) {
	return ArtifactRef{}, m.cause
}

func (m *quotaStore) Put(ctx context.Context, r io.Reader, meta *ArtifactMetadata) (ArtifactRef, error) {
	m.calls++
	if m.calls == m.failOn {
		return ArtifactRef{}, Errorf(ErrorCodeArtifactQuotaExceeded, "artifact_quota_exceeded: backend quota is exhausted")
	}
	return m.recordingStore.Put(ctx, r, meta)
}

func TestQuotaThroughStore(t *testing.T) {
	for _, failOn := range []int{1, 2} {
		store := &quotaStore{failOn: failOn}
		svc, err := NewService(ptrLimits(), WithStore(store))
		if err != nil {
			t.Fatal(err)
		}
		_, err = svc.Ingest(t.Context(), &IngestRequest{
			Content:    bytes.NewReader(pngBytes(t, 8, 8)),
			SessionID:  "sess-1",
			Provenance: Provenance{Source: "terminal", ExternalID: "line:1", TurnID: "turn-1"},
		})
		if code, ok := CodeOf(err); !ok || code != ErrorCodeArtifactQuotaExceeded {
			t.Errorf("failOn %d code = %v (%v), want artifact_quota_exceeded", failOn, code, err)
		}
		if err != nil && !strings.Contains(err.Error(), "artifact_quota_exceeded") {
			t.Errorf("failOn %d error does not preserve quota cause: %v", failOn, err)
		}
	}
}

func TestQuotaExhaustionPreservesCauseAndLeavesNoPartialLink(t *testing.T) {
	sentinel := io.ErrUnexpectedEOF
	stores := &causeStore{cause: wrapWithCode(ErrorCodeArtifactQuotaExceeded, "backend quota is exhausted", sentinel)}
	svc, err := NewService(ptrLimits(), WithStore(stores))
	if err != nil {
		t.Fatal(err)
	}
	_, err = svc.Ingest(t.Context(), &IngestRequest{
		Content:    bytes.NewReader(pngBytes(t, 8, 8)),
		SessionID:  "sess-1",
		Provenance: Provenance{Source: "terminal", ExternalID: "line:1", TurnID: "turn-1"},
	})
	if code, ok := CodeOf(err); !ok || code != ErrorCodeArtifactQuotaExceeded {
		t.Fatalf("code = %v (%v), want artifact_quota_exceeded", code, err)
	}
	if !strings.Contains(err.Error(), "artifact_quota_exceeded") {
		t.Fatalf("quota cause missing from error chain: %v", err)
	}
	if !errors.Is(err, sentinel) {
		t.Fatalf("quota cause is not reachable in chain: %v", err)
	}
	if len(stores.blobs) != 0 {
		t.Fatalf("blobs = %d, want 0 (no partial link on quota failure)", len(stores.blobs))
	}
}

func TestNonQuotaWriteFailureKeepsUnavailableWithCause(t *testing.T) {
	sentinel := io.ErrUnexpectedEOF
	stores := &causeStore{cause: sentinel}
	svc, err := NewService(ptrLimits(), WithStore(stores))
	if err != nil {
		t.Fatal(err)
	}
	_, err = svc.Ingest(t.Context(), &IngestRequest{
		Content:    bytes.NewReader(pngBytes(t, 8, 8)),
		SessionID:  "sess-1",
		Provenance: Provenance{Source: "terminal", ExternalID: "line:1", TurnID: "turn-1"},
	})
	if code, ok := CodeOf(err); !ok || code != ErrorCodeVisionArtifactUnavailable {
		t.Fatalf("code = %v (%v), want vision_artifact_unavailable", code, err)
	}
	if !errors.Is(err, sentinel) {
		t.Fatalf("store cause is not reachable in chain: %v", err)
	}
}

func TestDerivedBytesEXIFFree(t *testing.T) {
	store := &recordingStore{}
	svc, err := NewService(ptrLimits(), WithStore(store))
	if err != nil {
		t.Fatal(err)
	}
	raw := exifJPEGBytes(t, 12, 10, 8)
	part, err := svc.Ingest(t.Context(), &IngestRequest{
		Content:    bytes.NewReader(raw),
		SessionID:  "sess-1",
		Provenance: Provenance{Source: "terminal", ExternalID: "line:1", TurnID: "turn-1"},
	})
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if len(store.blobs) != 2 {
		t.Fatalf("blobs = %d", len(store.blobs))
	}
	for i, blob := range store.blobs {
		if i == 1 && (bytes.Contains(blob, []byte("Exif")) || bytes.Contains(blob, []byte("GPS")) || bytes.Contains(blob, []byte("XMP"))) {
			t.Errorf("derived blob %d retains metadata", i)
		}
	}
	_ = part
}
