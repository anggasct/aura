package embedding

import (
	"cmp"
	"encoding/binary"
	"math"
	"slices"
	"testing"

	"github.com/anggasct/aura/internal/platform/fault"
)

func TestEmbeddingVectorPrimitivesAndRRF(t *testing.T) {
	t.Run("Normalize rejects invalid inputs and returns unit copy", func(t *testing.T) {
		raw := []float32{3, 4}
		unit, ok := Normalize(raw, 2)
		if !ok {
			t.Fatal("Normalize([3,4], 2) failed")
		}
		if math.Abs(float64(unit[0])-0.6) > 1e-6 || math.Abs(float64(unit[1])-0.8) > 1e-6 {
			t.Fatalf("unit = %v, want [0.6 0.8]", unit)
		}
		raw[0] = 99
		if math.Abs(float64(unit[0])-0.6) > 1e-6 {
			t.Fatal("Normalize aliased input slice")
		}
		for _, tc := range []struct {
			name string
			vec  []float32
			dims int
		}{
			{"non-positive dimensions", []float32{1}, 0},
			{"negative dimensions", []float32{1}, -2},
			{"length mismatch", []float32{1, 2}, 3},
			{"zero vector", []float32{0, 0, 0}, 3},
			{"NaN element", []float32{float32(math.NaN()), 1}, 2},
			{"positive Inf", []float32{float32(math.Inf(1)), 1}, 2},
			{"negative Inf", []float32{float32(math.Inf(-1)), 1}, 2},
		} {
			if got, ok := Normalize(tc.vec, tc.dims); ok || got != nil {
				t.Errorf("%s: Normalize = (%v, %t), want (nil, false)", tc.name, got, ok)
			}
		}
	})

	t.Run("ValidateUnit enforces unit length and finite values", func(t *testing.T) {
		if err := ValidateUnit([]float32{0.6, 0.8}); err != nil {
			t.Fatalf("ValidateUnit(unit) = %v", err)
		}
		for _, tc := range []struct {
			name string
			vec  []float32
		}{
			{"empty", nil},
			{"zero", []float32{0, 0}},
			{"non-unit", []float32{2, 0}},
			{"NaN", []float32{float32(math.NaN()), 1}},
			{"Inf", []float32{float32(math.Inf(1)), 0}},
		} {
			err := ValidateUnit(tc.vec)
			if !fault.HasCode(err, ErrorCodeVectorInvalid) {
				t.Errorf("%s: ValidateUnit = %v, want %s", tc.name, err, ErrorCodeVectorInvalid)
			}
		}
	})

	t.Run("Encode and Decode round-trip and reject corrupt or overflowing blobs", func(t *testing.T) {
		orig := []float32{0.25, -0.5, 0.75, -0.125, 0.3125}
		blob := Encode(orig)
		decoded, err := Decode(blob, len(orig))
		if err != nil {
			t.Fatalf("Decode(valid) = %v", err)
		}
		if !slices.Equal(decoded, orig) {
			t.Fatalf("decoded = %v, want %v", decoded, orig)
		}

		for _, tc := range []struct {
			name string
			blob []byte
			dims int
		}{
			{"dimensions zero", blob, 0},
			{"dimensions negative", blob, -1},
			{"dimensions overflow", blob, math.MaxInt/VectorValueBytes + 1},
			{"wrong byte length", blob[:len(blob)-1], len(orig)},
		} {
			if _, err := Decode(tc.blob, tc.dims); !fault.HasCode(err, ErrorCodeVectorInvalid) {
				t.Errorf("%s: Decode = %v, want %s", tc.name, err, ErrorCodeVectorInvalid)
			}
		}

		for _, bad := range []float32{float32(math.NaN()), float32(math.Inf(1)), float32(math.Inf(-1))} {
			corrupt := make([]byte, VectorValueBytes)
			binary.LittleEndian.PutUint32(corrupt, math.Float32bits(bad))
			if _, err := Decode(corrupt, 1); !fault.HasCode(err, ErrorCodeVectorInvalid) {
				t.Errorf("Decode(%v) = %v, want %s", bad, err, ErrorCodeVectorInvalid)
			}
		}
	})

	t.Run("Dot computes unrolled inner product safely on mismatched lengths", func(t *testing.T) {
		a := []float32{1, 2, 3, 4, 5}
		b := []float32{2, 3, 4, 5, 6}
		if got := Dot(a, b); math.Abs(float64(got)-70) > 1e-5 {
			t.Fatalf("Dot(a, b) = %v, want 70", got)
		}
		if got := Dot(a, []float32{2, 3}); math.Abs(float64(got)-8) > 1e-5 {
			t.Fatalf("Dot(a, short) = %v, want 8", got)
		}
		if got := Dot([]float32{2, 3}, b); math.Abs(float64(got)-13) > 1e-5 {
			t.Fatalf("Dot(short, b) = %v, want 13", got)
		}
		if got := Dot(nil, b); got != 0 {
			t.Fatalf("Dot(nil, b) = %v, want 0", got)
		}
	})

	t.Run("FuseRRF applies k=60, depth cap, duplicate dedupe, and deterministic tie-breaking", func(t *testing.T) {
		if got := FuseRRF([]string{"a"}, []string{"b"}, 0, cmp.Compare[string]); got != nil {
			t.Fatalf("FuseRRF(depth=0) = %v, want nil", got)
		}
		if got := FuseRRF([]string{"a"}, []string{"b"}, -5, cmp.Compare[string]); got != nil {
			t.Fatalf("FuseRRF(depth=-5) = %v, want nil", got)
		}

		fused := FuseRRF([]string{"a", "b", "c"}, []string{"c", "a"}, 10, cmp.Compare[string])
		wantKeys := []string{"a", "c", "b"}
		wantScores := []float64{1.0/61 + 1.0/62, 1.0/63 + 1.0/61, 1.0 / 62}
		if len(fused) != len(wantKeys) {
			t.Fatalf("len(fused) = %d, want %d", len(fused), len(wantKeys))
		}
		for i := range wantKeys {
			if fused[i].Key != wantKeys[i] || math.Abs(fused[i].Score-wantScores[i]) > 1e-12 {
				t.Errorf("fused[%d] = %+v, want (%s, %v)", i, fused[i], wantKeys[i], wantScores[i])
			}
		}

		deduped := FuseRRF([]string{"a", "a", "b"}, []string{"b", "b"}, 10, cmp.Compare[string])
		if len(deduped) != 2 {
			t.Fatalf("deduped = %+v, want 2 items", deduped)
		}
		if deduped[0].Key != "b" || math.Abs(deduped[0].Score-(1.0/63+1.0/61)) > 1e-12 {
			t.Errorf("deduped[0] = %+v, want b with single contribution per list", deduped[0])
		}
		if deduped[1].Key != "a" || math.Abs(deduped[1].Score-(1.0/61)) > 1e-12 {
			t.Errorf("deduped[1] = %+v, want a with score 1/61 (not double-counted)", deduped[1])
		}

		capped := FuseRRF([]string{"l1", "l2", "l3"}, []string{"s1", "s2", "s3"}, 2, cmp.Compare[string])
		if len(capped) != 4 {
			t.Fatalf("capped len = %d, want 4", len(capped))
		}
		for _, item := range capped {
			if item.Key == "l3" || item.Key == "s3" {
				t.Fatalf("item beyond depth leaked: %s", item.Key)
			}
		}

		tiedCmp := FuseRRF([]string{"z"}, []string{"a"}, 5, cmp.Compare[string])
		if len(tiedCmp) != 2 || tiedCmp[0].Key != "a" || tiedCmp[1].Key != "z" {
			t.Fatalf("tiedCmp = %+v, want [a, z]", tiedCmp)
		}
		tiedFirstSeen := FuseRRF([]string{"z"}, []string{"a"}, 5, nil)
		if len(tiedFirstSeen) != 2 || tiedFirstSeen[0].Key != "z" || tiedFirstSeen[1].Key != "a" {
			t.Fatalf("tiedFirstSeen = %+v, want [z, a]", tiedFirstSeen)
		}
	})
}
