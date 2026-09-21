package cli

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/anggasct/aura/internal/child"
	"github.com/anggasct/aura/internal/store"
)

type childRegistry struct {
	db *sql.DB
}

func newChildRegistry(db *sql.DB) *childRegistry {
	return &childRegistry{db: db}
}

func (r *childRegistry) Spawn(ctx context.Context, spec *child.Spec, now time.Time) (child.Spawn, bool, error) {
	if spec == nil {
		return child.Spawn{}, false, errors.New("cli: child spec must not be nil")
	}
	grants, err := json.Marshal(spec.RequestedGrants)
	if err != nil {
		return child.Spawn{}, false, fmt.Errorf("cli: encode child grants: %w", err)
	}
	budget, err := json.Marshal(spec.Budget)
	if err != nil {
		return child.Spawn{}, false, fmt.Errorf("cli: encode child budget: %w", err)
	}
	children := store.NewChildStore(r.db)
	existing, found, err := children.GetRunByInvocation(ctx, spec.ParentInvocation, spec.IdempotencyKey)
	if err != nil {
		return child.Spawn{}, false, err
	}
	if found {
		if existing.ContextDigest != spec.ContextDigest {
			return child.Spawn{}, false, fmt.Errorf("cli: child invocation conflicts with an existing child: %w", childConflictSentinel())
		}
		return childSpawnFromStore(&existing), false, nil
	}
	deadline := now.Add(spec.Budget.Timeout)
	parentDepth, err := r.Depth(ctx, spec.ParentSessionID)
	if err != nil {
		return child.Spawn{}, false, err
	}
	if parentDepth+1 > 1 {
		return child.Spawn{}, false, child.Errorf(child.ErrorCodeChildDepthExceeded, "child depth exceeds the maximum of one")
	}
	childDepth := parentDepth + 1
	run := &store.ChildRun{
		ID: spec.ID, IdempotencyKey: spec.IdempotencyKey,
		ParentSessionID: spec.ParentSessionID, ParentTurnID: spec.ParentTurnID,
		ParentInvocation: spec.ParentInvocation, ChildSessionID: spec.ChildSessionID,
		Depth:      childDepth,
		DurableKey: child.DurableChildKey(spec.ID), ContextDigest: spec.ContextDigest,
		GrantsJSON: string(grants), BudgetJSON: string(budget),
		State: child.StatusQueued, Deadline: deadline, CreatedAt: now, UpdatedAt: now,
	}
	if err := children.InsertRun(ctx, run); err != nil {
		if code, ok := store.CodeOf(err); ok && code == store.ErrorCodeChildConflict {
			existing, found, readErr := children.GetRunByInvocation(ctx, spec.ParentInvocation, spec.IdempotencyKey)
			if readErr != nil {
				return child.Spawn{}, false, readErr
			}
			if found && existing.ContextDigest == spec.ContextDigest {
				return childSpawnFromStore(&existing), false, nil
			}
			return child.Spawn{}, false, fmt.Errorf("cli: child run conflicts under concurrency: %w", childConflictSentinel())
		}
		return child.Spawn{}, false, err
	}
	return child.Spawn{
		ID: run.ID, SessionID: run.ChildSessionID, Depth: childDepth,
		Grants: spec.RequestedGrants, DurableKey: run.DurableKey,
		ContextDigest: run.ContextDigest, Deadline: deadline, CreatedAt: now,
	}, true, nil
}

func (r *childRegistry) Get(ctx context.Context, id string) (child.Spawn, bool, error) {
	children := store.NewChildStore(r.db)
	run, found, err := children.GetRun(ctx, id)
	if err != nil || !found {
		return child.Spawn{}, found, err
	}
	return childSpawnFromStore(&run), true, nil
}

func (r *childRegistry) Depth(ctx context.Context, sessionID string) (int, error) {
	if strings.TrimSpace(sessionID) == "" {
		return 0, errors.New("cli: child session must not be empty")
	}
	if r.db == nil {
		return 0, errors.New("cli: child store must not be nil")
	}
	var one int
	if err := r.db.QueryRowContext(ctx, `SELECT 1 FROM child_run WHERE child_session_id = ? LIMIT 1`, sessionID).Scan(&one); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, nil
		}
		return 0, err
	}
	return 1, nil
}

func childSpawnFromStore(run *store.ChildRun) child.Spawn {
	var grants []child.Grant
	if err := json.Unmarshal([]byte(run.GrantsJSON), &grants); err != nil {
		grants = nil
	}
	return child.Spawn{
		ID: run.ID, SessionID: run.ChildSessionID, Depth: run.Depth,
		Grants: grants, DurableKey: run.DurableKey,
		ContextDigest: run.ContextDigest, Deadline: run.Deadline, CreatedAt: run.CreatedAt,
	}
}

func childConflictSentinel() error {
	return child.Errorf(child.ErrorCodeChildConflict, "child run conflicts")
}
