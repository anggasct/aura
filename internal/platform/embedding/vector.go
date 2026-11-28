package embedding

import (
	"cmp"
	"encoding/binary"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/anggasct/aura/internal/platform/secret"
)

const (
	VectorValueBytes    = 4
	UnitLengthTolerance = 1e-3
	RRFRankOffset       = 60
	QueryEmbedTimeout   = 400 * time.Millisecond
	MaxEmbedQueryBytes  = 16 << 10
)

func ResolveCredential(ref secret.Ref) (string, bool) {
	if ref.IsZero() {
		return "", true
	}
	value, err := ref.Reference().Resolve()
	value = strings.TrimSpace(value)
	if err != nil || value == "" {
		return "", false
	}
	return value, true
}

func Normalize(vector []float32, dimensions int) ([]float32, bool) {
	if dimensions <= 0 || len(vector) != dimensions {
		return nil, false
	}
	var sum float64
	for _, value := range vector {
		v := float64(value)
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return nil, false
		}
		sum += v * v
	}
	norm := math.Sqrt(sum)
	if norm == 0 || math.IsNaN(norm) || math.IsInf(norm, 0) {
		return nil, false
	}
	unit := make([]float32, len(vector))
	for i, value := range vector {
		scaled := float32(float64(value) / norm)
		if math.IsNaN(float64(scaled)) || math.IsInf(float64(scaled), 0) {
			return nil, false
		}
		unit[i] = scaled
	}
	return unit, true
}

func ValidateUnit(vector []float32) error {
	if len(vector) == 0 {
		return Errorf(ErrorCodeVectorInvalid, "vector must not be empty")
	}
	var sum float64
	for _, value := range vector {
		v := float64(value)
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return Errorf(ErrorCodeVectorInvalid, "vector must be finite")
		}
		sum += v * v
	}
	if math.Abs(math.Sqrt(sum)-1) > UnitLengthTolerance {
		return Errorf(ErrorCodeVectorInvalid, "vector must have unit length")
	}
	return nil
}

func Encode(vector []float32) []byte {
	blob := make([]byte, len(vector)*VectorValueBytes)
	for i, value := range vector {
		binary.LittleEndian.PutUint32(blob[i*VectorValueBytes:], math.Float32bits(value))
	}
	return blob
}

func Decode(blob []byte, dimensions int) ([]float32, error) {
	if dimensions <= 0 || dimensions > math.MaxInt/VectorValueBytes {
		return nil, Errorf(ErrorCodeVectorInvalid, "vector dimensions %d are out of bounds", dimensions)
	}
	want := dimensions * VectorValueBytes
	if len(blob) != want {
		return nil, Errorf(ErrorCodeVectorInvalid, "vector has %d bytes, want %d", len(blob), want)
	}
	vector := make([]float32, dimensions)
	for i := range dimensions {
		value := math.Float32frombits(binary.LittleEndian.Uint32(blob[i*VectorValueBytes:]))
		if math.IsNaN(float64(value)) || math.IsInf(float64(value), 0) {
			return nil, Errorf(ErrorCodeVectorInvalid, "vector holds a value that is not finite")
		}
		vector[i] = value
	}
	return vector, nil
}

func Dot(a, b []float32) float32 {
	var s0, s1, s2, s3 float32
	n := min(len(a), len(b))
	a, b = a[:n], b[:n]
	for len(a) >= 4 && len(b) >= 4 {
		s0 += a[0] * b[0]
		s1 += a[1] * b[1]
		s2 += a[2] * b[2]
		s3 += a[3] * b[3]
		a, b = a[4:], b[4:]
	}
	for i := range a {
		s0 += a[i] * b[i]
	}
	return s0 + s1 + s2 + s3
}

type RRFItem[K comparable] struct {
	Key   K
	Score float64
}

func FuseRRF[K comparable](lexical, semantic []K, depth int, cmpKey func(a, b K) int) []RRFItem[K] {
	if depth <= 0 {
		return nil
	}
	type entry struct {
		key   K
		score float64
		order int
	}
	lex := lexical[:min(len(lexical), depth)]
	sem := semantic[:min(len(semantic), depth)]
	byKey := make(map[K]*entry, len(lex)+len(sem))
	nextOrder := 0
	for _, list := range [][]K{lex, sem} {
		seen := make(map[K]struct{}, len(list))
		for rank, key := range list {
			if _, duplicate := seen[key]; duplicate {
				continue
			}
			seen[key] = struct{}{}
			item := byKey[key]
			if item == nil {
				item = &entry{key: key, order: nextOrder}
				byKey[key] = item
				nextOrder++
			}
			item.score += 1 / float64(RRFRankOffset+rank+1)
		}
	}
	if len(byKey) == 0 {
		return nil
	}
	entries := make([]*entry, 0, len(byKey))
	for _, item := range byKey {
		entries = append(entries, item)
	}
	slices.SortFunc(entries, func(a, b *entry) int {
		if byScore := cmp.Compare(b.score, a.score); byScore != 0 {
			return byScore
		}
		if cmpKey != nil {
			if byKeyOrder := cmpKey(a.key, b.key); byKeyOrder != 0 {
				return byKeyOrder
			}
		}
		return cmp.Compare(a.order, b.order)
	})
	out := make([]RRFItem[K], len(entries))
	for i, item := range entries {
		out[i] = RRFItem[K]{Key: item.key, Score: item.score}
	}
	return out
}
