package app

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/modules/profile"
	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/sqlite"
	"github.com/anggasct/aura/internal/platform/turn"
	turnstore "github.com/anggasct/aura/internal/platform/turn/store"
)

const profileAgentOriginStep = 28

func TestProfileAgentOriginStepKeepsEveryFactItsEvidenceAndItsIndex(t *testing.T) {
	ctx := t.Context()
	var before []sqlite.Migration
	history := Migrations()
	for _, step := range history {
		if step.Version < profileAgentOriginStep {
			before = append(before, step)
		}
	}
	db, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "profile-origin.db"), sqlite.Options{})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := sqlite.Migrate(ctx, db, before); err != nil {
		t.Fatalf("Migrate the earlier steps: %v", err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	if err := turnstore.NewSessionService(db).Create(ctx, &turn.Session{ID: "sess-1", OwnerID: "owner-1", CreatedAt: now, UpdatedAt: now, Metadata: []byte(`{}`)}); err != nil {
		t.Fatalf("create the session: %v", err)
	}
	if _, err := turnstore.NewEventStore(db).AppendSequenced(ctx, "sess-1", &turn.Event{
		ID: "evt-1", SessionID: "sess-1", TurnID: "t1", InvocationID: "inv-1", Author: "owner-1",
		Kind: "message.completed", SchemaVersion: 1, Payload: []byte(`{"text":"i prefer go"}`), CreatedAt: now,
	}); err != nil {
		t.Fatalf("append the event: %v", err)
	}
	store, err := profile.NewSQLiteStore(db)
	if err != nil {
		t.Fatalf("NewSQLiteStore: %v", err)
	}
	verified := profile.Fact{
		ID: "pf-owner", OwnerID: "owner-1", Category: "tool", Key: "editor", Value: "neovim", ValueDigest: "d-owner",
		Status: profile.StatusActive, Origin: profile.OriginOwner, OwnerVerified: true, Confidence: 1,
		ConfidencePolicy: profile.ConfidencePolicyV1, CreatedAt: now, UpdatedAt: now,
	}
	learned := profile.Fact{
		ID: "pf-learned", OwnerID: "owner-1", Category: "tool", Key: "editor", Value: "vim", ValueDigest: "d-learned",
		Status: profile.StatusCandidate, Origin: profile.OriginDerived, Confidence: 0.6, ConflictsWithID: "pf-owner",
		ConfidencePolicy: profile.ConfidencePolicyV1, CreatedAt: now, UpdatedAt: now,
	}
	for _, fact := range []*profile.Fact{&verified, &learned} {
		if err := store.InsertFact(ctx, fact); err != nil {
			t.Fatalf("seed %s: %v", fact.ID, err)
		}
	}
	if added, err := store.AddEvidence(ctx, "pf-learned", &profile.Evidence{SourceEventID: "evt-1", SourceDigest: "digest-1", Provider: "p", Model: "m", PromptVersion: "v1", ObservedAt: now}); err != nil || !added {
		t.Fatalf("seed the evidence: %v, %v", added, err)
	}

	if err := sqlite.Migrate(ctx, db, history); err != nil {
		t.Fatalf("Migrate the step: %v", err)
	}

	for _, want := range []*profile.Fact{&verified, &learned} {
		got, found, err := store.GetFact(ctx, want.ID)
		if err != nil || !found || got.Value != want.Value || got.Status != want.Status || got.Origin != want.Origin ||
			got.OwnerVerified != want.OwnerVerified || got.ConflictsWithID != want.ConflictsWithID || !got.CreatedAt.Equal(want.CreatedAt) {
			t.Fatalf("fact %s = %+v, %v, %v, want %+v", want.ID, got, found, err, want)
		}
	}
	evidence, err := store.ListEvidence(ctx, "pf-learned")
	if err != nil || len(evidence) != 1 || evidence[0].SourceEventID != "evt-1" || evidence[0].SourceDigest != "digest-1" {
		t.Fatalf("evidence = %+v, %v, want the row kept", evidence, err)
	}
	hits, err := store.SearchFacts(ctx, "owner-1", "", "neovim", false, 10, now)
	if err != nil || len(hits) != 1 || hits[0].ID != "pf-owner" {
		t.Fatalf("search = %+v, %v, want the index rebuilt", hits, err)
	}

	agent := profile.Fact{
		ID: "pf-agent", OwnerID: "owner-1", Category: "habit", Key: "standup", Value: "09:30", ValueDigest: "d-agent",
		Status: profile.StatusCandidate, Origin: profile.OriginAgent, Confidence: 0.5,
		ConfidencePolicy: profile.ConfidencePolicyV1, CreatedAt: now, UpdatedAt: now,
	}
	if err := store.InsertFact(ctx, &agent); err != nil {
		t.Fatalf("insert an agent fact: %v", err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE profile_fact SET origin = 'model' WHERE id = 'pf-agent'`); !fault.HasCode(sqlite.Classify(err), sqlite.ErrorCodeConstraintCheck) {
		t.Fatalf("an unknown origin = %v, want %s", err, sqlite.ErrorCodeConstraintCheck)
	}
	if _, err := db.ExecContext(ctx, `DELETE FROM profile_fact WHERE id = 'pf-learned'`); err != nil {
		t.Fatalf("delete a fact: %v", err)
	}
	if remaining, err := store.ListEvidence(ctx, "pf-learned"); err != nil || len(remaining) != 0 {
		t.Fatalf("evidence after the delete = %+v, %v, want it cascaded", remaining, err)
	}
}
