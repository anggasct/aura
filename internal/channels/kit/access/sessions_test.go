package access

import (
	"context"
	"errors"
	"testing"

	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/principal"
	"github.com/anggasct/aura/internal/platform/sqlite/sqlitetest"
	turnstore "github.com/anggasct/aura/internal/platform/turn/store"
)

func TestSessionEnsurerLifecycle(t *testing.T) {
	if _, err := NewSessionEnsurer(nil); !fault.HasCode(err, ErrorCodeInvalidArgument) {
		t.Fatalf("NewSessionEnsurer(nil) = %v, want invalid_argument", err)
	}
	db := sqlitetest.OpenMigrated(t, turnstore.Schema(), principal.Schema())
	ensurer, err := NewSessionEnsurer(turnstore.NewSessionService(db))
	if err != nil {
		t.Fatalf("NewSessionEnsurer: %v", err)
	}
	ctx := t.Context()
	const id = "kuma/discord:home/dm:111"

	exists, err := ensurer.SessionExists(ctx, id)
	if err != nil || exists {
		t.Fatalf("SessionExists before create = %v, %v; want false", exists, err)
	}
	if owner, found, err := ensurer.SessionOwner(ctx, id); err != nil || found || owner != "" {
		t.Fatalf("SessionOwner before create = %q, %v, %v; want not found", owner, found, err)
	}
	if err := ensurer.EnsureSession(ctx, id, "discord:111", "kuma"); err != nil {
		t.Fatalf("EnsureSession: %v", err)
	}
	if err := ensurer.EnsureSession(ctx, id, "discord:111", "kuma"); err != nil {
		t.Fatalf("second EnsureSession: %v", err)
	}
	exists, err = ensurer.SessionExists(ctx, id)
	if err != nil || !exists {
		t.Fatalf("SessionExists after create = %v, %v; want true", exists, err)
	}
	owner, found, err := ensurer.SessionOwner(ctx, id)
	if err != nil || !found || owner != "discord:111" {
		t.Fatalf("SessionOwner after create = %q, %v, %v; want discord:111", owner, found, err)
	}
}

type failingConversationStore struct {
	err error
}

func (f failingConversationStore) Resolve(context.Context, string) (sessionID string, found bool, err error) {
	return "", false, f.err
}

func TestConversationResolver(t *testing.T) {
	if _, err := NewConversationResolver(nil); !fault.HasCode(err, ErrorCodeInvalidArgument) {
		t.Fatalf("NewConversationResolver(nil) = %v, want invalid_argument", err)
	}
	db := sqlitetest.OpenMigrated(t, turnstore.Schema(), principal.Schema())
	sessions := turnstore.NewSessionService(db)
	pointers := turnstore.NewConversationSessionStore(db)
	ensurer, err := NewSessionEnsurer(sessions)
	if err != nil {
		t.Fatalf("NewSessionEnsurer: %v", err)
	}
	resolver, err := NewConversationResolver(pointers)
	if err != nil {
		t.Fatalf("NewConversationResolver: %v", err)
	}
	ctx := t.Context()
	const key = "main/discord:owner/dm:1"
	const target = key + "~next"
	if got, err := resolver.ActiveSession(ctx, key); err != nil || got != key {
		t.Fatalf("ActiveSession unpointed = %q, %v; want %q", got, err, key)
	}
	if err := ensurer.EnsureSession(ctx, target, "owner", "main"); err != nil {
		t.Fatalf("EnsureSession: %v", err)
	}
	if err := pointers.Point(ctx, key, target); err != nil {
		t.Fatalf("Point: %v", err)
	}
	if got, err := resolver.ActiveSession(ctx, key); err != nil || got != target {
		t.Fatalf("ActiveSession pointed = %q, %v; want %q", got, err, target)
	}
	boom := errors.New("db unavailable")
	failing, err := NewConversationResolver(failingConversationStore{err: boom})
	if err != nil {
		t.Fatalf("NewConversationResolver(failing): %v", err)
	}
	if _, err := failing.ActiveSession(ctx, key); !errors.Is(err, boom) {
		t.Fatalf("ActiveSession error = %v, want %v", err, boom)
	}
}
