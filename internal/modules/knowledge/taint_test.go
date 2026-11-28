package knowledge

import (
	"encoding/json"
	"testing"

	"github.com/anggasct/aura/internal/platform/durable"
	"github.com/anggasct/aura/internal/platform/turn"
)

func TestUntrustedPassagesTaintOnlyTheTurnTheyEnter(t *testing.T) {
	ctx := t.Context()

	t.Run("turn with rendered untrusted passages gets a knowledge taint mark", func(t *testing.T) {
		index := &contextTestIndex{
			hits: []Hit{{
				ChunkID:    "chunk-1",
				Collection: "faq",
				Label:      "refunds.md#1",
				Heading:    "Refunds",
				Content:    contextTestText,
			}},
		}
		provider := newTestContextProvider(t, index)
		blocks, err := provider.TurnContext(ctx, contextTurnRequest("faq"))
		if err != nil {
			t.Fatalf("TurnContext(): %v", err)
		}
		if len(blocks) == 0 || blocks[0].Taint == nil {
			t.Fatalf("blocks = %+v, want non-nil Taint mark", blocks)
		}
		if blocks[0].Taint.Source != turn.TaintSourceKnowledge || blocks[0].Taint.Detail != "faq" {
			t.Fatalf("blocks[0].Taint = %+v, want knowledge faq", blocks[0].Taint)
		}
	})

	t.Run("turn with zero rendered passages stays clean", func(t *testing.T) {
		index := &contextTestIndex{hits: nil}
		provider := newTestContextProvider(t, index)
		blocks, err := provider.TurnContext(ctx, contextTurnRequest("faq"))
		if err != nil {
			t.Fatalf("TurnContext(): %v", err)
		}
		for i := range blocks {
			if blocks[i].Taint != nil {
				t.Fatalf("blocks[%d].Taint = %+v, want nil when no passage was rendered", i, blocks[i].Taint)
			}
		}
	})
}

func TestKnowledgeJournalWithoutTrustFieldTaints(t *testing.T) {
	raw, err := json.Marshal(map[string]any{
		"outcome":     "ok",
		"block":       "[untrusted_knowledge]\npassage\n[/untrusted_knowledge]",
		"chunk_ids":   []string{"chunk-1"},
		"collections": []string{"faq"},
		"counts":      map[string]int{"retrieved": 1, "rendered": 1},
	})
	if err != nil {
		t.Fatalf("marshal legacy knowledge journal: %v", err)
	}
	inv := &contextJournalInvocation{
		journal: map[string][]byte{
			knowledgeAction: raw,
		},
	}
	ctx := durable.WithTurnScope(t.Context(), durable.NewTurnScope(inv))
	provider := newTestContextProvider(t, &contextTestIndex{})
	blocks, err := provider.TurnContext(ctx, contextTurnRequest("faq"))
	if err != nil {
		t.Fatalf("TurnContext() replay: %v", err)
	}
	if len(blocks) == 0 || blocks[0].Taint == nil || blocks[0].Taint.Source != turn.TaintSourceKnowledge {
		t.Fatalf("replayed blocks = %+v, want fail-closed knowledge taint when only_trusted is absent", blocks)
	}
}
