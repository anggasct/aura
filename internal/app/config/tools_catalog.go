package config

import (
	"github.com/anggasct/aura/internal/channels/email"
	"github.com/anggasct/aura/internal/modules/codeexec"
	"github.com/anggasct/aura/internal/modules/fetch"
	"github.com/anggasct/aura/internal/modules/filesystem"
	"github.com/anggasct/aura/internal/modules/handoff"
	"github.com/anggasct/aura/internal/modules/knowledge"
	"github.com/anggasct/aura/internal/modules/search"
	"github.com/anggasct/aura/internal/modules/shellexec"
	"github.com/anggasct/aura/internal/modules/workflow"
	"github.com/anggasct/aura/internal/platform/tools"
)

func BuiltinToolDefinitions() []tools.Definition {
	definitions := make([]tools.Definition, 0, 16)
	definitions = append(definitions,
		shellexec.ToolDefinition(),
		codeexec.ToolDefinition(),
	)
	definitions = append(definitions, filesystem.ToolDefinitions()...)
	definitions = append(definitions,
		fetch.ToolDefinition(),
		search.ToolDefinition(),
		knowledge.SearchToolDefinition(),
		handoff.ToolDefinition(),
		email.SendToolDefinition(),
	)
	definitions = append(definitions, workflow.ToolDefinitions()...)
	return definitions
}
