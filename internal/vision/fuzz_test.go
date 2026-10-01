package vision

import (
	"bytes"
	"testing"
)

func FuzzSniffFormat(f *testing.F) {
	seeds := [][]byte{
		{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A},
		{0xFF, 0xD8, 0xFF, 0xE0},
		[]byte("RIFF\x00\x00\x00\x00WEBPVP8 "),
		[]byte("GIF89a"),
		[]byte("BM"),
		[]byte("II*\x00"),
		[]byte("not an image at all"),
		{},
	}
	for _, seed := range seeds {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, raw []byte) {
		if len(raw) > 4096 {
			t.Skip()
		}
		mime, ok := sniffFormat(raw)
		if !ok {
			return
		}
		switch mime {
		case MIMEPNG, MIMEJPEG, MIMEWebP, MIMEgif:
		default:
			t.Fatalf("sniff returned unknown mime %q", mime)
		}
		_, err := sniffDimensions(mime, raw)
		if err != nil {
			if _, ok := CodeOf(err); !ok {
				t.Fatalf("untyped sniff error: %v", err)
			}
		}
	})
}

func FuzzPartRoundTrip(f *testing.F) {
	part := ImagePart{
		Kind:             PartKindImageRef,
		ArtifactID:       "art-seed",
		SourceDigest:     "sha256:abc",
		DerivedDigest:    "sha256:def",
		MIME:             MIMEPNG,
		Width:            8,
		Height:           8,
		EncodedBytes:     128,
		TransformVersion: "v1",
		Transform:        TransformGeometry{Width: 8, Height: 8, Kernel: "lanczos3"},
		Provenance:       Provenance{Source: "terminal", SessionID: "sess-1", TurnID: "turn-1", IngestedAt: "2026-01-01T00:00:00Z"},
		Trust:            TrustUntrustedExternal,
	}
	raw, err := MarshalPart(&part)
	if err != nil {
		f.Fatal(err)
	}
	f.Add(raw)
	f.Fuzz(func(t *testing.T, input []byte) {
		if len(input) > 4096 {
			t.Skip()
		}
		decoded, err := UnmarshalPart(input)
		if err != nil {
			if _, ok := CodeOf(err); !ok {
				t.Fatalf("untyped part error: %v", err)
			}
			return
		}
		roundTrip, err := MarshalPart(&decoded)
		if err != nil {
			t.Fatalf("MarshalPart: %v", err)
		}
		again, err := UnmarshalPart(roundTrip)
		if err != nil {
			t.Fatalf("UnmarshalPart: %v", err)
		}
		if again != decoded {
			t.Fatal("part round trip is not stable")
		}
		if bytes.Contains(roundTrip, []byte("iVBOR")) {
			t.Fatal("part embeds pixels")
		}
	})
}
