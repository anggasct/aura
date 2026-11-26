package app

import (
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	appconfig "github.com/anggasct/aura/internal/app/config"
	"github.com/anggasct/aura/internal/modules/github"
	"github.com/anggasct/aura/internal/modules/schedule"
	auraagent "github.com/anggasct/aura/internal/platform/agent"
	"github.com/anggasct/aura/internal/platform/fault"
)

func newAgentsCmd(gf *globalFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "agents",
		Short: "Inspect registered agent definitions",
	}
	cmd.AddCommand(newAgentsListCmd(gf), newAgentsShowCmd(gf))
	return cmd
}

func newAgentsListCmd(gf *globalFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List registered agent definitions",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			result, err := appconfig.Load(gf.configPath)
			if err != nil {
				return err
			}
			registry, err := buildAgentRegistry(result.Config)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			write := func(format string, args ...any) error {
				_, werr := fmt.Fprintf(out, format, args...)
				return werr
			}
			if err := write("ID\tCAPABILITIES\tTOOLS\tMODEL ROUTE\n"); err != nil {
				return err
			}
			definitions := registry.Definitions()
			for i := range definitions {
				definition := &definitions[i]
				if err := write("%s\t%s\t%s\t%s\n",
					definition.ID,
					strings.Join(definition.Capabilities, ","),
					strings.Join(definition.Tools, ","),
					agentRouteLabel(definition.ModelRoute),
				); err != nil {
					return err
				}
			}
			return ctx.Err()
		},
	}
}

func newAgentsShowCmd(gf *globalFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "show <id>",
		Short: "Show one agent definition",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			result, err := appconfig.Load(gf.configPath)
			if err != nil {
				return err
			}
			registry, err := buildAgentRegistry(result.Config)
			if err != nil {
				return err
			}
			definition, ok := registry.Lookup(args[0])
			if !ok {
				return fault.New(auraagent.ErrorCodeNotFound, "agent "+args[0]+" is not registered")
			}
			out := cmd.OutOrStdout()
			write := func(format string, args ...any) error {
				_, werr := fmt.Fprintf(out, format, args...)
				return werr
			}
			if err := write("ID: %s\n", definition.ID); err != nil {
				return err
			}
			if err := write("Description: %s\n", definition.Description); err != nil {
				return err
			}
			if err := write("Instructions: %s\n", definition.Instructions); err != nil {
				return err
			}
			if err := write("Model route: %s\n", agentRouteLabel(definition.ModelRoute)); err != nil {
				return err
			}
			if err := write("Tools: %s\n", strings.Join(definition.Tools, ", ")); err != nil {
				return err
			}
			if err := write("Capabilities: %s\n", orNone(strings.Join(definition.Capabilities, ", "))); err != nil {
				return err
			}
			if len(definition.Plugins) > 0 {
				if err := write("Plugins: %s\n", strings.Join(definition.Plugins, ", ")); err != nil {
					return err
				}
			}
			if definition.Limits.TurnTimeout > 0 {
				return write("Turn timeout: %s\n", definition.Limits.TurnTimeout)
			}
			return write("Turn timeout: runtime default\n")
		},
	}
}

func agentRouteLabel(route string) string {
	if route == "" {
		return "runtime default"
	}
	return route
}

func orNone(value string) string {
	if value == "" {
		return "(none)"
	}
	return value
}

func personaToolNames(cfg *appconfig.Config) []string {
	names := definitionNames()
	if len(githubCredentials(cfg)) > 0 {
		names = append(names, github.ToolNames()...)
	}
	return names
}

func buildAgentRegistry(cfg *appconfig.Config) (*auraagent.Registry, error) {
	routeSet := make(map[string]bool, len(cfg.Models.Definitions)+len(cfg.ModelRoutes))
	for route := range cfg.Models.Definitions {
		routeSet[route] = true
	}
	for route := range cfg.ModelRoutes {
		routeSet[route] = true
	}
	modelRoutes := slices.Collect(maps.Keys(routeSet))
	slices.Sort(modelRoutes)
	var overrides []auraagent.Persona
	if cfg.Agents != nil {
		overrides = cfg.Agents.Definitions
	}
	catalog := append(appconfig.BuiltinToolDefinitions(), github.Definitions()...)
	registry, err := auraagent.BuildWithMCP(overrides, personaToolNames(cfg), catalog, cfg.MCP.ServerNames(), modelRoutes)
	if err != nil {
		return nil, err
	}
	definitions := registry.Definitions()
	for i := range definitions {
		behavior := definitions[i].EvalBehavior
		if behavior == nil || behavior.Schedule == "" {
			continue
		}
		if _, err := schedule.ParseExpression(behavior.Schedule); err != nil {
			return nil, fault.New(auraagent.ErrorCodeDefinitionInvalid, fmt.Sprintf("agent %q evals.behavior.schedule: %v", definitions[i].ID, err))
		}
	}
	return registry, nil
}
