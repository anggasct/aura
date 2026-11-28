package knowledgetest

import (
	"bytes"
	"context"
	"database/sql"
	"io"
	"net/http"
	"sync"
	"testing"

	"github.com/anggasct/aura/internal/modules/knowledge"
)

const (
	Collection = "support"
	PageURL    = "https://docs.example.test/refunds"
	PageText   = "# Refunds\nRefunds are issued within thirty days.\n\n# Shipping\nShipping takes five days.\n"
)

type Site struct {
	mu        sync.Mutex
	mediaType string
	body      []byte
	fetched   int
}

func NewSite(mediaType, body string) *Site {
	return &Site{mediaType: mediaType, body: []byte(body)}
}

func (s *Site) Serve(mediaType, body string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.mediaType, s.body = mediaType, []byte(body)
}

func (s *Site) Fetches() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.fetched
}

func (s *Site) Do(req *http.Request) (*http.Response, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fetched++
	header := http.Header{}
	header.Set("Content-Type", s.mediaType)
	return &http.Response{
		StatusCode:    http.StatusOK,
		Header:        header,
		ContentLength: int64(len(s.body)),
		Body:          io.NopCloser(bytes.NewReader(s.body)),
		Request:       req,
	}, nil
}

type Provider struct {
	mu   sync.Mutex
	sent int
}

func (p *Provider) Embed(_ context.Context, _ knowledge.EmbedRole, texts []string) ([][]float32, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.sent += len(texts)
	vectors := make([][]float32, len(texts))
	for i := range texts {
		vectors[i] = []float32{3, 4, 0}
	}
	return vectors, nil
}

func (p *Provider) Sent() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.sent
}

type Ledger struct {
	mu      sync.Mutex
	actions []knowledge.OwnerAction
}

func (l *Ledger) RecordOwnerAction(_ context.Context, action *knowledge.OwnerAction) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.actions = append(l.actions, *action)
	return nil
}

func (l *Ledger) Actions() []knowledge.OwnerAction {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]knowledge.OwnerAction(nil), l.actions...)
}

type clearScreen struct{}

func (clearScreen) Contains(string) bool { return false }

type Vault struct{}

func (Vault) Snapshot(context.Context) (knowledge.SecretScreen, error) { return clearScreen{}, nil }

func Settings() *knowledge.EmbeddingSettings {
	return &knowledge.EmbeddingSettings{
		Fingerprint: "fingerprint-test", Destination: "embeddings.example.test:443", Model: "test-model", Dimensions: 3, BatchSize: 8,
	}
}

func identityReducer(_ context.Context, source []byte, _ int64) (string, bool, error) {
	return string(source), false, nil
}

type Rig struct {
	Service  *knowledge.Service
	Store    *knowledge.SQLiteStore
	Site     *Site
	Provider *Provider
	Ledger   *Ledger
}

func NewRig(tb testing.TB, db *sql.DB) *Rig {
	tb.Helper()
	store, err := knowledge.NewSQLiteStore(db)
	if err != nil {
		tb.Fatalf("knowledge.NewSQLiteStore: %v", err)
	}
	rig := &Rig{Store: store, Site: NewSite("text/markdown", PageText), Provider: &Provider{}, Ledger: &Ledger{}}
	rig.Service, err = knowledge.NewService(&knowledge.Deps{
		Store:     store,
		Secrets:   Vault{},
		Reducer:   identityReducer,
		HTTP:      rig.Site,
		Actions:   rig.Ledger,
		Vectors:   store,
		Embedder:  rig.Provider,
		Embedding: Settings(),
	})
	if err != nil {
		tb.Fatalf("knowledge.NewService: %v", err)
	}
	return rig
}
