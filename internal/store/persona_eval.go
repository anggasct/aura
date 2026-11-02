package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

const (
	EvalLayerPolicy   = "policy"
	EvalLayerBehavior = "behavior"
)

type PersonaEvalResult struct {
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

type PersonaEvalStore interface {
	Record(ctx context.Context, result *PersonaEvalResult) (PersonaEvalResult, error)
	Latest(ctx context.Context, agentID, digest, layer string) (PersonaEvalResult, bool, error)
	List(ctx context.Context, agentID string, limit int) ([]PersonaEvalResult, error)
	EverPassed(ctx context.Context, agentID, digest, layer string) (bool, error)
}

type sqlitePersonaEvalStore struct {
	db *sql.DB
}

func NewPersonaEvalStore(db *sql.DB) PersonaEvalStore {
	return &sqlitePersonaEvalStore{db: db}
}

const personaEvalColumns = `id, agent_id, fingerprint, layer, passed, score, cases_total, cases_failed, detail_json, created_at`

func (s *sqlitePersonaEvalStore) Record(ctx context.Context, result *PersonaEvalResult) (PersonaEvalResult, error) {
	if result == nil || result.AgentID == "" || result.Digest == "" || (result.Layer != EvalLayerPolicy && result.Layer != EvalLayerBehavior) {
		return PersonaEvalResult{}, errors.New("record persona eval: agent, digest and a known layer are required")
	}
	recorded := *result
	if recorded.ID == "" {
		var raw [12]byte
		if _, err := rand.Read(raw[:]); err != nil {
			return PersonaEvalResult{}, fmt.Errorf("record persona eval: id: %w", err)
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
		return PersonaEvalResult{}, fmt.Errorf("record persona eval: detail: %w", err)
	}
	passed := 0
	if recorded.Passed {
		passed = 1
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO persona_eval_result (`+personaEvalColumns+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		recorded.ID, recorded.AgentID, recorded.Digest, recorded.Layer, passed, recorded.Score, recorded.CasesTotal, recorded.CasesFailed, string(detail), formatTime(recorded.CreatedAt)); err != nil {
		return PersonaEvalResult{}, classifyBusy(fmt.Errorf("record persona eval: %w", err))
	}
	return recorded, nil
}

func (s *sqlitePersonaEvalStore) Latest(ctx context.Context, agentID, digest, layer string) (PersonaEvalResult, bool, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+personaEvalColumns+` FROM persona_eval_result
		WHERE agent_id = ? AND fingerprint = ? AND layer = ? ORDER BY created_at DESC, id DESC LIMIT 1`, agentID, digest, layer)
	result, err := scanPersonaEval(row)
	if errors.Is(err, sql.ErrNoRows) {
		return PersonaEvalResult{}, false, nil
	}
	if err != nil {
		return PersonaEvalResult{}, false, err
	}
	return result, true, nil
}

func (s *sqlitePersonaEvalStore) EverPassed(ctx context.Context, agentID, digest, layer string) (bool, error) {
	var passed int
	err := s.db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM persona_eval_result WHERE agent_id = ? AND fingerprint = ? AND layer = ? AND passed = 1)`,
		agentID, digest, layer).Scan(&passed)
	if err != nil {
		return false, classifyBusy(fmt.Errorf("check persona eval history: %w", err))
	}
	return passed == 1, nil
}

func (s *sqlitePersonaEvalStore) List(ctx context.Context, agentID string, limit int) ([]PersonaEvalResult, error) {
	if limit <= 0 {
		limit = 20
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+personaEvalColumns+` FROM persona_eval_result WHERE agent_id = ? ORDER BY created_at DESC, id DESC LIMIT ?`, agentID, limit)
	if err != nil {
		return nil, classifyBusy(fmt.Errorf("list persona evals: %w", err))
	}
	defer func() { _ = rows.Close() }()
	var results []PersonaEvalResult
	for rows.Next() {
		result, err := scanPersonaEval(rows)
		if err != nil {
			return nil, err
		}
		results = append(results, result)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list persona evals: %w", err)
	}
	return results, nil
}

func scanPersonaEval(row rowScanner) (PersonaEvalResult, error) {
	var result PersonaEvalResult
	var passed int
	var score sql.NullFloat64
	var detail, createdAt string
	if err := row.Scan(&result.ID, &result.AgentID, &result.Digest, &result.Layer, &passed, &score, &result.CasesTotal, &result.CasesFailed, &detail, &createdAt); err != nil {
		return PersonaEvalResult{}, err
	}
	result.Passed = passed == 1
	if score.Valid {
		value := score.Float64
		result.Score = &value
	}
	if err := json.Unmarshal([]byte(detail), &result.FailedCases); err != nil {
		return PersonaEvalResult{}, fmt.Errorf("parse persona eval detail: %w", err)
	}
	created, err := parseTime(createdAt)
	if err != nil {
		return PersonaEvalResult{}, fmt.Errorf("parse persona eval created_at: %w", err)
	}
	result.CreatedAt = created
	return result, nil
}
