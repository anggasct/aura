package cli

import (
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	auraagent "github.com/anggasct/aura/internal/agent"
	"github.com/anggasct/aura/internal/config"
	"github.com/anggasct/aura/internal/tools/builtin"
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
			result, err := config.Load(gf.configPath)
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
			for _, definition := range registry.Definitions() {
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
			result, err := config.Load(gf.configPath)
			if err != nil {
				return err
			}
			registry, err := buildAgentRegistry(result.Config)
			if err != nil {
				return err
			}
			definition, ok := registry.Lookup(args[0])
			if !ok {
				return &auraagent.Error{Code: auraagent.ErrorCodeNotFound, Detail: "agent " + args[0] + " is not registered"}
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

// modelRouteResolver maps an agent definition's model route onto the model
// registered for that routing role.
func modelRouteResolver(cfg *config.Config) func(route string) (string, error) {
	return func(route string) (string, error) {
		if r, ok := cfg.ModelRoutes[route]; ok && len(r.Candidates) > 0 {
			candidate := r.Candidates[0]
			if def, defOk := cfg.Models.Definitions[candidate]; defOk && def.Model != "" {
				return def.Model, nil
			}
		}
		definition, ok := cfg.Models.Definitions[route]
		if !ok || definition.Model == "" {
			return "", fmt.Errorf("unknown model route %q", route)
		}
		return definition.Model, nil
	}
}

// buildAgentRegistry validates the compiled-in definitions plus configured
// overrides against the tool registry and configured model routes. An
// invalid definition aborts before any session starts.
func buildAgentRegistry(cfg *config.Config) (*auraagent.Registry, error) {
	routeSet := make(map[string]bool, len(cfg.Models.Definitions)+len(cfg.ModelRoutes))
	for route := range cfg.Models.Definitions {
		routeSet[route] = true
	}
	for route := range cfg.ModelRoutes {
		routeSet[route] = true
	}
	modelRoutes := slices.Collect(maps.Keys(routeSet))
	slices.Sort(modelRoutes)
	var overrides []config.AgentDefinition
	if cfg.Agents != nil {
		overrides = cfg.Agents.Definitions
	}
	return auraagent.Build(overrides, toolsbuiltin.DefinitionNames(), modelRoutes)
}
