package runtimeadk

import (
	"strings"
	"testing"

	auraagent "github.com/anggasct/aura/internal/agent"
	"github.com/anggasct/aura/internal/config"
	"github.com/anggasct/aura/internal/principal"
)

const kumaSoul = "You are Kuma: casual, brief, and warm."

func TestPersonaSoulSitsBetweenOwnerBlockAndCaveats(t *testing.T) {
	registry, err := auraagent.Build([]config.AgentDefinition{
		{ID: "kuma", Description: "Casual assistant", SoulFile: "personas/kuma.md", Soul: kumaSoul},
	}, []string{"read_file"}, nil)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	owner := RenderOwnerContext(&OwnerContext{Name: "Angga"})
	for _, class := range []principal.Class{principal.ClassOwner, principal.ClassTrusted} {
		t.Run(string(class), func(t *testing.T) {
			model := &capturingModel{answer: "done", tokens: 1}
			db, sessions, events := newSessionTestDB(t)
			t.Cleanup(func() { _ = db.Close() })
			executor, err := NewADKExecutor("aura", capturingModelName(t, model), sessions, events, &fakeBroker{}, nil, nil,
				WithAgentResolver(registry, nil),
				WithOwnerContext(owner),
				WithRecallProvider(&stubRecallProvider{evidence: corpusEvidence()}),
			)
			if err != nil {
				t.Fatalf("NewADKExecutor: %v", err)
			}
			mustCreateSession(t, db, "session-1")
			req := ownerTurnRequest()
			req.PrincipalClass = class
			req.AgentID = "kuma"
			drainTurn(t, executor, req)

			system := strings.Join(model.systems, "\n")
			soulAt := strings.Index(system, kumaSoul)
			caveatAt := strings.Index(system, recallEvidenceCaveat)
			ownerAt := strings.Index(system, ownerContextHeader)
			if soulAt < 0 || caveatAt < soulAt {
				t.Fatalf("soul missing or after caveat (soul=%d caveat=%d):\n%s", soulAt, caveatAt, system)
			}
			if class == principal.ClassOwner && (ownerAt < 0 || ownerAt > soulAt) {
				t.Fatalf("owner block missing or after soul (owner=%d soul=%d):\n%s", ownerAt, soulAt, system)
			}
			if class != principal.ClassOwner && ownerAt >= 0 {
				t.Fatalf("owner block reached a %s turn:\n%s", class, system)
			}
		})
	}
}
