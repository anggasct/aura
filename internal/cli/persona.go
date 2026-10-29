package cli

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"

	"github.com/spf13/cobra"

	auraagent "github.com/anggasct/aura/internal/agent"
	"github.com/anggasct/aura/internal/config"
	"github.com/anggasct/aura/internal/runtime"
	"github.com/anggasct/aura/internal/store"
	"github.com/anggasct/aura/internal/telemetry"
)

const soulDigestDisplayLength = 12

type registryPersonas struct {
	registry *auraagent.Registry
}

func (p registryPersonas) Fingerprint(agentID string) (runtime.PersonaFingerprint, bool) {
	definition, ok := p.registry.Lookup(agentID)
	if !ok {
		return runtime.PersonaFingerprint{}, false
	}
	fingerprint := definition.Fingerprint()
	return runtime.PersonaFingerprint{
		AgentID:     fingerprint.AgentID,
		SoulSHA256:  fingerprint.SoulSHA256,
		ToolsSHA256: fingerprint.ToolsSHA256,
		ModelRoute:  fingerprint.ModelRoute,
	}, true
}

func personaTurnHook(recorder *telemetry.PersonaRecorder) postTurnHook {
	return func(ctx context.Context, req *runtime.TurnRequest, _ []store.RuntimeEvent) error {
		recorder.Turn(ctx, req.AgentID)
		return nil
	}
}

func logPersonas(ctx context.Context, logger *slog.Logger, registry *auraagent.Registry) {
	definitions := registry.Definitions()
	for i := range definitions {
		definition := &definitions[i]
		fingerprint := definition.Fingerprint()
		logger.InfoContext(ctx, "persona registered", "component", "persona", "agent_id", definition.ID,
			"soul_sha256", fingerprint.SoulSHA256, "tools_sha256", fingerprint.ToolsSHA256, "model_route", agentRouteLabel(definition.ModelRoute))
	}
}

func resolveChatPersona(ctx context.Context, registry *auraagent.Registry, sessions store.SessionService, sessionID, requested string) (string, error) {
	if requested != "" {
		if _, ok := registry.Lookup(requested); !ok {
			return "", &runtime.Error{Code: runtime.ErrorCodePersonaNotFound, Detail: fmt.Sprintf("persona %q is not registered", requested)}
		}
	}
	if sessionID != "" {
		session, err := sessions.Get(ctx, sessionID)
		switch {
		case err == nil && requested != "" && requested != session.AgentID:
			return "", &runtime.Error{Code: runtime.ErrorCodePersonaMismatch, Detail: fmt.Sprintf("session %s belongs to persona %q, not %q", sessionID, session.AgentID, requested)}
		case err == nil:
			return session.AgentID, nil
		case !errors.Is(err, sql.ErrNoRows):
			return "", err
		}
	}
	if requested != "" {
		return requested, nil
	}
	return auraagent.DefaultID, nil
}

func newPersonaCmd(gf *globalFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "persona",
		Short: "Inspect configured personas",
	}
	cmd.AddCommand(newPersonaListCmd(gf))
	return cmd
}

func newPersonaListCmd(gf *globalFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List personas with their soul source and digest",
		Args:  noPositionalArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			result, err := config.Load(gf.configPath)
			if err != nil {
				return err
			}
			registry, err := buildAgentRegistry(result.Config)
			if err != nil {
				return err
			}
			tbl := newTable(cmd.OutOrStdout())
			tbl.printf("ID\tSOUL\tSOUL SHA256\tMODEL ROUTE\n")
			definitions := registry.Definitions()
			for i := range definitions {
				definition := &definitions[i]
				tbl.printf("%s\t%s\t%s\t%s\n",
					definition.ID,
					soulSource(definition),
					definition.Fingerprint().SoulSHA256[:soulDigestDisplayLength],
					agentRouteLabel(definition.ModelRoute),
				)
			}
			if err := tbl.flush(); err != nil {
				return err
			}
			return cmd.Context().Err()
		},
	}
}

func soulSource(definition *auraagent.Definition) string {
	switch {
	case definition.SoulFile != "":
		return definition.SoulFile
	case definition.Instructions != "":
		return "inline"
	default:
		return "(none)"
	}
}
