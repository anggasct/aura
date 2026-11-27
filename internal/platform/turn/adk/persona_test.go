package runtimeadk

import (
	"strings"
	"testing"

	auraagent "github.com/anggasct/aura/internal/platform/agent"
	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/principal"
	"github.com/anggasct/aura/internal/platform/turn"
)

const kumaSoul = "You are Kuma: casual, brief, and warm."

func TestPersonaSoulSitsBetweenOwnerBlockAndCaveats(t *testing.T) {
	registry, err := auraagent.Build([]auraagent.Persona{
		{ID: "kuma", Description: "Casual assistant", SoulFile: "personas/kuma.md", Soul: kumaSoul},
	}, []string{"read_file"}, nil, nil, "en")
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	owner := RenderOwnerContext(&OwnerContext{Name: "Angga"})
	for _, class := range []principal.Class{principal.ClassOwner, principal.ClassTrusted} {
		t.Run(string(class), func(t *testing.T) {
			model := &capturingModel{answer: "done", tokens: 1}
			db, sessions, _ := newSessionTestDB(t)
			t.Cleanup(func() { _ = db.Close() })
			modelName, models := capturingModelSet(model)
			executor, err := NewADKExecutor("aura", modelName, models, sessions, &fakeBroker{}, nil, nil,
				WithAgentResolver(registry, nil),
				WithOwnerContext(owner),
				WithContextProvider(&stubRecallProvider{evidence: corpusEvidence()}),
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
			caveatAt := strings.Index(system, turn.RecallEvidenceCaveat)
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

func TestExecuteRefusesAChangedPersonaOnlyForAPinnedWorkflowTurn(t *testing.T) {
	pinned := auraagent.Definition{ID: "researcher", Instructions: "Read carefully."}
	fingerprint := pinned.Fingerprint()
	cases := []struct {
		name     string
		origin   turn.Origin
		pin      *turn.PersonaFingerprint
		current  auraagent.Definition
		wantCode turn.ErrorCode
		wantRuns int
	}{
		{name: "no pin runs", origin: turn.OriginWorkflow, current: pinned, wantRuns: 1},
		{name: "unchanged persona runs", origin: turn.OriginWorkflow, pin: &turn.PersonaFingerprint{AgentID: fingerprint.AgentID, SoulSHA256: fingerprint.SoulSHA256, ToolsSHA256: fingerprint.ToolsSHA256, ModelRoute: fingerprint.ModelRoute}, current: pinned, wantRuns: 1},
		{name: "changed tools alone still runs", origin: turn.OriginWorkflow, pin: &turn.PersonaFingerprint{SoulSHA256: fingerprint.SoulSHA256, ToolsSHA256: "other-tools", ModelRoute: fingerprint.ModelRoute}, current: pinned, wantRuns: 1},
		{name: "edited instructions are refused", origin: turn.OriginWorkflow, pin: &turn.PersonaFingerprint{SoulSHA256: fingerprint.SoulSHA256, ModelRoute: fingerprint.ModelRoute}, current: auraagent.Definition{ID: "researcher", Instructions: "Read carelessly."}, wantCode: turn.ErrorCodePersonaMismatch},
		{name: "changed model route is refused", origin: turn.OriginWorkflow, pin: &turn.PersonaFingerprint{SoulSHA256: fingerprint.SoulSHA256, ModelRoute: "premium"}, current: pinned, wantCode: turn.ErrorCodePersonaMismatch},
		{name: "a channel turn runs on an edited persona", origin: "telegram:pocket", pin: &turn.PersonaFingerprint{SoulSHA256: fingerprint.SoulSHA256, ModelRoute: fingerprint.ModelRoute}, current: auraagent.Definition{ID: "researcher", Instructions: "Read carelessly."}, wantRuns: 1},
		{name: "a terminal turn runs on a changed model route", origin: turn.OriginTerminal, pin: &turn.PersonaFingerprint{SoulSHA256: fingerprint.SoulSHA256, ModelRoute: "premium"}, current: pinned, wantRuns: 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			model := &fakeADKModel{answer: "done", tokens: 1}
			modelName, models := fakeModelSet(model)
			executor, resolver, db := newAgentTestExecutor(t, modelName, models)
			resolver.definition = tc.current
			mustCreateSession(t, db, "session-1")
			req := personaTurn()
			req.AgentID = "researcher"
			req.Origin = tc.origin
			req.Persona = tc.pin
			var got error
			for _, err := range executor.Execute(t.Context(), req) {
				if err != nil {
					got = err
					break
				}
			}
			if tc.wantCode == "" && got != nil {
				t.Fatalf("Execute: %v", got)
			}
			if tc.wantCode != "" && !fault.HasCode(got, tc.wantCode) {
				t.Fatalf("Execute error = %v, want code %q", got, tc.wantCode)
			}
			if model.callCount != tc.wantRuns {
				t.Fatalf("model calls = %d, want %d", model.callCount, tc.wantRuns)
			}
		})
	}
}

func TestExecuteWithoutAPersonaResolverHasNoPersonaToCompareWithThePin(t *testing.T) {
	model := &fakeADKModel{answer: "done", tokens: 1}
	modelName, models := fakeModelSet(model)
	db, sessions, _ := newSessionTestDB(t)
	t.Cleanup(func() { _ = db.Close() })
	executor, err := NewADKExecutor("aura", modelName, models, sessions, &fakeBroker{}, nil, nil)
	if err != nil {
		t.Fatalf("NewADKExecutor: %v", err)
	}
	mustCreateSession(t, db, "session-1")
	req := personaTurn()
	req.Origin = turn.OriginWorkflow
	req.Persona = &turn.PersonaFingerprint{AgentID: "researcher", SoulSHA256: "soul", ModelRoute: "premium"}
	drainTurn(t, executor, req)
	if model.callCount != 1 {
		t.Fatalf("model calls = %d, want 1", model.callCount)
	}
}
