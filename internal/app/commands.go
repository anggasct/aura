package app

import (
	"context"
	"log/slog"

	"github.com/spf13/cobra"

	appconfig "github.com/anggasct/aura/internal/app/config"
	"github.com/anggasct/aura/internal/modules/broadcast"
	"github.com/anggasct/aura/internal/modules/handoff"
	"github.com/anggasct/aura/internal/modules/knowledge"
	"github.com/anggasct/aura/internal/modules/schedule"
	"github.com/anggasct/aura/internal/modules/skills"
)

func mountCommands(root *cobra.Command, gf *globalFlags) {
	root.AddCommand(knowledge.Commands(knowledgeOpener(gf))...)
	root.AddCommand(handoff.Commands(handoffOpener(gf))...)
	root.AddCommand(schedule.Commands(scheduleOpener(gf))...)
	root.AddCommand(broadcast.Commands(broadcastOpener(gf))...)
	root.AddCommand(skills.Commands(skillsOpener(gf))...)
}

func skillsOpener(gf *globalFlags) skills.Opener {
	return func(cmd *cobra.Command, fn func(ctx context.Context, engine *skills.Engine, defaultRoot string) error) error {
		result, err := appconfig.Load(gf.configPath)
		if err != nil {
			return err
		}
		return withApp(cmd, &commandInput{result: &result, parts: []Part{PartSkills}}, func(ctx context.Context, app *App) error {
			return fn(ctx, app.skillsEngine, app.skillsRoot)
		})
	}
}

func knowledgeOpener(gf *globalFlags) knowledge.Opener {
	return func(cmd *cobra.Command, fn func(ctx context.Context, logger *slog.Logger, service *knowledge.Service) error) error {
		result, err := appconfig.Load(gf.configPath)
		if err != nil {
			return err
		}
		return withApp(cmd, &commandInput{result: &result, parts: []Part{PartKnowledge}}, func(ctx context.Context, app *App) error {
			return fn(ctx, app.logger, app.knowledge)
		})
	}
}
