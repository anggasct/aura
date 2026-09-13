package cli

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/anggasct/aura/internal/channel/terminal"
	"github.com/anggasct/aura/internal/config"
	"github.com/anggasct/aura/internal/skills"
	"github.com/anggasct/aura/internal/store"
)

type skillStoreRegistry struct {
	inner store.SkillStore
}

func (a *skillStoreRegistry) UpsertScan(ctx context.Context, record *skills.Record) (bool, error) {
	inserted, err := a.inner.UpsertScan(ctx, &store.SkillRow{
		ID:         record.ID,
		Name:       record.Name,
		Origin:     record.Origin,
		Digest:     record.Digest,
		State:      record.State,
		Validation: record.Validation,
		Requested:  record.Requested,
		Granted:    record.Granted,
	})
	if err != nil {
		return false, mapSkillStoreError(err)
	}
	return inserted, nil
}

func (a *skillStoreRegistry) Get(ctx context.Context, id string) (skills.Record, error) {
	row, err := a.inner.GetSkill(ctx, id)
	if err != nil {
		return skills.Record{}, mapSkillStoreError(err)
	}
	return skillRowToRecord(&row), nil
}

func (a *skillStoreRegistry) ListByState(ctx context.Context, state string, limit int) ([]skills.Record, error) {
	rows, err := a.inner.ListSkillsByState(ctx, state, limit)
	if err != nil {
		return nil, mapSkillStoreError(err)
	}
	out := make([]skills.Record, 0, len(rows))
	for i := range rows {
		out = append(out, skillRowToRecord(&rows[i]))
	}
	return out, nil
}

func (a *skillStoreRegistry) AcceptSkill(ctx context.Context, id, digest, granted string) error {
	if err := a.inner.AcceptSkill(ctx, id, digest, granted); err != nil {
		return mapSkillStoreError(err)
	}
	return nil
}

func (a *skillStoreRegistry) RejectSkill(ctx context.Context, id string) error {
	if err := a.inner.RejectSkill(ctx, id); err != nil {
		return mapSkillStoreError(err)
	}
	return nil
}

func (a *skillStoreRegistry) DisableSkill(ctx context.Context, id string) error {
	if err := a.inner.DisableSkill(ctx, id); err != nil {
		return mapSkillStoreError(err)
	}
	return nil
}

func skillRowToRecord(row *store.SkillRow) skills.Record {
	record := skills.Record{
		ID:         row.ID,
		Name:       row.Name,
		Origin:     row.Origin,
		Digest:     row.Digest,
		State:      row.State,
		Validation: row.Validation,
		Requested:  row.Requested,
		Granted:    row.Granted,
	}
	if row.ReviewedAt != nil {
		record.ReviewedAt = row.ReviewedAt.UTC().Format(time.RFC3339Nano)
	}
	return record
}

func (a *skillStoreRegistry) ListAll(ctx context.Context, limit int) ([]skills.Record, error) {
	rows, err := a.inner.ListSkills(ctx, limit)
	if err != nil {
		return nil, mapSkillStoreError(err)
	}
	out := make([]skills.Record, 0, len(rows))
	for i := range rows {
		out = append(out, skillRowToRecord(&rows[i]))
	}
	return out, nil
}

func mapSkillStoreError(err error) error {
	code, ok := store.CodeOf(err)
	if !ok {
		return skills.Errorf(skills.ErrorCodeSkillUnavailable, "skill registry failed")
	}
	switch code {
	case store.ErrorCodeSkillNotFound:
		return skills.Errorf(skills.ErrorCodeSkillNotFound, "skill is not registered")
	case store.ErrorCodeSkillInvalid:
		return skills.Errorf(skills.ErrorCodeSkillInvalid, "skill record is not valid")
	case store.ErrorCodeSkillDigestChanged:
		return skills.Errorf(skills.ErrorCodeSkillDigestChanged, "skill content changed since review")
	case store.ErrorCodeInvalidArgument:
		return skills.Errorf(skills.ErrorCodeInvalidArgument, "skill argument is not valid")
	default:
		return skills.Errorf(skills.ErrorCodeSkillUnavailable, "skill registry is unavailable")
	}
}

type skillTerminalBridge struct {
	engine *skills.Engine
	root   string
}

func (b *skillTerminalBridge) ActivateSkill(ctx context.Context, name string) (terminal.SkillContext, error) {
	activation, err := b.engine.Activate(ctx, name, "explicit: /skill "+name)
	if err != nil {
		return terminal.SkillContext{}, err
	}
	return terminal.SkillContext{
		ID:     activation.SkillID,
		Caveat: activation.Context.Caveat,
		Text:   activation.Context.Text,
	}, nil
}

func (b *skillTerminalBridge) CreateSkill(ctx context.Context, name, description string) (terminal.SkillDraft, error) {
	summary, err := b.engine.StageDraft(ctx, name, description, b.root)
	if err != nil {
		return terminal.SkillDraft{}, err
	}
	return terminal.SkillDraft{ID: summary.ID, Digest: summary.Digest}, nil
}

func buildSkillsEngine(ctx context.Context, cfg *config.Skills, db *sql.DB, logger *slog.Logger) (*skills.Engine, error) {
	if cfg == nil {
		return nil, errors.New("cli: skills config must not be nil")
	}
	roots := make([]string, 0, len(cfg.Roots))
	for _, dir := range cfg.Roots {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("cli: skill root %s is not available: %w", filepath.Base(dir), err)
		}
		roots = append(roots, dir)
	}
	engine, err := skills.NewEngine(&skillStoreRegistry{inner: store.NewSkillStore(db)}, &skills.EngineConfig{
		Dirs:                 roots,
		MaxIndexed:           cfg.MaxIndexedSkills,
		MaxInstructionRunes:  cfg.MaxInstructionTokens * skills.CharsPerToken,
		MaxResourceBytes:     cfg.MaxResourceBytes,
		ScriptToolName:       "exec",
		ScriptToolCapability: "shell.execute",
		QuarantineRetention:  time.Duration(cfg.QuarantineRetention),
		PolicyVersion:        skills.PolicyVersion,
		Logger:               logger,
	})
	if err != nil {
		return nil, err
	}
	if err := engine.Refresh(ctx); err != nil {
		return nil, err
	}
	return engine, nil
}
