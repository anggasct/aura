package workflow

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

const (
	ProposalProposed = "proposed"
	ProposalApproved = "approved"
	ProposalRejected = "rejected"
	ProposalExpired  = "expired"

	ProposalTTL = 7 * 24 * time.Hour
)

type Proposal struct {
	ID          string
	SessionID   string
	AgentID     string
	PrincipalID string
	Tainted     bool
	Spec        *Spec
	Digest      string
	Status      string
	DecidedAt   time.Time
	RunID       string
	CreatedAt   time.Time
}

func NewProposalID() string {
	return newID("prop")
}

func NormalizeProposal(spec *Spec, proposalID string) *Spec {
	normalized := *spec
	normalized.ID = "gen_" + proposalID
	normalized.Version = 1
	normalized.Source = SourceGenerated
	normalized.Steps = slices.Clone(spec.Steps)
	return &normalized
}

func Digest(spec *Spec) (string, error) {
	encoded, err := json.Marshal(spec)
	if err != nil {
		return "", codedError(ErrorCodeSpecInvalid, "encode spec: "+err.Error())
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:]), nil
}

func CheckCeiling(spec *Spec, tools []string) error {
	for index := range spec.Steps {
		step := &spec.Steps[index]
		if step.Executor.Kind != KindTool || step.Executor.ToolID == nil {
			continue
		}
		if !slices.Contains(tools, *step.Executor.ToolID) {
			return codedError(ErrorCodeCapabilityExceeded, fmt.Sprintf("step %q uses tool %q, which the proposing persona does not have", step.ID, *step.Executor.ToolID))
		}
	}
	return nil
}

type ToolCallText func(toolID string, args json.RawMessage) (string, error)

func RenderPlan(spec *Spec, graph *Graph, show ToolCallText) (string, error) {
	lines := make([]string, 0, 1+len(graph.Order))
	lines = append(lines, "Plan: "+spec.Goal)
	for index, stepID := range graph.Order {
		step := graph.ByStep[stepID]
		line := fmt.Sprintf("%d. %s — ", index+1, step.ID)
		var call string
		switch step.Executor.Kind {
		case KindTool:
			line += "run tool " + deref(step.Executor.ToolID)
			text, err := showToolCall(step, show)
			if err != nil {
				return "", err
			}
			call = text
		case KindAgent:
			line += "ask agent " + cmpOr(deref(step.Executor.AgentID), strings.Join(step.Executor.RequiredCapabilities, "+"))
		case KindWait:
			line += "wait for " + deref(step.Executor.Event)
		case KindApproval:
			line += "owner approval"
		case KindWorkflow:
			line += "run workflow " + deref(step.Executor.WorkflowID)
		default:
			line += string(step.Executor.Kind)
		}
		if len(step.DependsOn) > 0 {
			line += " (after " + strings.Join(step.DependsOn, ", ") + ")"
		}
		if step.Condition != nil {
			line += " if " + *step.Condition
		}
		lines = append(lines, line)
		if call != "" {
			for _, callLine := range strings.Split(call, "\n") {
				lines = append(lines, "    "+callLine)
			}
		}
	}
	return strings.Join(lines, "\n"), nil
}

func showToolCall(step *StepSpec, show ToolCallText) (string, error) {
	if show == nil {
		return "", codedError(ErrorCodePlanUnshowable, fmt.Sprintf("step %q: no tool display is available to show the call", step.ID))
	}
	text, err := show(deref(step.Executor.ToolID), toolArguments(step))
	if err != nil {
		return "", codedError(ErrorCodePlanUnshowable, fmt.Sprintf("step %q: the call of tool %q cannot be shown: %v", step.ID, deref(step.Executor.ToolID), err))
	}
	return text, nil
}

func deref(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func cmpOr(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func (s *Store) CreateProposal(ctx context.Context, proposal *Proposal) error {
	encoded, err := json.Marshal(proposal.Spec)
	if err != nil {
		return codedError(ErrorCodeSpecInvalid, "encode proposal spec: "+err.Error())
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO workflow_proposal (id, session_id, agent_id, principal_id, spec_json, spec_sha256, status, created_at, tainted) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		proposal.ID, proposal.SessionID, proposal.AgentID, proposal.PrincipalID, string(encoded), proposal.Digest, ProposalProposed, proposal.CreatedAt.UTC().Format(time.RFC3339Nano), proposal.Tainted)
	if err != nil {
		return fmt.Errorf("insert proposal: %w", err)
	}
	return nil
}

const proposalColumns = `id, session_id, agent_id, principal_id, spec_json, spec_sha256, status, COALESCE(decided_at, ''), COALESCE(run_id, ''), created_at, tainted`

func (s *Store) Proposal(ctx context.Context, id string, now time.Time) (*Proposal, error) {
	if err := s.expireProposals(ctx, now); err != nil {
		return nil, err
	}
	proposal, err := scanProposal(s.db.QueryRowContext(ctx, `SELECT `+proposalColumns+` FROM workflow_proposal WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, codedError(ErrorCodeProposalNotFound, "proposal "+id+" does not exist")
	}
	return proposal, err
}

func (s *Store) Proposals(ctx context.Context, status string, now time.Time) ([]*Proposal, error) {
	if err := s.expireProposals(ctx, now); err != nil {
		return nil, err
	}
	query := `SELECT ` + proposalColumns + ` FROM workflow_proposal`
	args := []any{}
	if status != "" {
		query += ` WHERE status = ?`
		args = append(args, status)
	}
	rows, err := s.db.QueryContext(ctx, query+` ORDER BY created_at, id`, args...)
	if err != nil {
		return nil, fmt.Errorf("list proposals: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var proposals []*Proposal
	for rows.Next() {
		proposal, err := scanProposal(rows)
		if err != nil {
			return nil, err
		}
		proposals = append(proposals, proposal)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate proposals: %w", err)
	}
	return proposals, nil
}

func (s *Store) DecideProposal(ctx context.Context, id, status, runID string, at time.Time) error {
	result, err := s.db.ExecContext(ctx, `UPDATE workflow_proposal SET status = ?, decided_at = ?, run_id = ? WHERE id = ? AND status = ?`,
		status, at.UTC().Format(time.RFC3339Nano), nullable(runID), id, ProposalProposed)
	if err != nil {
		return fmt.Errorf("decide proposal: %w", err)
	}
	if changed, err := result.RowsAffected(); err != nil || changed != 1 {
		return codedError(ErrorCodeProposalDecided, "proposal "+id+" is not awaiting a decision")
	}
	return nil
}

func (s *Store) expireProposals(ctx context.Context, now time.Time) error {
	if _, err := s.db.ExecContext(ctx, `UPDATE workflow_proposal SET status = ? WHERE status = ? AND created_at < ?`,
		ProposalExpired, ProposalProposed, now.Add(-ProposalTTL).UTC().Format(time.RFC3339Nano)); err != nil {
		return fmt.Errorf("expire proposals: %w", err)
	}
	return nil
}

type proposalRow interface {
	Scan(dest ...any) error
}

func scanProposal(row proposalRow) (*Proposal, error) {
	var proposal Proposal
	var specJSON, decidedAt, createdAt string
	if err := row.Scan(&proposal.ID, &proposal.SessionID, &proposal.AgentID, &proposal.PrincipalID, &specJSON, &proposal.Digest, &proposal.Status, &decidedAt, &proposal.RunID, &createdAt, &proposal.Tainted); err != nil {
		return nil, err
	}
	proposal.Spec = &Spec{}
	if err := json.Unmarshal([]byte(specJSON), proposal.Spec); err != nil {
		return nil, codedError(ErrorCodeSpecInvalid, "decode proposal spec: "+err.Error())
	}
	proposal.CreatedAt, _ = time.Parse(time.RFC3339Nano, createdAt)
	if decidedAt != "" {
		proposal.DecidedAt, _ = time.Parse(time.RFC3339Nano, decidedAt)
	}
	return &proposal, nil
}

func (i *Interpreter) CompileProposal(spec *Spec, tools []string) (*Graph, error) {
	i.depsMu.Lock()
	deps := i.deps
	i.depsMu.Unlock()
	graph, err := Compile(spec, deps)
	if err != nil {
		return nil, err
	}
	if err := ValidateComposition(spec, i.Spec); err != nil {
		return nil, err
	}
	if err := CheckCeiling(spec, tools); err != nil {
		return nil, err
	}
	if err := checkStepBudget(spec, i.options.GeneratedBudget); err != nil {
		return nil, err
	}
	return graph, nil
}

func (s *Store) AttachProposalRun(ctx context.Context, id, runID string) error {
	if _, err := s.db.ExecContext(ctx, `UPDATE workflow_proposal SET run_id = ? WHERE id = ?`, runID, id); err != nil {
		return fmt.Errorf("attach proposal run: %w", err)
	}
	return nil
}

func (i *Interpreter) ValidationDeps() ValidationDeps {
	i.depsMu.Lock()
	defer i.depsMu.Unlock()
	return i.deps
}
