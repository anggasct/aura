package workflow

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

type ApprovalKind string

const (
	ApprovalInstalled ApprovalKind = "installed"
	ApprovalProposal  ApprovalKind = "proposal"
	ApprovalRoutine   ApprovalKind = "routine"
)

type Approval struct {
	DefinitionID            string
	DefinitionVersion       int
	Digest                  string
	Kind                    ApprovalKind
	AgentID                 string
	ApproverID              string
	ProposalID              string
	SourceDefinitionID      string
	SourceDefinitionVersion int
	Tainted                 bool
	ApprovedAt              time.Time
}

type approvalExecer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

func insertApproval(ctx context.Context, db approvalExecer, approval *Approval) error {
	var stored string
	err := db.QueryRowContext(ctx,
		`SELECT spec_sha256 FROM workflow_approval WHERE definition_id = ? AND definition_version = ?`,
		approval.DefinitionID, approval.DefinitionVersion,
	).Scan(&stored)
	switch {
	case err == nil:
		if stored != approval.Digest {
			return codedError(ErrorCodeApprovalConflict, fmt.Sprintf("definition %s version %d is already approved with a different digest", approval.DefinitionID, approval.DefinitionVersion))
		}
		return nil
	case !errors.Is(err, sql.ErrNoRows):
		return fmt.Errorf("read approval: %w", err)
	}
	var sourceVersion any
	if approval.SourceDefinitionID != "" {
		sourceVersion = approval.SourceDefinitionVersion
	}
	if _, err := db.ExecContext(ctx,
		`INSERT INTO workflow_approval (definition_id, definition_version, spec_sha256, kind, agent_id, approver_id, proposal_id, source_definition_id, source_definition_version, tainted, approved_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		approval.DefinitionID, approval.DefinitionVersion, approval.Digest, string(approval.Kind), nullable(approval.AgentID), approval.ApproverID,
		nullable(approval.ProposalID), nullable(approval.SourceDefinitionID), sourceVersion, approval.Tainted,
		approval.ApprovedAt.UTC().Format(time.RFC3339Nano),
	); err != nil {
		return fmt.Errorf("insert approval: %w", err)
	}
	return nil
}

func (s *Store) Approval(ctx context.Context, id string, version int) (*Approval, error) {
	var approval Approval
	var kind, approvedAt string
	var sourceVersion sql.NullInt64
	err := s.db.QueryRowContext(ctx,
		`SELECT definition_id, definition_version, spec_sha256, kind, COALESCE(agent_id, ''), approver_id, COALESCE(proposal_id, ''),
			COALESCE(source_definition_id, ''), source_definition_version, tainted, approved_at
		FROM workflow_approval WHERE definition_id = ? AND definition_version = ?`,
		id, version,
	).Scan(&approval.DefinitionID, &approval.DefinitionVersion, &approval.Digest, &kind, &approval.AgentID, &approval.ApproverID, &approval.ProposalID,
		&approval.SourceDefinitionID, &sourceVersion, &approval.Tainted, &approvedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, codedError(ErrorCodeApprovalMissing, fmt.Sprintf("definition %s version %d has no approval record", id, version))
	}
	if err != nil {
		return nil, fmt.Errorf("read approval: %w", err)
	}
	approval.Kind = ApprovalKind(kind)
	approval.SourceDefinitionVersion = int(sourceVersion.Int64)
	approval.ApprovedAt, _ = time.Parse(time.RFC3339Nano, approvedAt)
	return &approval, nil
}

func (s *Store) ApproveProposal(ctx context.Context, proposalID string, approval *Approval) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin proposal approval: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	record := *approval
	record.Kind = ApprovalProposal
	record.ProposalID = proposalID
	if record.ApprovedAt.IsZero() {
		record.ApprovedAt = time.Now().UTC()
	}
	result, err := tx.ExecContext(ctx, `UPDATE workflow_proposal SET status = ?, decided_at = ? WHERE id = ? AND status = ?`,
		ProposalApproved, record.ApprovedAt.UTC().Format(time.RFC3339Nano), proposalID, ProposalProposed)
	if err != nil {
		return fmt.Errorf("approve proposal: %w", err)
	}
	if changed, err := result.RowsAffected(); err != nil || changed != 1 {
		return codedError(ErrorCodeProposalDecided, "proposal "+proposalID+" is not awaiting a decision")
	}
	if err := insertApproval(ctx, tx, &record); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit proposal approval: %w", err)
	}
	return nil
}

func (i *Interpreter) requireApproval(ctx context.Context, id string, version int, digest string) (*Approval, error) {
	approval, err := i.store.Approval(ctx, id, version)
	if err != nil {
		return nil, err
	}
	if approval.Digest != digest {
		return nil, codedError(ErrorCodeApprovalMissing, fmt.Sprintf("definition %s version %d is not approved in this form", id, version))
	}
	return approval, nil
}

func (i *Interpreter) requireSpecApproval(ctx context.Context, spec *Spec) (*Approval, error) {
	digest, err := Digest(spec)
	if err != nil {
		return nil, err
	}
	return i.requireApproval(ctx, spec.ID, spec.Version, digest)
}

func originUnderApproval(origin *RunOrigin, approval *Approval) (*RunOrigin, error) {
	if origin == nil {
		return nil, codedError(ErrorCodeOriginIncomplete, "a run needs a principal, a known principal class and an agent")
	}
	copied := *origin
	copied.Tainted = origin.Tainted || approval.Tainted
	return &copied, nil
}
