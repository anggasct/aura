package knowledge

import (
	"fmt"
	"math"
	"slices"
	"testing"
)

func chunkHits(ids ...string) []Hit {
	hits := make([]Hit, len(ids))
	for i, id := range ids {
		hits[i] = Hit{ChunkID: id, Collection: "support"}
	}
	return hits
}

func reciprocal(rank int) float64 { return 1 / float64(60+rank) }

func TestFuseAppliesReciprocalRankFusionWithTiesByChunkID(t *testing.T) {
	tests := map[string]struct {
		lexical  []string
		semantic []string
		want     []string
		scores   []float64
	}{
		"agreeing lists keep their order": {
			lexical: []string{"a", "b"}, semantic: []string{"a", "b"},
			want:   []string{"a", "b"},
			scores: []float64{reciprocal(1) + reciprocal(1), reciprocal(2) + reciprocal(2)},
		},
		"disjoint lists interleave and tie by chunk id": {
			lexical: []string{"d", "b"}, semantic: []string{"c", "a"},
			want:   []string{"c", "d", "a", "b"},
			scores: []float64{reciprocal(1), reciprocal(1), reciprocal(2), reciprocal(2)},
		},
		"overlap lifts a chunk both lists found": {
			lexical: []string{"a", "b", "c"}, semantic: []string{"c", "a"},
			want:   []string{"a", "c", "b"},
			scores: []float64{reciprocal(1) + reciprocal(2), reciprocal(3) + reciprocal(1), reciprocal(2)},
		},
		"a semantic only list": {
			lexical: nil, semantic: []string{"x", "y"},
			want:   []string{"x", "y"},
			scores: []float64{reciprocal(1), reciprocal(2)},
		},
		"a lexical only list": {
			lexical: []string{"x"}, semantic: nil,
			want:   []string{"x"},
			scores: []float64{reciprocal(1)},
		},
		"nothing": {},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			got := Fuse(chunkHits(test.lexical...), chunkHits(test.semantic...))
			ids := make([]string, len(got))
			scores := make([]float64, len(got))
			for i := range got {
				ids[i], scores[i] = got[i].ChunkID, got[i].Score
			}
			if len(ids) != len(test.want) || (len(ids) > 0 && !slices.Equal(ids, test.want)) {
				t.Fatalf("order = %v, want %v", ids, test.want)
			}
			for i := range scores {
				if math.Abs(scores[i]-test.scores[i]) > 1e-12 {
					t.Errorf("score[%d] = %v, want %v", i, scores[i], test.scores[i])
				}
			}
		})
	}
}

func TestFuseTakesOnlyTheTopFiftyOfEachList(t *testing.T) {
	var lexical, semantic []string
	for i := range 80 {
		lexical = append(lexical, fmt.Sprintf("l%03d", i))
		semantic = append(semantic, fmt.Sprintf("s%03d", i))
	}
	got := Fuse(chunkHits(lexical...), chunkHits(semantic...))
	if len(got) != 2*SearchDepth {
		t.Fatalf("fused %d hits, want %d", len(got), 2*SearchDepth)
	}
	for i := range got {
		if got[i].ChunkID == "l050" || got[i].ChunkID == "s050" {
			t.Fatalf("rank 51 reached the fusion: %s", got[i].ChunkID)
		}
	}
}
