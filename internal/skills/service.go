package skills

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
)

const (
	OriginLocal   = "local"
	OriginPending = "pending"
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
	ReviewedAt string
}

type Registry interface {
	UpsertScan(ctx context.Context, record *Record) (bool, error)
	Get(ctx context.Context, id string) (Record, error)
	ListByState(ctx context.Context, state string, limit int) ([]Record, error)
	ListAll(ctx context.Context, limit int) ([]Record, error)
	AcceptSkill(ctx context.Context, id, digest, granted string) error
	RejectSkill(ctx context.Context, id string) error
	DisableSkill(ctx context.Context, id string) error
}

type ScanSummary struct {
	ID       string
	Digest   string
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
	return registerResult(ctx, registry, dir, scope, OriginLocal, result)
}

func registerResult(ctx context.Context, registry Registry, dir, scope, kind string, result ScanResult) (ScanSummary, []string, error) {
	if result.Err != nil {
		return ScanSummary{}, nil, result.Err
	}
	scanned := result.Scanned
	if scanned == nil {
		digest := quarantineDigest(scope, dirBase(dir), result.Findings)
		scanned = &ScannedDir{Scope: scope, Name: dirBase(dir), Digest: digest}
	}
	if scanned.Name == "" {
		scanned.Name = dirBase(dir)
	}
	if len(scanned.Digest) < 12 {
		scanned.Digest = quarantineDigest(scope, scanned.Name, result.Findings)
	}
	if scanned.Scope == "" {
		scanned.Scope = scope
	}
	record := &Record{
		ID:      scope + "/" + scanned.Name + "@" + scanned.Digest[:12],
		Name:    scanned.Name,
		Digest:  scanned.Digest,
		State:   StateQuarantined,
		Granted: "[]",
	}
	origin, err := json.Marshal(map[string]string{"kind": kind, "scope": scope, "root": dir})
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
		var requested []string
		if scanned.Manifest != nil {
			requested = scanned.Manifest.AllowedTools
		}
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
	return ScanSummary{ID: record.ID, Digest: record.Digest, Inserted: inserted, Valid: len(result.Findings) == 0}, result.Findings, nil
}

func quarantineDigest(scope, name string, findings []string) string {
	writer := sha256.New()
	_, _ = writer.Write([]byte("skills-quarantine/v1\n"))
	_, _ = writer.Write([]byte(scope))
	_, _ = writer.Write([]byte{0})
	_, _ = writer.Write([]byte(name))
	_, _ = writer.Write([]byte{0})
	for _, finding := range findings {
		_, _ = writer.Write([]byte(finding))
		_, _ = writer.Write([]byte{0})
	}
	return hex.EncodeToString(writer.Sum(nil))
}
