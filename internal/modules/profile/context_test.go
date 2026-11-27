package profile

import (
	stdcontext "context"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/platform/principal"
	"github.com/anggasct/aura/internal/platform/turn"
)

type countingRegistry struct {
	*fakeRegistry
	searchCalls int
}

func (c *countingRegistry) Search(ctx stdcontext.Context, ownerID, category, query string, verifiedOnly bool, limit int, now time.Time) ([]SearchHit, error) {
	c.searchCalls++
	return c.fakeRegistry.Search(ctx, ownerID, category, query, verifiedOnly, limit, now)
}

func TestProfileContextOnlyAsksForOwnerTurns(t *testing.T) {
	reg := &countingRegistry{fakeRegistry: newFakeRegistry()}
	svc, err := NewService(reg, ServiceOptions{MinConfidence: 0.70})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	if _, err := svc.SetFact(t.Context(), principal.OwnerID, "tool", "editor", "neovim", nil, time.Now().UTC()); err != nil {
		t.Fatalf("SetFact: %v", err)
	}
	mod := &Module{
		service:   svc,
		maxFacts:  10,
		maxTokens: 1024,
	}
	for name, class := range map[string]principal.Class{"trusted": principal.ClassTrusted, "public": principal.ClassPublic, "missing": ""} {
		t.Run(name, func(t *testing.T) {
			before := reg.searchCalls
			req := &turn.TurnRequest{
				TurnID:         "turn-1",
				SessionID:      "session-1",
				PrincipalID:    "discord:111",
				PrincipalClass: class,
				Origin:         turn.OriginTerminal,
			}
			blocks, err := mod.TurnContext(t.Context(), req)
			if err != nil {
				t.Fatalf("TurnContext: %v", err)
			}
			if len(blocks) != 0 {
				t.Fatalf("blocks = %+v, want none for class %q", blocks, class)
			}
			if reg.searchCalls != before {
				t.Fatalf("registry search called %d times for class %q, want 0", reg.searchCalls-before, class)
			}
		})
	}
}
