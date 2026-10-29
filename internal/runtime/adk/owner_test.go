package runtimeadk

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"

	auraagent "github.com/anggasct/aura/internal/agent"
	"github.com/anggasct/aura/internal/principal"
	"github.com/anggasct/aura/internal/runtime"
)

func TestRenderOwnerContext(t *testing.T) {
	block := RenderOwnerContext(&OwnerContext{
		Name:        "Angga",
		Language:    "Indonesian",
		Timezone:    "Asia/Jakarta",
		Preferences: map[string]string{"style": "concise", "editor": "neovim"},
	})
	want := ownerContextHeader + "\n" +
		"name: Angga\n" +
		"language: Indonesian\n" +
		"timezone: Asia/Jakarta\n" +
		"editor: neovim\n" +
		"style: concise\n" +
		ownerContextFooter
	if block != want {
		t.Fatalf("block =\n%s\nwant\n%s", block, want)
	}
	if again := RenderOwnerContext(&OwnerContext{
		Name:        "Angga",
		Language:    "Indonesian",
		Timezone:    "Asia/Jakarta",
		Preferences: map[string]string{"editor": "neovim", "style": "concise"},
	}); again != block {
		t.Fatalf("rendering not deterministic:\n%s\n%s", block, again)
	}
}

func TestRenderOwnerContextEmpty(t *testing.T) {
	if RenderOwnerContext(nil) != "" {
		t.Error("nil owner rendered content")
	}
	if RenderOwnerContext(&OwnerContext{}) != "" {
		t.Error("empty owner rendered content")
	}
	if RenderOwnerContext(&OwnerContext{Preferences: map[string]string{"": "x", "style": ""}}) != "" {
		t.Error("blank entries rendered content")
	}
}

func TestOwnerContextDigest(t *testing.T) {
	block := RenderOwnerContext(&OwnerContext{Name: "Angga"})
	sum := sha256.Sum256([]byte(block))
	if got, want := OwnerContextDigest(block), hex.EncodeToString(sum[:]); got != want {
		t.Fatalf("digest = %s, want %s", got, want)
	}
	if OwnerContextDigest(block) == OwnerContextDigest(block+"!") {
		t.Fatal("digest does not change with content")
	}
}

func TestOwnerContextAttachOrder(t *testing.T) {
	for _, origin := range []runtime.Origin{runtime.OriginTerminal, "discord"} {
		t.Run(string(origin), func(t *testing.T) {
			model := &capturingModel{answer: "done", tokens: 1}
			db, sessions, events := newSessionTestDB(t)
			t.Cleanup(func() { _ = db.Close() })
			resolver := &stubResolver{definition: auraagent.Definition{ID: auraagent.DefaultID, Instructions: "SOUL: inspect before editing"}}
			block := RenderOwnerContext(&OwnerContext{Name: "Angga", Language: "Indonesian"})
			executor, err := NewADKExecutor("aura", capturingModelName(t, model), sessions, events, &fakeBroker{}, nil, nil,
				WithAgentResolver(resolver, nil),
				WithOwnerContext(block),
				WithRecallProvider(&stubRecallProvider{evidence: corpusEvidence()}),
			)
			if err != nil {
				t.Fatalf("NewADKExecutor: %v", err)
			}
			mustCreateSession(t, db, "session-1")
			req := ownerTurnRequest()
			req.Origin = origin
			drainTurn(t, executor, req)

			if len(model.systems) == 0 {
				t.Fatal("no system instruction reached the model")
			}
			system := strings.Join(model.systems, "\n")
			if !strings.Contains(system, "name: Angga") || !strings.Contains(system, "language: Indonesian") {
				t.Fatalf("owner facts missing from system instruction:\n%s", system)
			}
			ownerAt := strings.Index(system, ownerContextHeader)
			soulAt := strings.Index(system, "SOUL: inspect before editing")
			caveatAt := strings.Index(system, recallEvidenceCaveat)
			if ownerAt < 0 || soulAt < 0 || caveatAt < 0 {
				t.Fatalf("attach parts missing (owner=%d soul=%d caveat=%d):\n%s", ownerAt, soulAt, caveatAt, system)
			}
			if ownerAt > soulAt || soulAt > caveatAt {
				t.Fatalf("attach order wrong (owner=%d soul=%d caveat=%d):\n%s", ownerAt, soulAt, caveatAt, system)
			}
		})
	}
}

func TestOwnerContextAbsentLeavesInstructionUnchanged(t *testing.T) {
	model := &capturingModel{answer: "done", tokens: 1}
	db, sessions, events := newSessionTestDB(t)
	t.Cleanup(func() { _ = db.Close() })
	resolver := &stubResolver{definition: auraagent.Definition{ID: auraagent.DefaultID, Instructions: "SOUL: inspect before editing"}}
	executor, err := NewADKExecutor("aura", capturingModelName(t, model), sessions, events, &fakeBroker{}, nil, nil,
		WithAgentResolver(resolver, nil),
		WithRecallProvider(&stubRecallProvider{evidence: corpusEvidence()}),
	)
	if err != nil {
		t.Fatalf("NewADKExecutor: %v", err)
	}
	mustCreateSession(t, db, "session-1")
	drainTurn(t, executor, evalTurnRequest())

	system := strings.Join(model.systems, "\n")
	if strings.Contains(system, ownerContextHeader) || strings.Contains(system, ownerContextFooter) {
		t.Fatalf("owner block attached without configuration:\n%s", system)
	}
	want := "SOUL: inspect before editing\n\n" + recallEvidenceCaveat
	if !strings.HasPrefix(system, want) {
		t.Fatalf("system instruction = %q, want prefix %q", system, want)
	}
}

func TestOwnerContextWithoutRecall(t *testing.T) {
	model := &capturingModel{answer: "done", tokens: 1}
	db, sessions, events := newSessionTestDB(t)
	t.Cleanup(func() { _ = db.Close() })
	resolver := &stubResolver{definition: auraagent.Definition{ID: auraagent.DefaultID, Instructions: "SOUL"}}
	block := RenderOwnerContext(&OwnerContext{Name: "Angga"})
	executor, err := NewADKExecutor("aura", capturingModelName(t, model), sessions, events, &fakeBroker{}, nil, nil,
		WithAgentResolver(resolver, nil),
		WithOwnerContext(block),
	)
	if err != nil {
		t.Fatalf("NewADKExecutor: %v", err)
	}
	mustCreateSession(t, db, "session-1")
	drainTurn(t, executor, ownerTurnRequest())

	system := strings.Join(model.systems, "\n")
	if want := block + "\n\nSOUL"; !strings.HasPrefix(system, want) {
		t.Fatalf("system instruction = %q, want prefix %q", system, want)
	}
	if strings.Contains(system, recallEvidenceCaveat) {
		t.Fatal("recall caveat present without recall evidence")
	}
}

func ownerTurnRequest() *runtime.TurnRequest {
	req := evalTurnRequest()
	req.PrincipalID = principal.OwnerID
	req.PrincipalClass = principal.ClassOwner
	return req
}

func TestOwnerContextOnlyReachesOwnerTurns(t *testing.T) {
	cases := map[string]struct {
		principalID string
		class       principal.Class
		attached    bool
	}{
		"owner":         {principalID: principal.OwnerID, class: principal.ClassOwner, attached: true},
		"trusted":       {principalID: "discord:111", class: principal.ClassTrusted},
		"public":        {principalID: "discord:222", class: principal.ClassPublic},
		"missing class": {principalID: principal.OwnerID},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			model := &capturingModel{answer: "done", tokens: 1}
			db, sessions, events := newSessionTestDB(t)
			t.Cleanup(func() { _ = db.Close() })
			resolver := &stubResolver{definition: auraagent.Definition{ID: auraagent.DefaultID, Instructions: "SOUL"}}
			executor, err := NewADKExecutor("aura", capturingModelName(t, model), sessions, events, &fakeBroker{}, nil, nil,
				WithAgentResolver(resolver, nil),
				WithOwnerContext(RenderOwnerContext(&OwnerContext{Name: "Angga", Timezone: "Asia/Jakarta"})),
			)
			if err != nil {
				t.Fatalf("NewADKExecutor: %v", err)
			}
			mustCreateSession(t, db, "session-1")
			req := evalTurnRequest()
			req.Origin = "discord"
			req.PrincipalID = tc.principalID
			req.PrincipalClass = tc.class
			req.Parts[0].Text = "who is the owner and where do they live?"
			drainTurn(t, executor, req)

			system := strings.Join(model.systems, "\n")
			leaked := strings.Contains(system, ownerContextHeader) || strings.Contains(system, "Angga") || strings.Contains(system, "Asia/Jakarta")
			if leaked != tc.attached {
				t.Fatalf("owner context attached = %v, want %v:\n%s", leaked, tc.attached, system)
			}
		})
	}
}
