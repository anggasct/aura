package app

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"time"

	"github.com/spf13/cobra"

	appconfig "github.com/anggasct/aura/internal/app/config"
	"github.com/anggasct/aura/internal/channels/discord"
	"github.com/anggasct/aura/internal/channels/kit/access"
	kitapproval "github.com/anggasct/aura/internal/channels/kit/approval"
	toolsbuiltin "github.com/anggasct/aura/internal/modules/builtin"
	"github.com/anggasct/aura/internal/modules/github"
	"github.com/anggasct/aura/internal/modules/workflow"
	auraagent "github.com/anggasct/aura/internal/platform/agent"
	"github.com/anggasct/aura/internal/platform/principal"
	runtimechannelhost "github.com/anggasct/aura/internal/platform/turn/channelhost"
)

func stepToolCatalog(cfg *appconfig.Config) (*toolsbuiltin.StepCatalog, error) {
	credentials := githubCredentials(cfg)
	if len(credentials) == 0 {
		return toolsbuiltin.NewStepCatalog(nil), nil
	}
	definitions, err := github.CheckedDefinitions(credentials)
	if err != nil {
		return nil, fmt.Errorf("prepare github step tools: %w", err)
	}
	return toolsbuiltin.NewStepCatalog(definitions), nil
}

func workflowValidationDeps(cfg *appconfig.Config) (*workflow.ValidationDeps, error) {
	catalog, err := stepToolCatalog(cfg)
	if err != nil {
		return nil, err
	}
	deps := &workflow.ValidationDeps{
		KnownTools:     toolsbuiltin.StepToolNames(personaToolNames(cfg)),
		EffectfulTools: append(toolsbuiltin.EffectfulToolNames(), github.ToolNames()...),
		CheckToolCall:  catalog.CheckArguments,
	}
	if cfg.Workflows != nil {
		deps.DefaultStepTimeout = time.Duration(cfg.Workflows.DefaultStepTimeout)
	}
	registry, err := buildAgentRegistry(cfg)
	if err != nil {
		return deps, err
	}
	deps.Agents = registry
	deps.StepAllowed = stepCeiling(registry, catalog)
	return deps, nil
}

func stepCeiling(registry *auraagent.Registry, steps *toolsbuiltin.StepCatalog) func(agentID string, class principal.Class, toolID string) (allowed, personaKnown bool) {
	return func(agentID string, class principal.Class, toolID string) (bool, bool) {
		persona, known := registry.Lookup(agentID)
		if !known {
			return false, false
		}
		tool, found := steps.Lookup(toolID)
		return found && persona.StepToolAllowed(class, tool), true
	}
}

func workflowDefinitionsDir(cfg *appconfig.Config) string {
	if cfg.Workflows == nil || cfg.Workflows.DefinitionsDir == "" {
		return ""
	}
	return cfg.Workflows.DefinitionsDir
}

type workflowOwnerRouteAdapter struct {
	route *kitapproval.OwnerRoute
}

func newWorkflowOwnerRoute(route *kitapproval.OwnerRoute) workflow.OwnerRouteHolder {
	if route == nil {
		return nil
	}
	return &workflowOwnerRouteAdapter{route: route}
}

func (a *workflowOwnerRouteAdapter) Hold(sessionID string, requester *workflow.StepRequester) func() {
	if a == nil || a.route == nil || requester == nil {
		return func() {}
	}
	return a.route.Hold(sessionID, &kitapproval.Requester{
		Principal: requester.Principal,
		Class:     requester.Class,
		Persona:   requester.Persona,
	})
}

type replyTargeter interface {
	ReplyTarget(sessionID string) (access.HandoffTarget, bool)
}

type discordReplyTargeterAdapter struct {
	target replyTargeter
}

func (a *discordReplyTargeterAdapter) DiscordReplyTarget(sessionID string) (account, channelID string, ok bool) {
	target, found := a.target.ReplyTarget(sessionID)
	if !found || target.Channel != principal.ChannelDiscord {
		return "", "", false
	}
	return target.Account, target.ChannelID, true
}

func workflowReplyTargeters(ports []runtimechannelhost.ChannelPort) []workflow.ReplyTargeter {
	var targeters []workflow.ReplyTargeter
	for _, port := range ports {
		if targeter, ok := port.(replyTargeter); ok {
			targeters = append(targeters, &discordReplyTargeterAdapter{target: targeter})
		}
	}
	return targeters
}

type proposalOfferer interface {
	OfferProposal(ctx context.Context, card discord.ProposalCard, decide discord.ProposalDecider) error
	ReplyTarget(sessionID string) (access.HandoffTarget, bool)
}

type discordProposalOffererAdapter struct {
	offerer proposalOfferer
}

func (a *discordProposalOffererAdapter) OfferProposal(ctx context.Context, card workflow.ProposalCard, decide func(ctx context.Context, approved bool) (string, error)) error {
	return a.offerer.OfferProposal(ctx, discord.ProposalCard{ID: card.ID, Plan: card.Plan, Tainted: card.Tainted}, decide)
}

func (a *discordProposalOffererAdapter) ServesSession(sessionID string) bool {
	_, ok := a.offerer.ReplyTarget(sessionID)
	return ok
}

func workflowProposalOfferers(ports []runtimechannelhost.ChannelPort) []workflow.ProposalOfferer {
	var offerers []workflow.ProposalOfferer
	for _, port := range ports {
		if offerer, ok := port.(proposalOfferer); ok {
			offerers = append(offerers, &discordProposalOffererAdapter{offerer: offerer})
		}
	}
	return offerers
}

type workflowSessionTextAppender struct {
	db *sql.DB
}

func (a *workflowSessionTextAppender) AppendSessionText(ctx context.Context, sessionID, id, author, text string, at time.Time) error {
	return appendSessionText(ctx, a.db, sessionID, id, author, text, at)
}

type workflowCompletionDeliverer struct {
	cfg     *appconfig.Config
	deliver func(ctx context.Context, account *discord.Account, channelID, text string) error
}

func (d *workflowCompletionDeliverer) DeliverDiscordText(ctx context.Context, accountName, channelID, text string) error {
	account, ok := discordAccount(d.cfg, accountName)
	if !ok {
		return fmt.Errorf("discord account %q is not configured", accountName)
	}
	deliver := d.deliver
	if deliver == nil {
		deliver = deliverText
	}
	return deliver(ctx, account, channelID, text)
}

func workflowOpener(gf *globalFlags) workflow.CommandOpener {
	return func(cmd *cobra.Command, mode workflow.CommandMode, fn func(ctx context.Context, deps *workflow.CommandDeps) error) error {
		result, err := appconfig.Load(gf.configPath)
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
				secrets := slices.Concat(toolsbuiltin.ConfiguredToolSecrets(result.Config.Tools), githubCredentialSecrets(result.Config))
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
