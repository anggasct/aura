package knowledge

import (
	"cmp"
	"slices"
)

const rrfRankOffset = 60

func Fuse(lexical, semantic []Hit) []Hit {
	type fused struct {
		hit   Hit
		score float64
	}
	byChunk := make(map[string]*fused, len(lexical)+len(semantic))
	for _, list := range [][]Hit{lexical[:min(len(lexical), SearchDepth)], semantic[:min(len(semantic), SearchDepth)]} {
		for rank, hit := range list {
			entry := byChunk[hit.ChunkID]
			if entry == nil {
				entry = &fused{hit: hit}
				byChunk[hit.ChunkID] = entry
			}
			entry.score += 1 / float64(rrfRankOffset+rank+1)
		}
	}
	entries := make([]*fused, 0, len(byChunk))
	for _, entry := range byChunk {
		entries = append(entries, entry)
	}
	slices.SortFunc(entries, func(a, b *fused) int {
		return cmp.Or(cmp.Compare(b.score, a.score), cmp.Compare(a.hit.ChunkID, b.hit.ChunkID))
	})
	out := make([]Hit, len(entries))
	for i, entry := range entries {
		out[i] = entry.hit
		out[i].Score = entry.score
	}
	return out
}
