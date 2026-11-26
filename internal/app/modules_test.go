package app

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/anggasct/aura/internal/platform/turn"
)

func TestComposePostTurnHooks(t *testing.T) {
	if composeTurnHooks(nil) != nil || composeTurnHooks([]turn.TurnHook{nil}) != nil {
		t.Fatal("composed hook from nothing")
	}
	var calls []string
	first := turnHookFunc(func(context.Context, *turn.TurnRequest, []turn.Event) error {
		calls = append(calls, "first")
		return errors.New("first failed")
	})
	second := turnHookFunc(func(context.Context, *turn.TurnRequest, []turn.Event) error {
		calls = append(calls, "second")
		return nil
	})
	err := composeTurnHooks([]turn.TurnHook{first, nil, second})(t.Context(), ownerTurn(turn.OriginTerminal), nil)
	if err == nil || !strings.Contains(err.Error(), "first failed") {
		t.Errorf("err = %v, want the first failure", err)
	}
	if strings.Join(calls, ",") != "first,second" {
		t.Errorf("calls = %v, want both hooks in order", calls)
	}
}
