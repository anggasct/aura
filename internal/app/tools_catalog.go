package app

import (
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"slices"

	appconfig "github.com/anggasct/aura/internal/app/config"
	"github.com/anggasct/aura/internal/platform/effect"
	"github.com/anggasct/aura/internal/platform/toolbroker"
	"github.com/anggasct/aura/internal/platform/tools"
	runtimeadk "github.com/anggasct/aura/internal/platform/turn/adk"
	turnstore "github.com/anggasct/aura/internal/platform/turn/store"
)

type stepCatalog struct {
	definitions map[string]*tools.Definition
}

func newStepCatalog(extra []tools.Definition) *stepCatalog {
	builtins := appconfig.BuiltinToolDefinitions()
	catalog := &stepCatalog{definitions: make(map[string]*tools.Definition, len(builtins)+len(extra))}
	for i := range builtins {
		catalog.definitions[builtins[i].Name] = &builtins[i]
	}
	for i := range extra {
		catalog.definitions[extra[i].Name] = &extra[i]
	}
	return catalog
}

func (c *stepCatalog) Lookup(name string) (*tools.Definition, bool) {
	definition, ok := c.definitions[name]
	return definition, ok
}

func (c *stepCatalog) CheckArguments(name string, args json.RawMessage) error {
	definition, ok := c.definitions[name]
	if !ok {
		return toolbroker.Errorf(toolbroker.ResultPolicyDenied, "unknown tool %q version %q", name, tools.Version)
	}
	if _, err := definition.Validate(args); err != nil {
		return toolbroker.Errorf(toolbroker.ResultInvalidArgument, "%v", err)
	}
	return nil
}

func definitionNames() []string {
	builtins := appconfig.BuiltinToolDefinitions()
	names := make([]string, 0, len(builtins))
	for i := range builtins {
		names = append(names, builtins[i].Name)
	}
	slices.Sort(names)
	return names
}

func catalogDefinitions() []runtimeadk.BuiltinToolDefinition {
	builtins := appconfig.BuiltinToolDefinitions()
	out := make([]runtimeadk.BuiltinToolDefinition, 0, len(builtins))
	for i := range builtins {
		out = append(out, runtimeadk.BuiltinToolDefinition{
			Name:                 builtins[i].Name,
			Version:              builtins[i].Version,
			Channel:              builtins[i].Channel,
			Description:          "Aura built-in " + builtins[i].Name + " tool",
			Schema:               builtins[i].Schema,
			RequiredCapabilities: builtins[i].RequiredCapabilities,
			OptIn:                builtins[i].OptIn,
		})
	}
	return out
}

func effectfulToolNames() []string {
	builtins := appconfig.BuiltinToolDefinitions()
	names := make([]string, 0, len(builtins))
	for i := range builtins {
		if builtins[i].RequiresApproval || builtins[i].Effectful {
			names = append(names, builtins[i].Name)
		}
	}
	slices.Sort(names)
	return names
}

func stepToolNames(personaTools []string) []string {
	orchestration := map[string]bool{}
	builtins := appconfig.BuiltinToolDefinitions()
	for i := range builtins {
		if builtins[i].Orchestration {
			orchestration[builtins[i].Name] = true
		}
	}
	return slices.DeleteFunc(slices.Clone(personaTools), func(name string) bool { return orchestration[name] })
}

func configuredToolSecrets(toolsCfg *appconfig.Tools) []string {
	if toolsCfg == nil {
		return nil
	}
	return toolsCfg.WebSearch.ConfiguredSecrets()
}

func newChannelEffects(db *sql.DB, logger *slog.Logger) (*effect.Executor, error) {
	if db == nil {
		return nil, errors.New("storage database is required")
	}
	if logger == nil {
		logger = slog.Default()
	}
	journal, err := effect.NewJournal(db, effect.Options{Logger: logger, Turns: turnstore.NewTurnStore(db)})
	if err != nil {
		return nil, err
	}
	return effect.NewExecutor(journal)
}
