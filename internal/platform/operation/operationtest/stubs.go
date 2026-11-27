package operationtest

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/anggasct/aura/internal/platform/operation"
	"github.com/anggasct/aura/internal/platform/sqlite"
)

const ledgerSQL = `
CREATE TABLE operation_stub_ledger (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    operation TEXT NOT NULL,
    subject TEXT NOT NULL
)`

type Hook func(ctx context.Context, call *operation.Call) error

type Stubs struct {
	db       *sql.DB
	mu       sync.Mutex
	handled  map[string]int
	previews atomic.Int32
	limited  atomic.Int32
	before   map[string]Hook
	after    map[string]Hook
	failWith map[string]error
	skipped  map[string]bool
}

func NewStubs(db *sql.DB) *Stubs {
	return &Stubs{
		db:       db,
		handled:  map[string]int{},
		before:   map[string]Hook{},
		after:    map[string]Hook{},
		failWith: map[string]error{},
		skipped:  map[string]bool{},
	}
}

func (s *Stubs) BeforeCommit(id string, hook Hook) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.before[id] = hook
}

func (s *Stubs) AfterCommit(id string, hook Hook) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.after[id] = hook
}

func (s *Stubs) FailWith(id string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failWith[id] = err
}

func (s *Stubs) SkipCommit(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.skipped[id] = true
}

func (s *Stubs) Handled(id string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.handled[id]
}

func (s *Stubs) Previews() int {
	return int(s.previews.Load())
}

func (s *Stubs) RateLimited() int {
	return int(s.limited.Load())
}

func (s *Stubs) Ledger(ctx context.Context, id string) (int, error) {
	var count int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM operation_stub_ledger WHERE operation = ?`, id).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("operationtest: count the ledger of %s: %w", id, err)
	}
	return count, nil
}

func (s *Stubs) Bindings() []operation.Binding {
	ids := []string{Read, Change, Notice, Fresh, Grant, Delete, External, Idempotent, Host, OwnerOnly}
	bindings := make([]operation.Binding, 0, len(ids))
	for _, id := range ids {
		binding := operation.Binding{ID: id, Handle: s.handler(id)}
		switch id {
		case Grant, Delete:
			binding.Preview = s.previewer()
		case Notice:
			binding.RateLimit = s.rateLimit
		}
		bindings = append(bindings, binding)
	}
	return bindings
}

func (s *Stubs) rateLimit(context.Context, *operation.Call) error {
	s.limited.Add(1)
	return nil
}

func (s *Stubs) previewer() operation.Previewer {
	return func(_ context.Context, call *operation.Call) (operation.Preview, error) {
		s.previews.Add(1)
		var input struct {
			Subject string `json:"subject"`
		}
		if err := json.Unmarshal(call.Input, &input); err != nil {
			return operation.Preview{}, err
		}
		return operation.Preview{Lines: []string{"subject: " + input.Subject, "cannot be undone"}, Digest: "set-of-" + input.Subject}, nil
	}
}

func (s *Stubs) count(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.handled[id]++
}

type settings struct {
	before, after Hook
	failure       error
	skip          bool
}

func (s *Stubs) lookup(id string) settings {
	s.mu.Lock()
	defer s.mu.Unlock()
	return settings{before: s.before[id], after: s.after[id], failure: s.failWith[id], skip: s.skipped[id]}
}

func (s *Stubs) handler(id string) operation.Handler {
	return func(ctx context.Context, call *operation.Call) (operation.Result, error) {
		s.count(id)
		configured := s.lookup(id)
		if configured.before != nil {
			if err := configured.before(ctx, call); err != nil {
				return operation.Result{}, err
			}
		}
		if configured.failure != nil {
			return operation.Result{}, configured.failure
		}
		data := json.RawMessage(fmt.Sprintf(`{"done":true,"operation":%q}`, id))
		if configured.skip || !transactional(id) {
			return operation.Result{Data: data}, nil
		}
		var input struct {
			Subject string `json:"subject"`
		}
		if err := json.Unmarshal(call.Input, &input); err != nil {
			return operation.Result{}, err
		}
		result, err := call.Commit(ctx, func(ctx context.Context) (operation.Result, error) {
			_, err := sqlite.Conn(ctx, s.db).ExecContext(ctx,
				`INSERT INTO operation_stub_ledger (operation, subject) VALUES (?, ?)`, id, input.Subject)
			return operation.Result{Data: data}, err
		})
		if err != nil {
			return operation.Result{}, err
		}
		if configured.after != nil {
			if err := configured.after(ctx, call); err != nil {
				return operation.Result{}, err
			}
		}
		return result, nil
	}
}

func transactional(id string) bool {
	switch id {
	case Change, Fresh, Grant, Delete:
		return true
	}
	return false
}
