package app

import (
	"fmt"

	"github.com/anggasct/aura/internal/channels/discord"
	"github.com/anggasct/aura/internal/channels/email"
	"github.com/anggasct/aura/internal/channels/telegram"
	"github.com/anggasct/aura/internal/modules/broadcast"
	"github.com/anggasct/aura/internal/modules/child"
	personaeval "github.com/anggasct/aura/internal/modules/eval/persona"
	"github.com/anggasct/aura/internal/modules/handoff"
	"github.com/anggasct/aura/internal/modules/knowledge"
	"github.com/anggasct/aura/internal/modules/memory"
	"github.com/anggasct/aura/internal/modules/plugin"
	"github.com/anggasct/aura/internal/modules/profile"
	"github.com/anggasct/aura/internal/modules/schedule"
	"github.com/anggasct/aura/internal/modules/skills"
	"github.com/anggasct/aura/internal/modules/workflow"
	durablesqlite "github.com/anggasct/aura/internal/platform/durable/sqlite"
	"github.com/anggasct/aura/internal/platform/effect"
	"github.com/anggasct/aura/internal/platform/model"
	"github.com/anggasct/aura/internal/platform/operation"
	"github.com/anggasct/aura/internal/platform/principal"
	"github.com/anggasct/aura/internal/platform/sqlite"
	"github.com/anggasct/aura/internal/platform/storage"
	turnstore "github.com/anggasct/aura/internal/platform/turn/store"
	"github.com/anggasct/aura/internal/platform/usage"
	"github.com/anggasct/aura/internal/platform/webhook"
)

func Schemas() []sqlite.SchemaOwner {
	return []sqlite.SchemaOwner{
		turnstore.Schema(),
		durablesqlite.Schema(),
		effect.Schema(),
		usage.Schema(),
		principal.Schema(),
		model.Schema(),
		storage.Schema(),
		webhook.Schema(),
		knowledge.Schema(),
		memory.Schema(),
		profile.Schema(),
		schedule.Schema(),
		broadcast.Schema(),
		workflow.Schema(),
		handoff.Schema(),
		skills.Schema(),
		plugin.Schema(),
		child.Schema(),
		personaeval.Schema(),
		discord.Schema(),
		telegram.Schema(),
		email.Schema(),
		operation.Schema(),
	}
}

func Migrations() []sqlite.Migration {
	migrations, err := sqlite.History(Schemas()...)
	if err != nil {
		panic(fmt.Sprintf("invalid schema owners: %v", err))
	}
	return migrations
}
