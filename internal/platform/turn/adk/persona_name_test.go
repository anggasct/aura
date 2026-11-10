package runtimeadk

import (
	"context"
	"strings"
	"testing"

	auraagent "github.com/anggasct/aura/internal/platform/agent"
	"github.com/anggasct/aura/internal/platform/turn"
	runtimeingress "github.com/anggasct/aura/internal/platform/turn/ingress"
	"github.com/anggasct/aura/internal/store"

	"google.golang.org/adk/v2/session"
	"google.golang.org/genai"
)

func newPersonaNameExecutor(t *testing.T, model *capturingModel, definition auraagent.Definition) (*ADKExecutor, store.EventStore) {
	t.Helper()
	db, sessions, events := newSessionTestDB(t)
	resolver := &stubResolver{definition: definition}
	executor, err := NewADKExecutor("aura", capturingModelName(t, model), sessions, events, &fakeBroker{}, nil, nil, WithAgentResolver(resolver, nil))
	if err != nil {
		t.Fatalf("NewADKExecutor: %v", err)
	}
	mustCreateSession(t, db, "session-1")
	return executor, events
}

func seedHistory(t *testing.T, events store.EventStore, author string) {
	t.Helper()
	turns := []*session.Event{
		{ID: "seed-user", InvocationID: "inv-0", Author: userAuthor},
		{ID: "seed-model", InvocationID: "inv-0", Author: author},
	}
	turns[0].Content = genai.NewContentFromText("earlier question", genai.RoleUser)
	turns[1].Content = genai.NewContentFromText("earlier answer", genai.RoleModel)
	for i, ev := range turns {
		stored, err := store.RuntimeEventFromADK("session-1", "turn-0", ev)
		if err != nil {
			t.Fatalf("RuntimeEventFromADK: %v", err)
		}
		stored.Sequence = uint64(i + 1)
		if err := events.Append(context.Background(), &stored); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}
}

func personaTurn() *turn.TurnRequest {
	return &turn.TurnRequest{
		TurnID: "turn-1", SessionID: "session-1", PrincipalID: "user-1",
		Origin: turn.OriginTerminal, Parts: []runtimeingress.InputPart{{Text: "hello"}},
	}
}

func TestPersonaNameIsTheAgentIdentity(t *testing.T) {
	for name, tc := range map[string]struct {
		definition auraagent.Definition
		want       string
	}{
		"named persona": {definition: auraagent.Definition{ID: "kuma", Name: "Kuma"}, want: `Your internal name is "Kuma".`},
		"default main":  {definition: auraagent.Definition{ID: auraagent.DefaultID, Name: auraagent.DefaultName}, want: `Your internal name is "Aura".`},
	} {
		t.Run(name, func(t *testing.T) {
			model := &capturingModel{answer: "hi"}
			executor, _ := newPersonaNameExecutor(t, model, tc.definition)
			drainTurn(t, executor, personaTurn())
			if !strings.Contains(strings.Join(model.systems, "\n"), tc.want) {
				t.Fatalf("system instruction lacks %q:\n%s", tc.want, strings.Join(model.systems, "\n"))
			}
		})
	}
}

func TestEarlierRepliesStayThePersonasOwn(t *testing.T) {
	for name, persona := range map[string]string{"legacy author": "Kuma", "renamed persona": "Kumo"} {
		t.Run(name, func(t *testing.T) {
			model := &capturingModel{answer: "new answer"}
			executor, events := newPersonaNameExecutor(t, model, auraagent.Definition{ID: "kuma", Name: persona})
			seedHistory(t, events, "aura")
			produced := drainTurn(t, executor, personaTurn())

			var replayed []string
			for _, content := range model.contents {
				for _, part := range content.Parts {
					if strings.Contains(part.Text, "For context") || strings.Contains(part.Text, "said:") {
						t.Fatalf("history replayed as another agent: %q", part.Text)
					}
					if content.Role == genai.RoleModel {
						replayed = append(replayed, part.Text)
					}
				}
			}
			if len(replayed) == 0 || replayed[0] != "earlier answer" {
				t.Fatalf("model-role history = %q, want the earlier answer as the persona's own turn", replayed)
			}
			for i := range produced {
				if produced[i].Kind == store.EventKindADK && produced[i].Author != userAuthor && produced[i].Author != persona {
					t.Fatalf("new event author = %q, want %q", produced[i].Author, persona)
				}
			}
		})
	}
}
