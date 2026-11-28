package skills

import (
	"errors"
	"strings"
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

	t.Run("corrupt or unversioned durable skill journal fails closed", func(t *testing.T) {
		cases := map[string][]byte{
			"truncated json":         []byte(`{"content":"SKILL BODY"`),
			"wrong taint_skill type": []byte(`{"content":"SKILL BODY","taint_skill":123}`),
			"plain text legacy":      []byte(`[untrusted_skill_context] LEGACY BODY [/untrusted_skill_context]`),
		}
		for name, raw := range cases {
			t.Run(name, func(t *testing.T) {
				f := newSessionFixture(t, map[string]string{"clean-skill": "CLEAN RULES"}, 4096)
				inv := &skillJournalInvocation{
					journal: map[string][]byte{skillContextAction: raw},
				}
				ctx := durable.WithTurnScope(t.Context(), durable.NewTurnScope(inv))
				blocks, err := f.context.TurnContext(ctx, ownerSkillTurnRequest("turn-2"))
				if err != nil || len(blocks) != 1 {
					t.Fatalf("TurnContext(%s) = %+v, %v; want 1 block", name, blocks, err)
				}
				if blocks[0].Taint == nil || blocks[0].Taint.Source != turn.TaintSourceLegacy {
					t.Fatalf("TurnContext(%s) Taint = %+v, want %s", name, blocks[0].Taint, turn.TaintSourceLegacy)
				}
			})
		}
	})

	t.Run("registry error on invoking turn fails closed without rendering untainted", func(t *testing.T) {
		f := newSessionFixture(t, map[string]string{"tainted-skill": "FOLLOW THESE RULES"}, 4096)
		taintedAct := f.activate(t, "tainted-skill", "turn-1")
		f.registry.records[taintedAct.SkillID].AcceptedTaintSource = "a web_fetch result"
		f.registry.err = errors.New("registry unavailable")

		if _, err := f.context.SkillContext(t.Context(), ownerSkillTurnRequest("turn-1")); err == nil {
			t.Fatal("SkillContext succeeded when registry.Get failed on invoking turn")
		}
		blocks, err := f.context.TurnContext(t.Context(), ownerSkillTurnRequest("turn-1"))
		if err != nil || len(blocks) != 0 {
			t.Fatalf("TurnContext on registry failure = %+v, %v; want nil, nil (never render untainted)", blocks, err)
		}
	})

	t.Run("evicted tainted skill beyond budget does not taint clean rendered skill", func(t *testing.T) {
		cleanBody := strings.Repeat("c", 3000)
		taintedBody := strings.Repeat("t", 3000)
		f := newSessionFixture(t, map[string]string{"clean-skill": cleanBody, "tainted-skill": taintedBody}, 4096)
		taintedAct := f.activate(t, "tainted-skill", "turn-1")
		f.registry.records[taintedAct.SkillID].AcceptedTaintSource = "a webhook"
		f.activate(t, "clean-skill", "turn-2")

		blocks, err := f.context.TurnContext(t.Context(), ownerSkillTurnRequest("turn-3"))
		if err != nil || len(blocks) != 1 {
			t.Fatalf("TurnContext = %+v, %v; want 1 block", blocks, err)
		}
		if !strings.Contains(blocks[0].Content, "clean-skill") || strings.Contains(blocks[0].Content, "tainted-skill") {
			t.Fatalf("rendered block did not evict older tainted skill: %s", blocks[0].Content)
		}
		if blocks[0].Taint != nil {
			t.Fatalf("evicted tainted skill tainted clean block: %+v", blocks[0].Taint)
		}
	})
}
