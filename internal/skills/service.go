package skills

import (
	"context"
	"encoding/json"
)

const (
	StateQuarantined = "quarantined"
	StateActive      = "active"
	StateDisabled    = "disabled"
	StateRejected    = "rejected"
	StateConflict    = "conflict"
)

type Record struct {
	ID         string
	Name       string
	Origin     string
	Digest     string
	State      string
	Validation string
	Requested  string
	Granted    string
}

type Registry interface {
	UpsertScan(ctx context.Context, record *Record) (bool, error)
}

type ScanSummary struct {
	ID       string
	Inserted bool
	Valid    bool
}

func RegisterScan(ctx context.Context, registry Registry, dir, scope string) (ScanSummary, []string, error) {
	if ctx == nil {
		return ScanSummary{}, nil, errNilArgument("ctx")
	}
	if registry == nil {
		return ScanSummary{}, nil, errNilArgument("registry")
	}
	result := ScanDir(ctx, dir, scope)
	if result.Err != nil {
		return ScanSummary{}, nil, result.Err
	}
	scanned := result.Scanned
	record := &Record{
		ID:      scope + "/" + scanned.Name + "@" + scanned.Digest[:12],
		Name:    scanned.Name,
		Digest:  scanned.Digest,
		State:   StateQuarantined,
		Granted: "[]",
	}
	origin, err := json.Marshal(map[string]string{"kind": "local", "scope": scope})
	if err != nil {
		return ScanSummary{}, nil, codedError(ErrorCodeSkillInvalid, "encode skill origin", err)
	}
	record.Origin = string(origin)
	if len(result.Findings) > 0 {
		validation, err := json.Marshal(result.Findings)
		if err != nil {
			return ScanSummary{}, nil, codedError(ErrorCodeSkillInvalid, "encode skill findings", err)
		}
		record.Validation = string(validation)
		record.Requested = "[]"
	} else {
		record.Validation = "[]"
		requested := scanned.Manifest.AllowedTools
		if requested == nil {
			requested = []string{}
		}
		encoded, err := json.Marshal(requested)
		if err != nil {
			return ScanSummary{}, nil, codedError(ErrorCodeSkillInvalid, "encode requested capabilities", err)
		}
		record.Requested = string(encoded)
	}
	inserted, err := registry.UpsertScan(ctx, record)
	if err != nil {
		return ScanSummary{}, nil, err
	}
	return ScanSummary{ID: record.ID, Inserted: inserted, Valid: len(result.Findings) == 0}, result.Findings, nil
}
