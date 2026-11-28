package profiletest

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/modules/profile"
	"github.com/anggasct/aura/internal/platform/principal"
	turnstore "github.com/anggasct/aura/internal/platform/turn/store"
)

var SeededAt = time.Date(2026, 8, 12, 9, 0, 0, 0, time.UTC)

const (
	VerifiedCategory = "language"
	VerifiedKey      = "backend"
	VerifiedValue    = "Go"

	CandidateCategory = "tool"
	CandidateKey      = "editor"
	CandidateValue    = "neovim"
)

type Rig struct {
	Service *profile.Service
	db      *sql.DB
}

func NewRig(tb testing.TB, db *sql.DB) *Rig {
	tb.Helper()
	store, err := profile.NewSQLiteStore(db)
	if err != nil {
		tb.Fatalf("profile.NewSQLiteStore: %v", err)
	}
	sink, err := profile.NewActionSink(turnstore.NewSessionService(db), turnstore.NewEventStore(db))
	if err != nil {
		tb.Fatalf("profile.NewActionSink: %v", err)
	}
	service, err := profile.NewService(store, profile.ServiceOptions{MinConfidence: 0.99, Actions: sink})
	if err != nil {
		tb.Fatalf("profile.NewService: %v", err)
	}
	return &Rig{Service: service, db: db}
}

func VerifiedFactID() string {
	return profile.FactID(principal.OwnerID, VerifiedCategory, VerifiedKey, profile.ValueDigest(VerifiedValue))
}

func CandidateFactID() string {
	return profile.FactID(principal.OwnerID, CandidateCategory, CandidateKey, profile.ValueDigest(CandidateValue))
}

func (r *Rig) SeedVerified(ctx context.Context) error {
	if r.exists(ctx, VerifiedFactID()) {
		return r.restore(ctx, VerifiedFactID())
	}
	_, err := r.Service.SetFact(ctx, principal.OwnerID, VerifiedCategory, VerifiedKey, VerifiedValue, nil, SeededAt)
	return err
}

func (r *Rig) SeedCandidate(ctx context.Context) error {
	if r.exists(ctx, CandidateFactID()) {
		return r.restore(ctx, CandidateFactID())
	}
	_, err := r.Service.ProposeAgentFact(ctx, &profile.AgentProposal{
		OwnerID: principal.OwnerID, Category: CandidateCategory, Key: CandidateKey, Value: CandidateValue,
	}, SeededAt)
	return err
}

func (r *Rig) exists(ctx context.Context, id string) bool {
	_, err := r.Service.GetFact(ctx, principal.OwnerID, id)
	return err == nil
}

func (r *Rig) restore(ctx context.Context, id string) error {
	_, err := r.db.ExecContext(ctx, `UPDATE profile_fact SET status = ? WHERE id = ?`, profile.StatusActive, id)
	return err
}
