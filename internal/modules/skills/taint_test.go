package skills

import (
	"testing"

	"github.com/anggasct/aura/internal/platform/durable"
	"github.com/anggasct/aura/internal/platform/turn"
)

func TestSkillAcceptedOnATaintedCardTaintsItsTurns(t *testing.T) {
	t.Run("context block carries skill taint mark and replays across durable journal", func(t *testing.T) {
		f := newSessionFixture(t, map[string]string{"tainted-skill": "FOLLOW THESE RULES", "clean-skill": "CLEAN RULES"}, 4096)
		taintedAct := f.activate(t, "tainted-skill", "turn-1")
		f.registry.records[taintedAct.SkillID].AcceptedTaintSource = "a web_fetch result from example.com"

		inv := &skillJournalInvocation{}
		ctx := durable.WithTurnScope(t.Context(), durable.NewTurnScope(inv))
		req := ownerSkillTurnRequest("turn-2")

		blocks, err := f.context.TurnContext(ctx, req)
		if err != nil || len(blocks) != 1 {
			t.Fatalf("TurnContext(tainted skill) = %+v, %v; want 1 block", blocks, err)
		}
		if blocks[0].Taint == nil {
			t.Fatal("tainted skill context block has nil Taint, want TaintSourceSkill")
		}
		if blocks[0].Taint.Source != turn.TaintSourceSkill || blocks[0].Taint.Detail != taintedAct.SkillID {
			t.Errorf("block Taint = %+v, want source=%s detail=%s", blocks[0].Taint, turn.TaintSourceSkill, taintedAct.SkillID)
		}
		if got := blocks[0].Taint.Label(); got != "a skill accepted in a conversation with untrusted content" {
			t.Errorf("label = %q", got)
		}

		f.registry.records[taintedAct.SkillID].AcceptedTaintSource = ""
		replayed, err := f.context.TurnContext(ctx, req)
		if err != nil || len(replayed) != 1 || replayed[0].Taint == nil || replayed[0].Taint.Source != turn.TaintSourceSkill {
			t.Fatalf("replayed tainted skill block = %+v, %v; want preserved TaintSourceSkill", replayed, err)
		}
	})

	t.Run("cleanly accepted skill does not taint its turns", func(t *testing.T) {
		f := newSessionFixture(t, map[string]string{"clean-skill": "CLEAN RULES"}, 4096)
		f.activate(t, "clean-skill", "turn-1")

		blocks, err := f.context.TurnContext(t.Context(), ownerSkillTurnRequest("turn-2"))
		if err != nil || len(blocks) != 1 {
			t.Fatalf("TurnContext(clean skill) = %+v, %v", blocks, err)
		}
		if blocks[0].Taint != nil {
			t.Fatalf("clean skill block Taint = %+v, want nil", blocks[0].Taint)
		}
	})
}
