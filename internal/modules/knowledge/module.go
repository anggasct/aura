package knowledge

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/anggasct/aura/internal/platform/egress"
	"github.com/anggasct/aura/internal/platform/health"
	"github.com/anggasct/aura/internal/platform/sqlite"
	"github.com/anggasct/aura/internal/platform/toolbroker"
	"github.com/anggasct/aura/internal/platform/turn"
)

const (
	readPoolConns   = 4
	loaderPoolConns = 1
)

type ModuleDeps struct {
	Config       *Config
	DBPath       string
	Scopes       []PersonaScope
	Secrets      SecretSource
	Resolver     egress.Resolver
	CheckTimeout time.Duration
	Logger       *slog.Logger
}

type Module struct {
	cfg          *Config
	scopes       []PersonaScope
	dbPath       string
	checkTimeout time.Duration

	pool      *sql.DB
	loader    *sql.DB
	vectors   *MemoryIndex
	retriever *Retriever
	provider  *ContextProvider

	closeOnce sync.Once
	closeErr  error
}

func New(ctx context.Context, deps *ModuleDeps) (*Module, error) {
	if deps == nil {
		return nil, Errorf(ErrorCodeInvalidArgument, "knowledge module dependencies must not be nil")
	}
	if deps.DBPath == "" {
		return nil, Errorf(ErrorCodeInvalidArgument, "knowledge database path must not be empty")
	}
	if len(deps.Scopes) > 0 && deps.Secrets == nil {
		return nil, Errorf(ErrorCodeInvalidArgument, "knowledge secret source must not be nil")
	}
	logger := deps.Logger
	if logger == nil {
		logger = slog.Default()
	}

	scopes := make([]PersonaScope, len(deps.Scopes))
	for i := range deps.Scopes {
		scopes[i] = PersonaScope{
			Persona:     deps.Scopes[i].Persona,
			Class:       deps.Scopes[i].Class,
			Collections: slices.Clone(deps.Scopes[i].Collections),
		}
	}

	mod := &Module{
		cfg:          deps.Config,
		scopes:       scopes,
		dbPath:       deps.DBPath,
		checkTimeout: deps.CheckTimeout,
	}
	if len(scopes) == 0 {
		return mod, nil
	}

	pool, err := sqlite.OpenReadOnly(ctx, deps.DBPath)
	if err != nil {
		return nil, err
	}
	pool.SetMaxOpenConns(readPoolConns)
	mod.pool = pool

	var retrieverOpts []RetrieverOption
	wiring, err := NewEmbeddingWiring(deps.Config, deps.Resolver)
	if err != nil {
		_ = mod.Close()
		return nil, err
	}
	if wiring.Settings != nil && wiring.Embedder != nil {
		reader, err := NewSQLiteVectorSource(pool)
		if err != nil {
			_ = mod.Close()
			return nil, err
		}
		loader, err := sqlite.OpenReadOnly(ctx, deps.DBPath)
		if err != nil {
			_ = mod.Close()
			return nil, err
		}
		loader.SetMaxOpenConns(loaderPoolConns)
		mod.loader = loader

		loaderSource, err := NewSQLiteVectorSource(loader)
		if err != nil {
			_ = mod.Close()
			return nil, err
		}
		vectors, err := NewMemoryIndex(&MemoryIndexConfig{
			Reader:      reader,
			Loader:      loaderSource,
			Fingerprint: wiring.Settings.Fingerprint,
			Dimensions:  wiring.Settings.Dimensions,
			MaxMemory:   wiring.Settings.MaxMemory,
		})
		if err != nil {
			_ = mod.Close()
			return nil, err
		}
		mod.vectors = vectors
		retrieverOpts = append(retrieverOpts, WithVectors(&VectorConfig{
			Index:    vectors,
			Embedder: wiring.Embedder,
			Settings: wiring.Settings,
		}))
	}

	index, err := NewSQLiteIndex(pool)
	if err != nil {
		_ = mod.Close()
		return nil, err
	}
	retriever, err := NewRetriever(index, deps.Secrets, retrieverOpts...)
	if err != nil {
		_ = mod.Close()
		return nil, err
	}
	provider, err := NewContextProvider(retriever, logger)
	if err != nil {
		_ = mod.Close()
		return nil, err
	}
	mod.retriever = retriever
	mod.provider = provider
	return mod, nil
}

func (m *Module) Tools() []toolbroker.ToolRegistration {
	if m == nil || m.retriever == nil {
		return nil
	}
	return []toolbroker.ToolRegistration{SearchToolRegistration(m.retriever)}
}

func (m *Module) TurnContext(ctx context.Context, req *turn.TurnRequest) ([]turn.ContextBlock, error) {
	if m == nil || m.provider == nil {
		return nil, nil
	}
	return m.provider.TurnContext(ctx, req)
}

func (m *Module) Required(req *turn.TurnRequest) bool {
	if m == nil || m.provider == nil {
		return false
	}
	return m.provider.Required(req)
}

func (m *Module) HealthChecks() []health.RegisteredCheck {
	if m == nil {
		return nil
	}
	return HealthChecks(m.cfg, m.scopes, m.dbPath, m.checkTimeout)
}

func (m *Module) VectorLoader() func(context.Context) {
	if m == nil || m.vectors == nil {
		return nil
	}
	return m.vectors.Run
}

func (m *Module) VectorIndex() *MemoryIndex {
	if m == nil {
		return nil
	}
	return m.vectors
}

func (m *Module) Retriever() *Retriever {
	if m == nil {
		return nil
	}
	return m.retriever
}

func (m *Module) Close() error {
	if m == nil {
		return nil
	}
	m.closeOnce.Do(func() {
		var errs []error
		if m.loader != nil {
			errs = append(errs, m.loader.Close())
		}
		if m.pool != nil {
			errs = append(errs, m.pool.Close())
		}
		m.closeErr = errors.Join(errs...)
	})
	return m.closeErr
}

var (
	_ toolbroker.ToolProvider = (*Module)(nil)
	_ turn.ContextProvider    = (*Module)(nil)
	_ health.HealthProvider   = (*Module)(nil)
	_ io.Closer               = (*Module)(nil)
)
