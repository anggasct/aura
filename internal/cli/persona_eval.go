package cli

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"

	auraagent "github.com/anggasct/aura/internal/agent"
	"github.com/anggasct/aura/internal/config"
	personaeval "github.com/anggasct/aura/internal/eval/persona"
	"github.com/anggasct/aura/internal/store"
)

func personaEvalDir(configPath string, definition *auraagent.Definition) string {
	base := filepath.Dir(configPath)
	if definition.EvalDir != "" {
		if filepath.IsAbs(definition.EvalDir) {
			return definition.EvalDir
		}
		return filepath.Join(base, definition.EvalDir)
	}
	return filepath.Join(base, "evals", definition.ID)
}

func runPersonaEval(ctx context.Context, db *sql.DB, configPath string, definition *auraagent.Definition) (store.PersonaEvalResult, error) {
	cases, err := personaeval.LoadCases(personaEvalDir(configPath, definition))
	if err != nil {
		return store.PersonaEvalResult{}, err
	}
	report, err := personaeval.Run(ctx, definition, cases, capturePersonaTurn)
	if err != nil {
		return store.PersonaEvalResult{}, err
	}
	return store.NewPersonaEvalStore(db).Record(ctx, &store.PersonaEvalResult{
		AgentID: definition.ID, Digest: definition.EvalDigest(), Layer: store.EvalLayerPolicy, Passed: report.Passed(),
		CasesTotal: report.Total, CasesFailed: report.Failed, FailedCases: report.FailedCases, CreatedAt: time.Now().UTC(),
	})
}

func loadPersona(cfg *config.Config, id string) (auraagent.Definition, error) {
	registry, err := buildAgentRegistry(cfg)
	if err != nil {
		return auraagent.Definition{}, err
	}
	definition, ok := registry.Lookup(id)
	if !ok {
		return auraagent.Definition{}, &exitCodeError{code: 1, err: fmt.Errorf("agent_not_found: no persona has id %q", id)}
	}
	return definition, nil
}

func newPersonaEvalCmd(gf *globalFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "eval <id>",
		Short: "Run a persona's policy evaluation and record the result",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			result, err := config.Load(gf.configPath)
			if err != nil {
				return err
			}
			definition, err := loadPersona(result.Config, args[0])
			if err != nil {
				return err
			}
			db, err := openStorage(cmd.Context(), result.Config)
			if err != nil {
				return err
			}
			defer func() { _ = db.Close() }()
			recorded, err := runPersonaEval(cmd.Context(), db, result.Path, &definition)
			if err != nil {
				return err
			}
			lines := []string{
				"persona: " + definition.ID,
				"digest: " + recorded.Digest[:12],
				fmt.Sprintf("cases: %d, failed: %d", recorded.CasesTotal, recorded.CasesFailed),
			}
			for _, name := range recorded.FailedCases {
				lines = append(lines, "failed: "+name)
			}
			if !recorded.Passed {
				if err := writeChildLines(cmd, append(lines, "result: fail")); err != nil {
					return err
				}
				return &exitCodeError{code: 1, err: errors.New("persona_eval_failed: the policy evaluation did not pass")}
			}
			return writeChildLines(cmd, append(lines, "result: pass"))
		},
	}
}

func newPersonaEvalResultsCmd(gf *globalFlags) *cobra.Command {
	var limit int
	cmd := &cobra.Command{
		Use:   "eval-results <id>",
		Short: "List recorded evaluation results for a persona",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			result, err := config.Load(gf.configPath)
			if err != nil {
				return err
			}
			definition, err := loadPersona(result.Config, args[0])
			if err != nil {
				return err
			}
			db, err := openStorage(cmd.Context(), result.Config)
			if err != nil {
				return err
			}
			defer func() { _ = db.Close() }()
			results, err := store.NewPersonaEvalStore(db).List(cmd.Context(), definition.ID, limit)
			if err != nil {
				return err
			}
			if len(results) == 0 {
				return writeChildLines(cmd, []string{"no evaluation results"})
			}
			current := definition.EvalDigest()
			lines := []string{"CREATED\tLAYER\tRESULT\tCASES\tFAILED\tDIGEST\tCURRENT"}
			for i := range results {
				outcome := "fail"
				if results[i].Passed {
					outcome = "pass"
				}
				lines = append(lines, fmt.Sprintf("%s\t%s\t%s\t%d\t%d\t%s\t%t", results[i].CreatedAt.UTC().Format(time.RFC3339), results[i].Layer, outcome,
					results[i].CasesTotal, results[i].CasesFailed, results[i].Digest[:12], results[i].Digest == current))
			}
			return writeChildLines(cmd, lines)
		},
	}
	cmd.Flags().IntVar(&limit, "limit", 20, "maximum results to list")
	return cmd
}

type personaEvalGate struct {
	registry *auraagent.Registry
	results  store.PersonaEvalStore
}

func (g *personaEvalGate) Evaluated(ctx context.Context, persona string) (bool, error) {
	definition, ok := g.registry.Lookup(persona)
	if !ok {
		return false, nil
	}
	latest, found, err := g.results.Latest(ctx, persona, definition.EvalDigest(), store.EvalLayerPolicy)
	if err != nil {
		return false, err
	}
	return found && latest.Passed, nil
}
