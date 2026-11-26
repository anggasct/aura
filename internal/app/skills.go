package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/anggasct/aura/internal/modules/skills"
	"github.com/anggasct/aura/internal/platform/command"
	"github.com/anggasct/aura/internal/platform/turn"
	"github.com/anggasct/aura/internal/platform/turn/store"
)

type skillCommandBridge struct {
	engine *skills.Engine
	root   string
}

func (b *skillCommandBridge) Activate(ctx context.Context, name string) (command.SkillPin, error) {
	activation, err := b.engine.Activate(ctx, name, "explicit: /skill "+name)
	if err != nil {
		return command.SkillPin{}, err
	}
	current, err := b.engine.Current(ctx, activation.SkillID, activation.Digest)
	if err != nil {
		return command.SkillPin{}, err
	}
	if !current {
		return command.SkillPin{}, skills.Errorf(skills.ErrorCodeSkillUnavailable, "skill is not active")
	}
	return command.SkillPin{
		SkillID:       activation.SkillID,
		Digest:        activation.Digest,
		InvocationID:  activation.InvocationID,
		PolicyVersion: activation.PolicyVersion,
	}, nil
}

type skillActivationLog struct {
	events turnstore.EventKindReader
}

func (l *skillActivationLog) Activations(ctx context.Context, sessionID string, limit int) ([]turn.SkillActivation, error) {
	events, err := l.events.LatestByKind(ctx, sessionID, turn.EventKindSkillActivated, limit)
	if err != nil {
		return nil, err
	}
	out := make([]turn.SkillActivation, 0, len(events))
	for i := range events {
		var activation turn.SkillActivation
		if err := json.Unmarshal(events[i].Payload, &activation); err != nil || activation.SkillID == "" {
			continue
		}
		out = append(out, activation)
	}
	return out, nil
}

type skillRuntime struct {
	engine  *skills.Engine
	context *skills.SessionContext
	bridge  *skillCommandBridge
}

func buildSkillRuntime(ctx context.Context, cfg *skills.Config, db *sql.DB, logger *slog.Logger) (*skillRuntime, error) {
	engine, err := buildSkillsEngine(ctx, cfg, db, logger)
	if err != nil {
		return nil, err
	}
	sessionContext, err := skills.NewSessionContext(engine, &skillActivationLog{events: turnstore.NewEventKindReader(db)}, cfg.MaxSessionContextTokens*skills.CharsPerToken)
	if err != nil {
		return nil, err
	}
	return &skillRuntime{engine: engine, context: sessionContext, bridge: &skillCommandBridge{engine: engine, root: cfg.Roots[0]}}, nil
}

func (b *skillCommandBridge) StageDraft(ctx context.Context, name, description string) (command.Draft, error) {
	summary, err := b.engine.StageDraft(ctx, name, description, b.root)
	if err != nil {
		return command.Draft{}, err
	}
	return command.Draft{ID: summary.ID, Digest: summary.Digest}, nil
}

func buildSkillsEngine(ctx context.Context, cfg *skills.Config, db *sql.DB, logger *slog.Logger) (*skills.Engine, error) {
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
	registry, err := skills.NewSQLiteStore(db)
	if err != nil {
		return nil, err
	}
	engine, err := skills.NewEngine(registry, &skills.EngineConfig{
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
