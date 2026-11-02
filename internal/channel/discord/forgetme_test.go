package discord

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/anggasct/aura/internal/command"
	"github.com/anggasct/aura/internal/config"
)

type fakeEraser struct {
	mu     sync.Mutex
	err    error
	erased []string
}

func (e *fakeEraser) Forget(_ context.Context, principalID string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.erased = append(e.erased, principalID)
	return e.err
}

func newForgetAdapter(t *testing.T, eraser *fakeEraser) (*Adapter, *memorySink, *approvalStub) {
	t.Helper()
	stub := newApprovalStub(t)
	adapter, sink, observer := newAccessAdapter(t, "lobby", []string{"discord:777"}, accessPersonas())
	settings := observer.settings()
	settings.Erasure = eraser
	adapter.SetAccess(&settings)
	adapter.restBase = stub.srv.URL
	registry, err := command.New(CommandBinding())
	if err != nil {
		t.Fatalf("command.New: %v", err)
	}
	dispatcher, err := command.NewDispatcher(registry, command.Ports{Sessions: noSessions{}}, nil)
	if err != nil {
		t.Fatalf("NewDispatcher: %v", err)
	}
	adapter.SetCommands(dispatcher)
	return adapter, sink, stub
}

type noSessions struct{}

func (noSessions) EventCount(context.Context, string) (int, error) { return 0, nil }
func (noSessions) Create(context.Context, *command.Caller) (command.Session, error) {
	return command.Session{}, nil
}
func (noSessions) Switch(context.Context, *command.Caller, string) (command.Session, error) {
	return command.Session{}, nil
}

func lastReply(t *testing.T, stub *approvalStub, channel string) string {
	t.Helper()
	posts := stub.postsTo(channel)
	if len(posts) == 0 {
		t.Fatalf("no reply in %s", channel)
	}
	content, _ := posts[len(posts)-1].body["content"].(string)
	return content
}

func TestForgetMeAsksThenErases(t *testing.T) {
	defaults := config.PersonaMessageDefaults()
	eraser := &fakeEraser{}
	adapter, sink, stub := newForgetAdapter(t, eraser)
	if handled, err := adapter.admit(t.Context(), textMessage("9801", "4400", "", "601", "/forgetme", nil)); err != nil || !handled {
		t.Fatalf("admit(/forgetme) = %v, %v", handled, err)
	}
	if got := lastReply(t, stub, "4400"); got != defaults[config.MessageForgetmeConfirm] {
		t.Fatalf("first reply = %q, want the confirmation prompt", got)
	}
	if len(eraser.erased) != 0 {
		t.Fatalf("erased before confirmation: %v", eraser.erased)
	}
	if _, err := adapter.admit(t.Context(), textMessage("9802", "4400", "", "601", "/forgetme CONFIRM", nil)); err != nil {
		t.Fatalf("admit(/forgetme confirm): %v", err)
	}
	if len(eraser.erased) != 1 || eraser.erased[0] != "discord:601" {
		t.Fatalf("erased = %v, want discord:601", eraser.erased)
	}
	if got := lastReply(t, stub, "4400"); got != defaults[config.MessageForgetmeDone] {
		t.Fatalf("second reply = %q, want the done message", got)
	}
	if sink.calls["message:9801"]+sink.calls["message:9802"] != 0 {
		t.Fatal("/forgetme started a turn")
	}
}

func TestForgetMeRefusesTheOwnerAndReportsNothingOnFailure(t *testing.T) {
	defaults := config.PersonaMessageDefaults()
	eraser := &fakeEraser{}
	adapter, _, stub := newForgetAdapter(t, eraser)
	if _, err := adapter.admit(t.Context(), textMessage("9811", "4401", "", "777", "/forgetme confirm", nil)); err != nil {
		t.Fatalf("owner admit: %v", err)
	}
	if got := lastReply(t, stub, "4401"); got != defaults[config.MessageNotPermitted] || len(eraser.erased) != 0 {
		t.Fatalf("owner reply = %q, erased = %v; want not_permitted and no erasure", got, eraser.erased)
	}
	eraser.err = errors.New("db down")
	if _, err := adapter.admit(t.Context(), textMessage("9812", "4402", "", "601", "/forgetme confirm", nil)); err != nil {
		t.Fatalf("admit: %v", err)
	}
	if posts := stub.postsTo("4402"); len(posts) != 0 {
		t.Fatalf("failed erasure replied %+v, want no done message", posts)
	}
}
