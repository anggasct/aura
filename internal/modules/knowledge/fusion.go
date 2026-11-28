package knowledge

import (
	"cmp"

	"github.com/anggasct/aura/internal/platform/embedding"
)

func Fuse(lexical, semantic []Hit) []Hit {
	lexCap := lexical[:min(len(lexical), SearchDepth)]
	semCap := semantic[:min(len(semantic), SearchDepth)]
	byChunk := make(map[string]Hit, len(lexCap)+len(semCap))
	lexIDs := make([]string, len(lexCap))
	for i := range lexCap {
		lexIDs[i] = lexCap[i].ChunkID
		if existing, exists := byChunk[lexCap[i].ChunkID]; !exists {
			byChunk[lexCap[i].ChunkID] = lexCap[i]
		} else {
			existing.Trusted = existing.Trusted && lexCap[i].Trusted
			byChunk[lexCap[i].ChunkID] = existing
		}
	}
	semIDs := make([]string, len(semCap))
	for i := range semCap {
		semIDs[i] = semCap[i].ChunkID
		if existing, exists := byChunk[semCap[i].ChunkID]; !exists {
			byChunk[semCap[i].ChunkID] = semCap[i]
		} else {
			existing.Trusted = existing.Trusted && semCap[i].Trusted
			byChunk[semCap[i].ChunkID] = existing
		}
	}
	fused := embedding.FuseRRF(lexIDs, semIDs, SearchDepth, cmp.Compare[string])
	out := make([]Hit, len(fused))
	for i, item := range fused {
		hit := byChunk[item.Key]
		hit.Score = item.Score
		out[i] = hit
	}
	return out
}
