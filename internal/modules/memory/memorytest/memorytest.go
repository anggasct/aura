package memorytest

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/modules/memory"
	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/principal"
	"github.com/anggasct/aura/internal/platform/secret"
	"github.com/anggasct/aura/internal/platform/turn"
	turnstore "github.com/anggasct/aura/internal/platform/turn/store"
)

const (
	MainPersona  = "main"
	AlphaPersona = "alpha"
	BetaPersona  = "beta"
	AlphaSession = "session-alpha"
	BetaSession  = "session-beta"

	FirstSession  = "session-1"
	SecondSession = "memory-session-2"

	ExpiredSummary = "mem-s1"
)

var seededAt = time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)

type Rig struct {
	Service *memory.Service
	Store   *memory.SQLiteStore
	db      *sql.DB
}

func NewRig(tb testing.TB, db *sql.DB) *Rig {
	tb.Helper()
	store, err := memory.NewSQLiteStore(db, turnstore.NewSessionService(db))
	if err != nil {
		tb.Fatalf("memory.NewSQLiteStore: %v", err)
	}
	service, err := memory.NewService(store, secret.New(), &memory.ServiceOptions{})
	if err != nil {
		tb.Fatalf("memory.NewService: %v", err)
	}
	return &Rig{Service: service, Store: store, db: db}
}

type seedDocument struct {
	id, session, kind, content string
	sequence                   int64
	expires                    *time.Time
}

func (r *Rig) Seed(ctx context.Context) error {
	err := turnstore.NewSessionService(r.db).Create(ctx, &turn.Session{
		ID: SecondSession, OwnerID: principal.OwnerID, CreatedAt: seededAt, UpdatedAt: seededAt, Metadata: []byte(`{}`),
	})
	if err != nil && !fault.HasCode(err, turnstore.ErrorCodeSessionIDConflict) {
		return err
	}
	expired := seededAt.Add(-time.Hour)
	clean := false
	for _, seed := range []seedDocument{
		{id: "mem-a1", session: FirstSession, kind: memory.KindEventText, content: "alpha recording one", sequence: 1},
		{id: "mem-a2", session: FirstSession, kind: memory.KindEventText, content: "alpha recording two", sequence: 2},
		{id: "mem-b1", session: SecondSession, kind: memory.KindEventText, content: "beta recording one", sequence: 1},
		{id: ExpiredSummary, session: SecondSession, kind: memory.KindSummary, content: "old summary content", sequence: 2, expires: &expired},
	} {
		if _, err := r.Store.UpsertDocument(ctx, &memory.StoredDocument{
			ID: seed.id, OwnerID: principal.OwnerID, SessionID: seed.session, Kind: seed.kind,
			FromSequence: seed.sequence, ToSequence: seed.sequence, Content: seed.content,
			TrustLabel: "owner_input", Tainted: &clean, CreatedAt: seededAt, ExpiresAt: seed.expires,
		}); err != nil {
			return err
		}
	}
	return nil
}

type Document struct {
	ID       string
	Session  string
	Content  string
	Sequence int64
}

func (r *Rig) Add(ctx context.Context, document *Document) error {
	clean := false
	_, err := r.Store.UpsertDocument(ctx, &memory.StoredDocument{
		ID: document.ID, OwnerID: principal.OwnerID, SessionID: document.Session, Kind: memory.KindEventText,
		FromSequence: document.Sequence, ToSequence: document.Sequence, Content: document.Content,
		TrustLabel: "owner_input", Tainted: &clean, CreatedAt: seededAt,
	})
	return err
}

func (r *Rig) AddExpiredSummary(ctx context.Context, document *Document) error {
	clean := false
	expired := seededAt.Add(-time.Minute)
	_, err := r.Store.UpsertDocument(ctx, &memory.StoredDocument{
		ID: document.ID, OwnerID: principal.OwnerID, SessionID: document.Session, Kind: memory.KindSummary,
		FromSequence: document.Sequence, ToSequence: document.Sequence, Content: document.Content,
		TrustLabel: "owner_input", Tainted: &clean, CreatedAt: seededAt, ExpiresAt: &expired,
	})
	return err
}

type Scopes map[string][]string

func (s Scopes) RecallScope(agentID string) ([]string, bool) {
	readable, known := s[agentID]
	return readable, known
}

func PersonaScopes() Scopes {
	return Scopes{MainPersona: {MainPersona}, AlphaPersona: {AlphaPersona}, BetaPersona: {BetaPersona}}
}

func (r *Rig) SeedPersonas(ctx context.Context) error {
	for _, session := range []struct{ id, persona string }{{AlphaSession, AlphaPersona}, {BetaSession, BetaPersona}} {
		err := turnstore.NewSessionService(r.db).Create(ctx, &turn.Session{
			ID: session.id, OwnerID: principal.OwnerID, AgentID: session.persona, CreatedAt: seededAt, UpdatedAt: seededAt, Metadata: []byte(`{}`),
		})
		if err != nil && !fault.HasCode(err, turnstore.ErrorCodeSessionIDConflict) {
			return err
		}
	}
	for _, document := range []Document{
		{ID: "mem-alpha-1", Session: AlphaSession, Content: "alpha notes about trains", Sequence: 1},
		{ID: "mem-beta-1", Session: BetaSession, Content: "beta notes about trains", Sequence: 1},
	} {
		if err := r.Add(ctx, &document); err != nil {
			return err
		}
	}
	for _, document := range []Document{
		{ID: "mem-alpha-old", Session: AlphaSession, Content: "alpha old summary", Sequence: 2},
		{ID: "mem-beta-old", Session: BetaSession, Content: "beta old summary", Sequence: 2},
	} {
		if err := r.AddExpiredSummary(ctx, &document); err != nil {
			return err
		}
	}
	return nil
}
