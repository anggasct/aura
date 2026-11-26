package personaeval

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/sqlite"
)

const (
	LayerPolicy   = "policy"
	LayerBehavior = "behavior"
)

type Result struct {
	ID          string
	AgentID     string
	Digest      string
	Layer       string
	Passed      bool
	Score       *float64
	CasesTotal  int
	CasesFailed int
	FailedCases []string
	CreatedAt   time.Time
}

type Store interface {
	Record(ctx context.Context, result *Result) (Result, error)
	Latest(ctx context.Context, agentID, digest, layer string) (Result, bool, error)
	List(ctx context.Context, agentID string, limit int) ([]Result, error)
	EverPassed(ctx context.Context, agentID, digest, layer string) (bool, error)
}

type rowScanner interface {
	Scan(dest ...any) error
}

type SQLiteStore struct {
	db *sql.DB
}

func NewSQLiteStore(db *sql.DB) (*SQLiteStore, error) {
	if db == nil {
		return nil, fault.New(ErrorCodeInvalidArgument, "persona eval store database must not be nil")
	}
	return &SQLiteStore{db: db}, nil
}

const personaEvalColumns = `id, agent_id, fingerprint, layer, passed, score, cases_total, cases_failed, detail_json, created_at`

func (s *SQLiteStore) Record(ctx context.Context, result *Result) (Result, error) {
	if s == nil || s.db == nil {
		return Result{}, fault.New(ErrorCodeInvalidArgument, "persona eval store database must not be nil")
	}
	if result == nil || result.AgentID == "" || result.Digest == "" || (result.Layer != LayerPolicy && result.Layer != LayerBehavior) {
		return Result{}, fault.New(ErrorCodeInvalidArgument, "record persona eval: agent, digest and a known layer are required")
	}
	recorded := *result
	if recorded.ID == "" {
		var raw [12]byte
		if _, err := rand.Read(raw[:]); err != nil {
			return Result{}, fmt.Errorf("record persona eval: id: %w", err)
		}
		recorded.ID = "eval_" + hex.EncodeToString(raw[:])
	}
	if recorded.CreatedAt.IsZero() {
		recorded.CreatedAt = time.Now().UTC()
	}
	failed := recorded.FailedCases
	if failed == nil {
		failed = []string{}
	}
	detail, err := json.Marshal(failed)
	if err != nil {
		return Result{}, fmt.Errorf("record persona eval: detail: %w", err)
	}
	passed := 0
	if recorded.Passed {
		passed = 1
	}
	if _, err := sqlite.Conn(ctx, s.db).ExecContext(ctx, `INSERT INTO persona_eval_result (`+personaEvalColumns+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		recorded.ID, recorded.AgentID, recorded.Digest, recorded.Layer, passed, recorded.Score, recorded.CasesTotal, recorded.CasesFailed, string(detail), sqlite.FormatTime(recorded.CreatedAt)); err != nil {
		return Result{}, fmt.Errorf("record persona eval: %w", sqlite.Classify(err))
	}
	return recorded, nil
}

func (s *SQLiteStore) Latest(ctx context.Context, agentID, digest, layer string) (Result, bool, error) {
	if s == nil || s.db == nil {
		return Result{}, false, fault.New(ErrorCodeInvalidArgument, "persona eval store database must not be nil")
	}
	row := sqlite.Conn(ctx, s.db).QueryRowContext(ctx, `SELECT `+personaEvalColumns+` FROM persona_eval_result
		WHERE agent_id = ? AND fingerprint = ? AND layer = ? ORDER BY created_at DESC, id DESC LIMIT 1`, agentID, digest, layer)
	result, err := scanResult(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Result{}, false, nil
	}
	if err != nil {
		return Result{}, false, sqlite.Classify(err)
	}
	return result, true, nil
}

func (s *SQLiteStore) EverPassed(ctx context.Context, agentID, digest, layer string) (bool, error) {
	if s == nil || s.db == nil {
		return false, fault.New(ErrorCodeInvalidArgument, "persona eval store database must not be nil")
	}
	var passed int
	err := sqlite.Conn(ctx, s.db).QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM persona_eval_result WHERE agent_id = ? AND fingerprint = ? AND layer = ? AND passed = 1)`,
		agentID, digest, layer).Scan(&passed)
	if err != nil {
		return false, fmt.Errorf("check persona eval history: %w", sqlite.Classify(err))
	}
	return passed == 1, nil
}

func (s *SQLiteStore) List(ctx context.Context, agentID string, limit int) ([]Result, error) {
	if s == nil || s.db == nil {
		return nil, fault.New(ErrorCodeInvalidArgument, "persona eval store database must not be nil")
	}
	if limit <= 0 {
		limit = 20
	}
	rows, err := sqlite.Conn(ctx, s.db).QueryContext(ctx, `SELECT `+personaEvalColumns+` FROM persona_eval_result WHERE agent_id = ? ORDER BY created_at DESC, id DESC LIMIT ?`, agentID, limit)
	if err != nil {
		return nil, fmt.Errorf("list persona evals: %w", sqlite.Classify(err))
	}
	defer func() { _ = rows.Close() }()
	var results []Result
	for rows.Next() {
		result, err := scanResult(rows)
		if err != nil {
			return nil, err
		}
		results = append(results, result)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list persona evals: %w", sqlite.Classify(err))
	}
	return results, nil
}

func scanResult(row rowScanner) (Result, error) {
	var result Result
	var passed int
	var score sql.NullFloat64
	var detail, createdAt string
	if err := row.Scan(&result.ID, &result.AgentID, &result.Digest, &result.Layer, &passed, &score, &result.CasesTotal, &result.CasesFailed, &detail, &createdAt); err != nil {
		return Result{}, err
	}
	result.Passed = passed == 1
	if score.Valid {
		value := score.Float64
		result.Score = &value
	}
	if err := json.Unmarshal([]byte(detail), &result.FailedCases); err != nil {
		return Result{}, fmt.Errorf("parse persona eval detail: %w", err)
	}
	created, err := sqlite.ParseTime(createdAt)
	if err != nil {
		return Result{}, fmt.Errorf("parse persona eval created_at: %w", err)
	}
	result.CreatedAt = created
	return result, nil
}
