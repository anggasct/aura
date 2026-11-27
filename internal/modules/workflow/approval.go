package workflow

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/anggasct/aura/internal/platform/sqlite"
)

type ApprovalKind string

const (
	ApprovalInstalled ApprovalKind = "installed"
	ApprovalProposal  ApprovalKind = "proposal"
	ApprovalRoutine   ApprovalKind = "routine"
)

type PersonaPin struct {
	SoulSHA256 string `json:"soul_sha256"`
	ModelRoute string `json:"model_route"`
}

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
	PersonaPins             map[string]PersonaPin
	ApprovedAt              time.Time
}

func pinAgentSteps(spec *Spec, agents AgentResolver) (map[string]PersonaPin, error) {
	pins := map[string]PersonaPin{}
	for index := range spec.Steps {
		executor := &spec.Steps[index].Executor
		if executor.Kind != KindAgent || executor.AgentID == nil {
			continue
		}
		personaID := *executor.AgentID
		if _, pinned := pins[personaID]; pinned {
			continue
		}
		if agents == nil {
			return nil, codedError(ErrorCodePersonaMissing, fmt.Sprintf("persona %q cannot be pinned: no persona registry is configured", personaID))
		}
		definition, err := agents.Resolve(nil, &personaID)
		if err != nil {
			return nil, codedError(ErrorCodePersonaMissing, fmt.Sprintf("persona %q is not configured", personaID))
		}
		fingerprint := definition.Fingerprint()
		pins[personaID] = PersonaPin{SoulSHA256: fingerprint.SoulSHA256, ModelRoute: fingerprint.ModelRoute}
	}
	return pins, nil
}

func (a *Approval) personaPin(personaID string) (pin PersonaPin, pinned bool, err error) {
	pin, pinned = a.PersonaPins[personaID]
	if !pinned && a.Kind != ApprovalInstalled {
		return PersonaPin{}, false, codedError(ErrorCodeApprovalMissing, fmt.Sprintf("definition %s version %d has no persona pin for %s", a.DefinitionID, a.DefinitionVersion, personaID))
	}
	return pin, pinned, nil
}

func insertApproval(ctx context.Context, db sqlite.Querier, approval *Approval) error {
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
		return sqlite.Classify(fmt.Errorf("read approval: %w", err))
	}
	var sourceVersion any
	if approval.SourceDefinitionID != "" {
		sourceVersion = approval.SourceDefinitionVersion
	}
	pins := approval.PersonaPins
	if pins == nil {
		pins = map[string]PersonaPin{}
	}
	encodedPins, err := json.Marshal(pins)
	if err != nil {
		return codedError(ErrorCodeInvalidArgument, "encode persona pins: "+err.Error())
	}
	if _, err := db.ExecContext(ctx,
		`INSERT INTO workflow_approval (definition_id, definition_version, spec_sha256, kind, agent_id, approver_id, proposal_id, source_definition_id, source_definition_version, tainted, persona_pins, approved_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		approval.DefinitionID, approval.DefinitionVersion, approval.Digest, string(approval.Kind), nullable(approval.AgentID), approval.ApproverID,
		nullable(approval.ProposalID), nullable(approval.SourceDefinitionID), sourceVersion, approval.Tainted, string(encodedPins),
		approval.ApprovedAt.UTC().Format(time.RFC3339Nano),
	); err != nil {
		return sqlite.Classify(fmt.Errorf("insert approval: %w", err))
	}
	return nil
}

func (s *Store) Approval(ctx context.Context, id string, version int) (*Approval, error) {
	var approval Approval
	var kind, approvedAt, pins string
	var sourceVersion sql.NullInt64
	err := sqlite.Conn(ctx, s.db).QueryRowContext(ctx,
		`SELECT definition_id, definition_version, spec_sha256, kind, COALESCE(agent_id, ''), approver_id, COALESCE(proposal_id, ''),
			COALESCE(source_definition_id, ''), source_definition_version, tainted, persona_pins, approved_at
		FROM workflow_approval WHERE definition_id = ? AND definition_version = ?`,
		id, version,
	).Scan(&approval.DefinitionID, &approval.DefinitionVersion, &approval.Digest, &kind, &approval.AgentID, &approval.ApproverID, &approval.ProposalID,
		&approval.SourceDefinitionID, &sourceVersion, &approval.Tainted, &pins, &approvedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, codedError(ErrorCodeApprovalMissing, fmt.Sprintf("definition %s version %d has no approval record", id, version))
	}
	if err != nil {
		return nil, sqlite.Classify(fmt.Errorf("read approval: %w", err))
	}
	if err := json.Unmarshal([]byte(pins), &approval.PersonaPins); err != nil {
		return nil, codedError(ErrorCodeApprovalMissing, fmt.Sprintf("definition %s version %d has unreadable persona pins", id, version))
	}
	if len(approval.PersonaPins) == 0 {
		approval.PersonaPins = nil
	}
	approval.Kind = ApprovalKind(kind)
	approval.SourceDefinitionVersion = int(sourceVersion.Int64)
	approval.ApprovedAt, _ = time.Parse(time.RFC3339Nano, approvedAt)
	return &approval, nil
}

func (s *Store) ApproveProposal(ctx context.Context, proposalID string, approval *Approval) error {
	if approval == nil {
		return codedError(ErrorCodeInvalidArgument, "proposal approval must not be nil")
	}
	record := *approval
	record.Kind = ApprovalProposal
	record.ProposalID = proposalID
	if record.ApprovedAt.IsZero() {
		record.ApprovedAt = time.Now().UTC()
	}
	return sqlite.RunInTx(ctx, s.db, func(ctx context.Context) error {
		q := sqlite.Conn(ctx, s.db)
		result, err := q.ExecContext(ctx, `UPDATE workflow_proposal SET status = ?, decided_at = ? WHERE id = ? AND status = ?`,
			ProposalApproved, record.ApprovedAt.UTC().Format(time.RFC3339Nano), proposalID, ProposalProposed)
		if err != nil {
			return sqlite.Classify(fmt.Errorf("approve proposal: %w", err))
		}
		if changed, err := result.RowsAffected(); err != nil || changed != 1 {
			if err != nil {
				return sqlite.Classify(fmt.Errorf("approve proposal: %w", err))
			}
			return codedError(ErrorCodeProposalDecided, "proposal "+proposalID+" is not awaiting a decision")
		}
		return insertApproval(ctx, q, &record)
	})
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

func proposalClaimFor(approval *Approval, explicit string) (string, error) {
	switch {
	case explicit != "":
		if approval.Kind != ApprovalProposal || approval.ProposalID != explicit {
			return "", codedError(ErrorCodeApprovalMissing, fmt.Sprintf("definition %s version %d is not the approval of proposal %s", approval.DefinitionID, approval.DefinitionVersion, explicit))
		}
		return explicit, nil
	case approval.Kind != ApprovalProposal:
		return "", nil
	case approval.Tainted:
		return "", codedError(ErrorCodeApprovalSpent, fmt.Sprintf("definition %s version %d was approved from a proposal that untrusted input shaped, and that approval covers only its own run", approval.DefinitionID, approval.DefinitionVersion))
	}
	return approval.ProposalID, nil
}

func (s *Store) proposalApproval(ctx context.Context, proposalID string) (*Approval, error) {
	var id string
	var version int
	err := sqlite.Conn(ctx, s.db).QueryRowContext(ctx, `SELECT definition_id, definition_version FROM workflow_approval WHERE proposal_id = ?`, proposalID).Scan(&id, &version)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, codedError(ErrorCodeApprovalMissing, "proposal "+proposalID+" has no approval record")
	}
	if err != nil {
		return nil, sqlite.Classify(fmt.Errorf("read proposal approval: %w", err))
	}
	return s.Approval(ctx, id, version)
}
