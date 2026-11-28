package profile

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/platform/operation"
	"github.com/anggasct/aura/internal/platform/operation/operationtest"
	"github.com/anggasct/aura/internal/platform/principal"
	"github.com/anggasct/aura/internal/platform/turn"
	turnstore "github.com/anggasct/aura/internal/platform/turn/store"
)

func TestVerifiedProfileReadsDoNotTaint(t *testing.T) {
	ctx := t.Context()
	db := operationtest.OpenDatabase(t, Schema())
	store, err := NewSQLiteStore(db)
	if err != nil {
		t.Fatalf("NewSQLiteStore: %v", err)
	}
	svc, err := NewService(store, ServiceOptions{MinConfidence: 0.7})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	ops, err := NewOperations(&OperationDeps{Service: svc})
	if err != nil {
		t.Fatalf("NewOperations: %v", err)
	}
	rig := operationtest.NewRig(t, &operationtest.Options{
		DB:       db,
		Catalogs: []operation.Catalog{Catalog()},
		Bindings: func(*operationtest.Stubs) []operation.Binding { return ops.Bindings() },
	})
	registry := rig.Registry

	now := time.Now().UTC()
	verified, err := svc.SetFact(ctx, principal.OwnerID, "preference", "editor", "neovim", nil, now)
	if err != nil {
		t.Fatalf("SetFact verified: %v", err)
	}

	listDesc, _ := registry.Descriptor("profile.list")
	showDesc, _ := registry.Descriptor("profile.show")
	reviewDesc, _ := registry.Descriptor("profile.review")
	acceptDesc, _ := registry.Descriptor("profile.accept")

	if !acceptDesc.UntrustedOutput {
		t.Fatal("profile.accept must have UntrustedOutput=true")
	}

	caller := operation.Caller{Surface: operation.SurfaceCLI, PrincipalID: principal.OwnerID}
	listClean, err := registry.Invoke(ctx, "profile.list", &operation.Call{Caller: caller, Input: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatalf("profile.list clean: %v", err)
	}
	if listDesc.ResultTaint(listClean.Data) {
		t.Fatalf("profile.list of only verified facts tainted: %s", listClean.Data)
	}

	showClean, err := registry.Invoke(ctx, "profile.show", &operation.Call{Caller: caller, Input: json.RawMessage(`{"id":"` + verified.ID + `"}`)})
	if err != nil {
		t.Fatalf("profile.show verified: %v", err)
	}
	if showDesc.ResultTaint(showClean.Data) {
		t.Fatalf("profile.show of verified fact tainted: %s", showClean.Data)
	}

	reviewEmpty, err := registry.Invoke(ctx, "profile.review", &operation.Call{Caller: caller, Input: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatalf("profile.review empty: %v", err)
	}
	if reviewDesc.ResultTaint(reviewEmpty.Data) {
		t.Fatalf("profile.review with no candidates tainted: %s", reviewEmpty.Data)
	}

	if err := turnstore.NewSessionService(db).Create(ctx, &turn.Session{ID: "sess-prof", OwnerID: principal.OwnerID, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("Create session: %v", err)
	}
	if _, err := turnstore.NewEventStore(db).AppendSequenced(ctx, "sess-prof", &turn.Event{
		ID: "ev-1", SessionID: "sess-prof", TurnID: "turn-1", Author: principal.OwnerID,
		Kind: turn.EventKindTurnAccepted, SchemaVersion: 1, Payload: json.RawMessage(`{"origin":"terminal","principal":"local-owner"}`), CreatedAt: now,
	}); err != nil {
		t.Fatalf("AppendSequenced: %v", err)
	}
	candidate, err := svc.ProposeFact(ctx, principal.OwnerID, "preference", "drink", "coffee", &Evidence{
		SourceEventID: "ev-1",
		SourceDigest:  "sha256:abc",
		Provider:      "test",
		Model:         "m",
		ObservedAt:    now,
	}, now)
	if err != nil {
		t.Fatalf("ProposeFact candidate: %v", err)
	}

	listDirty, err := registry.Invoke(ctx, "profile.list", &operation.Call{Caller: caller, Input: json.RawMessage(`{"status":"candidate"}`)})
	if err != nil {
		t.Fatalf("profile.list all: %v", err)
	}
	if !listDesc.ResultTaint(listDirty.Data) {
		t.Fatalf("profile.list including candidate did not taint: %s", listDirty.Data)
	}

	showDirty, err := registry.Invoke(ctx, "profile.show", &operation.Call{Caller: caller, Input: json.RawMessage(`{"id":"` + candidate.ID + `"}`)})
	if err != nil {
		t.Fatalf("profile.show candidate: %v", err)
	}
	if !showDesc.ResultTaint(showDirty.Data) {
		t.Fatalf("profile.show of candidate did not taint: %s", showDirty.Data)
	}

	reviewNonEmpty, err := registry.Invoke(ctx, "profile.review", &operation.Call{Caller: caller, Input: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatalf("profile.review non-empty: %v", err)
	}
	if !reviewDesc.ResultTaint(reviewNonEmpty.Data) {
		t.Fatalf("profile.review with candidates did not taint: %s", reviewNonEmpty.Data)
	}

	if !listDesc.ResultTaint(json.RawMessage(`{bad`)) {
		t.Error("profile.list unparsable output did not fail closed")
	}
}
