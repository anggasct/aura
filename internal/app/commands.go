package app

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"slices"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	appconfig "github.com/anggasct/aura/internal/app/config"
	"github.com/anggasct/aura/internal/channels/terminal"
	"github.com/anggasct/aura/internal/modules/broadcast"
	"github.com/anggasct/aura/internal/modules/child"
	"github.com/anggasct/aura/internal/modules/codeexec"
	"github.com/anggasct/aura/internal/modules/codeexec/interpreter"
	personaeval "github.com/anggasct/aura/internal/modules/eval/persona"
	"github.com/anggasct/aura/internal/modules/gitsync"
	"github.com/anggasct/aura/internal/modules/handoff"
	"github.com/anggasct/aura/internal/modules/knowledge"
	"github.com/anggasct/aura/internal/modules/mcp"
	"github.com/anggasct/aura/internal/modules/memory"
	"github.com/anggasct/aura/internal/modules/plugin"
	"github.com/anggasct/aura/internal/modules/profile"
	"github.com/anggasct/aura/internal/modules/schedule"
	"github.com/anggasct/aura/internal/modules/shellexec"
	"github.com/anggasct/aura/internal/modules/skills"
	"github.com/anggasct/aura/internal/modules/workflow"
	auraagent "github.com/anggasct/aura/internal/platform/agent"
	"github.com/anggasct/aura/internal/platform/capability"
	"github.com/anggasct/aura/internal/platform/clikit"
	"github.com/anggasct/aura/internal/platform/command"
	"github.com/anggasct/aura/internal/platform/effect"
	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/logging"
	"github.com/anggasct/aura/internal/platform/model"
	"github.com/anggasct/aura/internal/platform/operation"
	"github.com/anggasct/aura/internal/platform/principal"
	"github.com/anggasct/aura/internal/platform/storage"
	"github.com/anggasct/aura/internal/platform/toolbroker"
	"github.com/anggasct/aura/internal/platform/tools"
	turnstore "github.com/anggasct/aura/internal/platform/turn/store"
	"github.com/anggasct/aura/internal/platform/usage"
)

const (
	mcpTrustDirName  = "mcp"
	mcpTrustFileName = "trust.json"
)

type GlobalFlags struct {
	ConfigPath string
	LogLevel   string
	LogFormat  string
}

func NewUsageError(err error) error {
	return clikit.NewUsageError(err)
}

func IsInterrupted(err error) bool {
	return errors.Is(err, terminal.ErrInterrupted)
}

func ExitCode(root *cobra.Command, args []string, err error) int {
	if err == nil {
		return 0
	}
	if clikit.IsUsageError(err) || command.IsUsageError(err) {
		return 2
	}
	var ue interface{ UsageError() bool }
	if errors.As(err, &ue) && ue.UsageError() {
		return 2
	}
	var flagged interface{ IsUsageError() bool }
	if errors.As(err, &flagged) && flagged.IsUsageError() {
		return 2
	}
	if IsInterrupted(err) {
		return 128 + int(syscall.SIGINT)
	}
	if code, ok := clikit.ExitCodeOf(err); ok {
		return code
	}
	var coded interface{ ExitCode() int }
	if errors.As(err, &coded) {
		return coded.ExitCode()
	}
	if root != nil {
		if _, _, findErr := root.Find(args); findErr != nil {
			return 2
		}
	}
	return 1
}

func MountCommands(root *cobra.Command, gf *GlobalFlags) {
	if gf == nil {
		gf = &GlobalFlags{}
	}
	root.AddCommand(storage.Commands(storageOpener(gf))...)
	root.AddCommand(usage.Commands(usageOpener(gf))...)
	root.AddCommand(effect.Commands(effectOpener(gf))...)
	root.AddCommand(auraagent.Commands(agentOpener(gf), personaeval.Commands(personaEvalOpener(gf))...)...)
	root.AddCommand(principal.Commands(principalOpener(gf))...)
	root.AddCommand(model.Commands(modelOpener(gf))...)
	root.AddCommand(shellexec.Commands(NewUsageError)...)
	root.AddCommand(gitsync.Commands(syncOpener(gf))...)
	root.AddCommand(knowledge.Commands(knowledgeOpener(gf))...)
	root.AddCommand(memory.Commands(memoryOpener(gf))...)
	root.AddCommand(profile.Commands(profileOpener(gf))...)
	root.AddCommand(handoff.Commands(handoffOpener(gf))...)
	root.AddCommand(broadcast.Commands(broadcastOpener(gf))...)
	root.AddCommand(plugin.Commands(pluginOpener(gf))...)
	root.AddCommand(mcp.Commands(mcpOpener(gf))...)
	root.AddCommand(skills.Commands(skillsOpener(gf))...)
	root.AddCommand(workflow.Commands(workflowOpener(gf))...)
	root.AddCommand(codeexec.Commands(interpreterOpener(gf))...)
	root.AddCommand(child.Commands(childOpener(gf))...)
	mountOperationCommands(root, gf)
}

func loadCommandConfig(cmd *cobra.Command, gf *GlobalFlags) (appconfig.LoadResult, *slog.Logger, error) {
	result, err := appconfig.Load(gf.ConfigPath)
	if err != nil {
		return appconfig.LoadResult{}, nil, err
	}
	level, format := resolveLogging(cmd, result.Config)
	logger, err := logging.Setup(level, format, cmd.ErrOrStderr())
	if err != nil {
		return appconfig.LoadResult{}, nil, err
	}
	logConfigResult(cmd.Context(), logger, &result)
	return result, logger, nil
}

func storageOpener(gf *GlobalFlags) storage.CommandOpener {
	return func(cmd *cobra.Command, needDB bool, fn func(ctx context.Context, env *storage.CommandEnv) error) error {
		if !needDB {
			result, logger, err := loadCommandConfig(cmd, gf)
			if err != nil {
				return err
			}
			return fn(cmd.Context(), &storage.CommandEnv{
				Logger:     logger,
				Config:     &result.Config.Storage,
				Migrations: Migrations(),
			})
		}
		result, err := appconfig.Load(gf.ConfigPath)
		if err != nil {
			return err
		}
		return withApp(cmd, &commandInput{result: &result}, func(ctx context.Context, app *App) error {
			return fn(ctx, &storage.CommandEnv{
				Logger:     app.logger,
				Config:     &result.Config.Storage,
				DB:         app.db,
				Migrations: Migrations(),
			})
		})
	}
}

func modelOpener(gf *GlobalFlags) model.CommandOpener {
	return func(cmd *cobra.Command, needDB bool, fn func(ctx context.Context, env *model.CommandEnv) error) error {
		if !needDB {
			result, _, err := loadCommandConfig(cmd, gf)
			if err != nil {
				return err
			}
			return fn(cmd.Context(), &model.CommandEnv{
				Models: result.Config.Models,
				Routes: result.Config.ModelRoutes,
			})
		}
		result, err := appconfig.Load(gf.ConfigPath)
		if err != nil {
			return err
		}
		return withApp(cmd, &commandInput{result: &result}, func(ctx context.Context, app *App) error {
			return fn(ctx, &model.CommandEnv{
				Models: result.Config.Models,
				Routes: result.Config.ModelRoutes,
				DB:     app.db,
			})
		})
	}
}

func agentOpener(gf *GlobalFlags) auraagent.CommandOpener {
	return func(cmd *cobra.Command, fn func(ctx context.Context, env *auraagent.CommandEnv) error) error {
		result, _, err := loadCommandConfig(cmd, gf)
		if err != nil {
			return err
		}
		registry, err := buildAgentRegistry(result.Config)
		if err != nil {
			return err
		}
		return fn(cmd.Context(), &auraagent.CommandEnv{
			Registry: registry,
			Accounts: channelAccounts(&result.Config.Channels),
			Bindings: result.Config.Bindings,
		})
	}
}

func usageOpener(gf *GlobalFlags) usage.CommandOpener {
	return func(cmd *cobra.Command, pricesPath string, fn func(ctx context.Context, env *usage.CommandEnv) error) error {
		result, err := appconfig.Load(gf.ConfigPath)
		if err != nil {
			return err
		}
		return withApp(cmd, &commandInput{result: &result}, func(ctx context.Context, app *App) error {
			reg, err := openPriceRegistry(ctx, app.logger, result.Config, result.Path, pricesPath)
			if err != nil {
				return err
			}
			ledger, err := usage.NewTurnLedger(result.Config.Usage, result.Config.Access, app.db, reg, app.logger)
			if err != nil {
				return err
			}
			return fn(ctx, &usage.CommandEnv{
				Logger: app.logger,
				Ledger: ledger,
				Prices: reg,
			})
		})
	}
}

func effectOpener(gf *GlobalFlags) effect.CommandOpener {
	return func(cmd *cobra.Command, fn func(ctx context.Context, env *effect.CommandEnv) error) error {
		result, err := appconfig.Load(gf.ConfigPath)
		if err != nil {
			return err
		}
		return withApp(cmd, &commandInput{result: &result}, func(ctx context.Context, app *App) error {
			journal, err := effect.NewJournal(app.db, effect.Options{Logger: app.logger, Turns: app.turns})
			if err != nil {
				return err
			}
			return fn(ctx, &effect.CommandEnv{
				Logger:  app.logger,
				Journal: journal,
			})
		})
	}
}

type registryPersonaChecker struct {
	registry *auraagent.Registry
}

func (c registryPersonaChecker) Exists(personaID string) bool {
	_, ok := c.registry.Lookup(personaID)
	return ok
}

type turnStoreActivityReader struct {
	turns *turnstore.TurnStore
}

func (r turnStoreActivityReader) PrincipalActivity(ctx context.Context) ([]principal.PrincipalActivity, error) {
	rows, err := r.turns.PrincipalActivity(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]principal.PrincipalActivity, 0, len(rows))
	for i := range rows {
		out = append(out, principal.PrincipalActivity{
			PrincipalID: rows[i].PrincipalID,
			Sessions:    rows[i].Sessions,
			LastActive:  rows[i].LastActive,
		})
	}
	return out, nil
}

func principalOpener(gf *GlobalFlags) principal.CommandOpener {
	return func(cmd *cobra.Command, mode principal.CommandMode, fn func(ctx context.Context, env *principal.CommandEnv) error) error {
		if mode == principal.CommandModeConfig {
			result, logger, err := loadCommandConfig(cmd, gf)
			if err != nil {
				return err
			}
			owners, err := ownerResolver(result.Config)
			if err != nil {
				return err
			}
			return fn(cmd.Context(), &principal.CommandEnv{
				Logger: logger,
				Owners: owners,
			})
		}
		result, err := appconfig.Load(gf.ConfigPath)
		if err != nil {
			return err
		}
		var parts []Part
		if mode == principal.CommandModeForgetApply {
			parts = []Part{PartWorkflows}
		}
		return withApp(cmd, &commandInput{result: &result, parts: parts}, func(ctx context.Context, app *App) error {
			owners, err := ownerResolver(result.Config)
			if err != nil {
				return err
			}
			registry, err := buildAgentRegistry(result.Config)
			if err != nil {
				return err
			}
			pairings, err := principal.NewPairingStore(app.db, app.turns)
			if err != nil {
				return err
			}
			erasers, err := erasureParticipants(app.db)
			if err != nil {
				return err
			}
			rekeyers, err := rekeyParticipants(app.db)
			if err != nil {
				return err
			}
			_, artifactRoot, _, err := storage.ResolvePaths(&result.Config.Storage)
			if err != nil {
				return err
			}
			var liveRuns principal.LiveRunCanceller
			if mode == principal.CommandModeForgetApply {
				runs := &erasureRuns{}
				if app.interpreter != nil {
					runs.set(app.interpreter)
				}
				liveRuns = &liveRunCancellerAdapter{db: app.db, runs: runs}
			}
			return fn(ctx, &principal.CommandEnv{
				Logger:   app.logger,
				DB:       app.db,
				Owners:   owners,
				Actions:  turnstore.NewOwnerActionRecorder(app.db),
				Personas: registryPersonaChecker{registry: registry},
				PersonaNotFound: func(personaID string) error {
					return fault.New(auraagent.ErrorCodeNotFound, "agent "+personaID+" is not registered")
				},
				Activity:    turnStoreActivityReader{turns: app.turns},
				Allowlisted: allowlistedPrincipals(result.Config, owners),
				Pairings:    pairings,
				LiveRuns:    liveRuns,
				Erasers:     erasers,
				Rekeyers:    rekeyers,
				RekeyMoves: func(osUser string) ([]principal.OwnerMove, error) {
					return ownerRekeyMoves(result.Config, osUser)
				},
				ArtifactRoot: artifactRoot,
				CollectBlobs: collectErasedBlobs,
			})
		})
	}
}

func childOpener(gf *GlobalFlags) child.Opener {
	return func(cmd *cobra.Command, needsDurable bool, fn func(ctx context.Context, deps *child.CommandDeps) error) error {
		result, err := appconfig.Load(gf.ConfigPath)
		if err != nil {
			return err
		}
		if result.Config.Children == nil || !result.Config.Children.Enabled {
			return errors.New("subagent runner is not enabled")
		}
		var parts []Part
		if needsDurable {
			parts = []Part{PartDurable}
		}
		return withApp(cmd, &commandInput{result: &result, parts: parts}, func(ctx context.Context, app *App) error {
			store, err := child.NewSQLiteStore(app.db)
			if err != nil {
				return err
			}
			deps := &child.CommandDeps{Store: store}
			if needsDurable {
				registry, err := child.NewStoreRegistry(store, turnstore.NewSessionService(app.db))
				if err != nil {
					return err
				}
				runs, err := child.NewDurableCanceller(app.runtime)
				if err != nil {
					return err
				}
				canceller, err := child.NewCancelService(registry, runs)
				if err != nil {
					return err
				}
				deps.Canceller = canceller
			}
			return fn(ctx, deps)
		})
	}
}

func mcpTrustRegistryPath(cfg *appconfig.Config) (string, error) {
	dataRoot, err := storage.ResolveDataRoot(&cfg.Storage)
	if err != nil {
		return "", err
	}
	return filepath.Join(dataRoot, mcpTrustDirName, mcpTrustFileName), nil
}

func configuredMCPServers(cfg *appconfig.Config) []mcp.ServerConfig {
	if cfg == nil || cfg.MCP == nil {
		return nil
	}
	return cfg.MCP.Servers
}

func mcpOpener(gf *GlobalFlags) mcp.Opener {
	return func(cmd *cobra.Command, fn func(ctx context.Context, servers []mcp.ServerConfig, registry *mcp.FileTrustRegistry) error) error {
		result, err := appconfig.Load(gf.ConfigPath)
		if err != nil {
			return err
		}
		trustPath, err := mcpTrustRegistryPath(result.Config)
		if err != nil {
			return err
		}
		registry, err := mcp.NewFileTrustRegistry(trustPath)
		if err != nil {
			return err
		}
		return fn(cmd.Context(), configuredMCPServers(result.Config), registry)
	}
}

func skillsOpener(gf *GlobalFlags) skills.Opener {
	return func(cmd *cobra.Command, fn func(ctx context.Context, engine *skills.Engine, defaultRoot string) error) error {
		result, err := appconfig.Load(gf.ConfigPath)
		if err != nil {
			return err
		}
		return withApp(cmd, &commandInput{result: &result, parts: []Part{PartSkills}}, func(ctx context.Context, app *App) error {
			return fn(ctx, app.skillsEngine, app.skillsRoot)
		})
	}
}

func knowledgeOpener(gf *GlobalFlags) knowledge.Opener {
	return func(cmd *cobra.Command, fn func(ctx context.Context, logger *slog.Logger, service *knowledge.Service) error) error {
		result, err := appconfig.Load(gf.ConfigPath)
		if err != nil {
			return err
		}
		return withApp(cmd, &commandInput{result: &result, parts: []Part{PartKnowledge}}, func(ctx context.Context, app *App) error {
			return fn(ctx, app.logger, app.knowledge)
		})
	}
}

func memoryOpener(gf *GlobalFlags) memory.Opener {
	return func(cmd *cobra.Command, fn func(ctx context.Context, logger *slog.Logger, service *memory.Service) error) error {
		result, err := appconfig.Load(gf.ConfigPath)
		if err != nil {
			return err
		}
		return withApp(cmd, &commandInput{result: &result, parts: []Part{PartMemory}}, func(ctx context.Context, app *App) error {
			return fn(ctx, app.logger, app.memory)
		})
	}
}

func profileOpener(gf *GlobalFlags) profile.Opener {
	return func(cmd *cobra.Command, fn func(ctx context.Context, service *profile.Service) error) error {
		result, err := appconfig.Load(gf.ConfigPath)
		if err != nil {
			return err
		}
		if result.Config.Profile == nil || !result.Config.Profile.Enabled {
			return errors.New("user profiling is not enabled")
		}
		return withApp(cmd, &commandInput{result: &result, parts: []Part{PartProfile}}, func(ctx context.Context, app *App) error {
			return fn(ctx, app.profile)
		})
	}
}

func pluginOpener(gf *GlobalFlags) plugin.Opener {
	return func(cmd *cobra.Command, fn func(ctx context.Context, service *plugin.Service, audit plugin.AuditRecorder) error) error {
		result, err := appconfig.Load(gf.ConfigPath)
		if err != nil {
			return err
		}
		return withApp(cmd, &commandInput{result: &result, parts: []Part{PartPlugins}}, func(ctx context.Context, app *App) error {
			return fn(ctx, app.plugins, &pluginAuditAdapter{db: app.db})
		})
	}
}

func syncOpener(gf *GlobalFlags) gitsync.Opener {
	return func(cmd *cobra.Command, fn func(ctx context.Context, env gitsync.CommandEnv) error) error {
		result, err := appconfig.Load(gf.ConfigPath)
		if err != nil {
			return err
		}
		if result.Config.Sync == nil {
			return gitsync.Errorf(gitsync.ErrorCodeNotConfigured, "sync is not configured")
		}
		_, dataRoot, _, err := storage.ResolvePaths(&result.Config.Storage)
		if err != nil {
			return err
		}
		return fn(cmd.Context(), gitsync.CommandEnv{
			Config:   result.Config.Sync,
			DataRoot: dataRoot,
		})
	}
}

func broadcastOpener(gf *GlobalFlags) broadcast.Opener {
	return func(cmd *cobra.Command, needDurable bool, fn func(ctx context.Context, env *broadcast.CommandEnv) error) error {
		result, err := appconfig.Load(gf.ConfigPath)
		if err != nil {
			return err
		}
		var parts []Part
		if needDurable {
			parts = []Part{PartDurable}
		}
		return withApp(cmd, &commandInput{result: &result, parts: parts}, func(ctx context.Context, app *App) error {
			items, err := broadcast.NewSQLiteStore(app.db)
			if err != nil {
				return err
			}
			env := &broadcast.CommandEnv{
				Store:   items,
				Runtime: app.runtime,
				Effects: func() (broadcast.EffectReconciler, error) {
					executor, err := newChannelEffects(app.db, app.logger)
					if err != nil {
						return nil, err
					}
					return &broadcastEffectReconciler{executor: executor}, nil
				},
				EffectProviders: func() (map[string]effect.Provider, error) {
					return broadcastEffectProviders(app.result.Config, app.db, app.logger)
				},
			}
			return fn(ctx, env)
		})
	}
}

func operationCatalogs() []operation.Catalog {
	return []operation.Catalog{schedule.Catalog()}
}

func operationToolNames() []string {
	names := []string{tools.OperationSearchToolName}
	for _, catalog := range operationCatalogs() {
		for i := range catalog.Descriptors {
			if catalog.Descriptors[i].GeneratesTool() {
				names = append(names, operation.ToolName(catalog.Descriptors[i].ID))
			}
		}
	}
	slices.Sort(names)
	return names
}

func IsOperationCommand(cmd *cobra.Command) bool {
	return cmd != nil && cmd.Annotations[operation.AnnotationOperation] != ""
}

func mountOperationCommands(root *cobra.Command, gf *GlobalFlags) {
	catalogs := operationCatalogs()
	commands, err := operation.Commands(catalogs, operationOpener(gf))
	if err != nil {
		for i := range catalogs {
			root.AddCommand(refusedGroup(catalogs[i].Group, err))
		}
		return
	}
	root.AddCommand(commands...)
}

func refusedGroup(group operation.Group, cause error) *cobra.Command {
	return &cobra.Command{
		Use:                group.Name,
		Short:              group.Summary,
		DisableFlagParsing: true,
		RunE:               func(*cobra.Command, []string) error { return cause },
	}
}

func commandParts(needs []string) []Part {
	parts := make([]Part, 0, len(needs))
	for _, need := range needs {
		parts = append(parts, Part(need))
	}
	return parts
}

func operationOpener(gf *GlobalFlags) operation.Opener {
	return func(cmd *cobra.Command, descriptor *operation.Descriptor, run func(ctx context.Context, invoker operation.Invoker) error) error {
		result, err := appconfig.Load(gf.ConfigPath)
		if err != nil {
			return err
		}
		input := &commandInput{result: &result, parts: commandParts(descriptor.Needs)}
		return withApp(cmd, input, func(ctx context.Context, app *App) error {
			registry, err := app.commandOperations()
			if err != nil {
				return err
			}
			return run(ctx, registry)
		})
	}
}

func (a *App) commandOperations() (*operation.Registry, error) {
	cfg := a.result.Config
	jobs, err := schedule.NewSQLiteStore(a.db)
	if err != nil {
		return nil, err
	}
	service, err := schedule.NewService(jobs)
	if err != nil {
		return nil, err
	}
	operations, err := schedule.NewOperations(&schedule.OperationDeps{
		Config:  cfg.Scheduler,
		Store:   jobs,
		Service: service,
		Starter: a.runtime,
		WorkflowChecker: func(ctx context.Context, id string) error {
			return checkScheduledWorkflow(ctx, cfg, a.db, id)
		},
		Owner: &cfg.Broadcast,
	})
	if err != nil {
		return nil, err
	}
	effects, err := newChannelEffects(a.db, a.logger)
	if err != nil {
		return nil, err
	}
	lookup, err := toolbroker.New(&toolbroker.Options{Effects: effects})
	if err != nil {
		return nil, err
	}
	return operation.NewRegistry(operationCatalogs(), operations.Bindings(), &operation.Deps{
		DB:      a.db,
		Effects: effects,
		Tools:   lookup,
		Logger:  a.logger,
	})
}

func handoffOpener(gf *GlobalFlags) handoff.CommandOpener {
	return func(cmd *cobra.Command, fn func(ctx context.Context, deps *handoff.CommandDeps) error) error {
		result, err := appconfig.Load(gf.ConfigPath)
		if err != nil {
			return err
		}
		return withApp(cmd, &commandInput{result: &result}, func(ctx context.Context, app *App) error {
			store, err := handoff.NewSQLiteStore(app.db)
			if err != nil {
				return err
			}
			registry, err := buildAgentRegistry(result.Config)
			if err != nil {
				return err
			}
			actions := turnstore.NewOwnerActionRecorder(app.db)
			return fn(ctx, &handoff.CommandDeps{
				Store: store,
				Deliver: func(ctx context.Context, h *handoff.Handoff, text string) error {
					return deliverToHandoff(ctx, result.Config, h, text)
				},
				PersonaAuthor: func(agentID string) string {
					definition, _ := registry.Lookup(agentID)
					return cmp.Or(definition.Name, handoffReplyAuthorFallback)
				},
				ClosedMessage: func(agentID string) string {
					definition, _ := registry.Lookup(agentID)
					return definition.Message(auraagent.MessageHandoffClosed)
				},
				AppendReply: func(ctx context.Context, h *handoff.Handoff, author, text string, at time.Time) error {
					return appendPersonaReply(ctx, app.db, h, author, text, at)
				},
				RecordAction: func(ctx context.Context, action string, h *handoff.Handoff, eventID string, at time.Time) error {
					return actions.RecordAccessAction(ctx, &turnstore.AccessAction{
						Action:    action,
						Principal: h.PrincipalID,
						Persona:   h.AgentID,
						Account:   h.Account,
					}, eventID, at)
				},
				Logger: app.logger,
			})
		})
	}
}

func workflowOpener(gf *GlobalFlags) workflow.CommandOpener {
	return func(cmd *cobra.Command, mode workflow.CommandMode, fn func(ctx context.Context, deps *workflow.CommandDeps) error) error {
		result, err := appconfig.Load(gf.ConfigPath)
		if err != nil {
			return err
		}
		switch mode {
		case workflow.CommandModeConfig:
			return fn(cmd.Context(), &workflow.CommandDeps{
				DefinitionsDir: workflowDefinitionsDir(result.Config),
				ValidationDeps: func() (*workflow.ValidationDeps, error) {
					return workflowValidationDeps(result.Config)
				},
				Config: result.Config.Workflows,
			})
		case workflow.CommandModeStorage:
			return withApp(cmd, &commandInput{result: &result}, func(ctx context.Context, app *App) error {
				runs, err := workflow.NewStore(app.db, app.turns)
				if err != nil {
					return err
				}
				return fn(ctx, &workflow.CommandDeps{
					DefinitionsDir: workflowDefinitionsDir(result.Config),
					Store:          runs,
					Config:         result.Config.Workflows,
				})
			})
		case workflow.CommandModeRuntime:
			return withApp(cmd, &commandInput{result: &result, parts: []Part{PartWorkflows}}, func(ctx context.Context, app *App) error {
				runs, err := workflow.NewStore(app.db, app.turns)
				if err != nil {
					return err
				}
				secrets := slices.Concat(configuredToolSecrets(result.Config.Tools), githubCredentialSecrets(result.Config))
				return fn(ctx, &workflow.CommandDeps{
					DefinitionsDir: workflowDefinitionsDir(result.Config),
					ValidationDeps: func() (*workflow.ValidationDeps, error) {
						return workflowValidationDeps(result.Config)
					},
					Store:       runs,
					Interpreter: app.interpreter,
					Config:      result.Config.Workflows,
					Secrets:     secrets,
				})
			})
		default:
			return invalidOptions("unsupported workflow command mode")
		}
	}
}

type configLoader func(path string) (appconfig.LoadResult, error)

type interpreterDeps struct {
	load              configLoader
	manifest          func() (*interpreter.Manifest, error)
	platformSupported func() bool
}

func interpreterOpener(gf *GlobalFlags) codeexec.Opener {
	return newInterpreterOpener(gf, interpreterDeps{
		load:              appconfig.Load,
		manifest:          interpreter.DefaultManifest,
		platformSupported: interpreter.HostSupported,
	})
}

func newInterpreterOpener(gf *GlobalFlags, deps interpreterDeps) codeexec.Opener {
	return func(cmd *cobra.Command, fn func(ctx context.Context, logger *slog.Logger, store *interpreter.Store) error) error {
		result, err := deps.load(gf.ConfigPath)
		if err != nil {
			return err
		}
		level, format := resolveLogging(cmd, result.Config)
		logger, err := logging.Setup(level, format, cmd.ErrOrStderr())
		if err != nil {
			return err
		}
		logConfigResult(cmd.Context(), logger, &result)
		if err := requireInterpreterCapability(result.CapabilityReport, deps.platformSupported()); err != nil {
			return err
		}
		manifest, err := deps.manifest()
		if err != nil {
			return err
		}
		dataRoot, err := storage.ResolveDataRoot(&result.Config.Storage)
		if err != nil {
			return err
		}
		store, err := interpreter.NewStore(dataRoot, manifest)
		if err != nil {
			return err
		}
		return fn(cmd.Context(), logger, store)
	}
}

func requireInterpreterCapability(report capability.Report, platformSupported bool) error {
	status, ok := report.Status(codeexec.CapabilityName)
	if !ok || !status.Compiled {
		return fault.New(capability.ErrorCodeCapabilityUnavailable, codeexec.CapabilityName+": capability is absent from this artifact")
	}
	if !platformSupported {
		return fault.New(capability.ErrorCodeCapabilityUnavailable, codeexec.CapabilityName+": runtimes need linux/amd64")
	}
	return nil
}

func personaEvalOpener(gf *GlobalFlags) personaeval.Opener {
	return func(cmd *cobra.Command, req personaeval.CommandRequest, fn func(ctx context.Context, env personaeval.CommandEnv) error) error {
		result, err := appconfig.Load(gf.ConfigPath)
		if err != nil {
			return err
		}
		definition, err := loadPersona(result.Config, req.PersonaID)
		if err != nil {
			return err
		}
		var parts []Part
		if req.NeedBehavior && definition.EvalBehavior != nil {
			parts = []Part{PartModels}
		}
		return withApp(cmd, &commandInput{result: &result, parts: parts}, func(ctx context.Context, app *App) error {
			evalStore, err := personaeval.NewSQLiteStore(app.db)
			if err != nil {
				return err
			}
			deps := &personaeval.ServiceDeps{
				Store:    evalStore,
				Capturer: newPersonaCapturer(result.Config).capture,
				Catalog:  appconfig.BuiltinToolDefinitions(),
			}
			if req.NeedBehavior && definition.EvalBehavior != nil {
				models, err := routedBehaviorModels(result.Config, app.models)
				if err != nil {
					return err
				}
				deps.Converse = models.converse
				deps.JudgeFor = models.judge
			}
			svc, err := personaeval.NewService(deps)
			if err != nil {
				return err
			}
			return fn(ctx, personaeval.CommandEnv{
				ConfigPath: result.Path,
				Definition: definition,
				Service:    svc,
			})
		})
	}
}

func loadPersona(cfg *appconfig.Config, id string) (auraagent.Definition, error) {
	registry, err := buildAgentRegistry(cfg)
	if err != nil {
		return auraagent.Definition{}, err
	}
	definition, ok := registry.Lookup(id)
	if !ok {
		return auraagent.Definition{}, fmt.Errorf("agent_not_found: no persona has id %q", id)
	}
	return definition, nil
}
