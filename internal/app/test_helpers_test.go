package app

import (
	"bytes"
	"context"
	"database/sql"
	"os"
	"testing"
	"time"

	"github.com/spf13/cobra"

	appconfig "github.com/anggasct/aura/internal/app/config"
	"github.com/anggasct/aura/internal/channels/terminal"
	personaeval "github.com/anggasct/aura/internal/modules/eval/persona"
	"github.com/anggasct/aura/internal/modules/handoff"
	auraagent "github.com/anggasct/aura/internal/platform/agent"
	"github.com/anggasct/aura/internal/platform/health"
	"github.com/anggasct/aura/internal/platform/model"
	"github.com/anggasct/aura/internal/platform/principal"
	"github.com/anggasct/aura/internal/platform/secret"
	"github.com/anggasct/aura/internal/platform/storage"
	turnstore "github.com/anggasct/aura/internal/platform/turn/store"
)

type accessAction = turnstore.AccessAction

const (
	exitHealthy   = health.ExitHealthy
	exitDegraded  = health.ExitDegraded
	exitCritical  = health.ExitCritical
	exitCommand   = health.ExitCommand
	exitNoPersona = auraagent.ExitNoPersona

	liveDatabaseFilename = storage.LiveDatabaseFilename
	apiKeyLabelMaxLength = principal.APIKeyLabelMaxLength

	accessActionStandingGrant       = principal.AccessActionStandingGrant
	accessActionStandingGrantRevoke = principal.AccessActionStandingGrantRevoke
	accessActionAPIKeyCreate        = principal.AccessActionAPIKeyCreate
	accessActionAPIKeyRevoke        = principal.AccessActionAPIKeyRevoke
	accessActionPrincipalGrant      = principal.AccessActionPrincipalGrant
	accessActionPrincipalRevoke     = principal.AccessActionPrincipalRevoke
	accessActionPrincipalForget     = principal.AccessActionPrincipalForget
	accessActionHandoffReply        = handoff.ActionReply
	accessActionHandoffRelease      = handoff.ActionRelease

	ownerRekeyEventKind     = "principal.rekey.v1"
	pairingCodeLength       = 8
	pairingLockoutFailures  = 5
	soulDigestDisplayLength = 12
)

func NewRootCommand() *cobra.Command {
	gf := &GlobalFlags{}
	root := &cobra.Command{
		Use:           "aura",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.SetFlagErrorFunc(func(_ *cobra.Command, err error) error {
		return NewUsageError(err)
	})
	root.PersistentFlags().StringVar(&gf.ConfigPath, "config", "", "")
	root.PersistentFlags().StringVar(&gf.LogLevel, "log-level", "", "")
	root.PersistentFlags().StringVar(&gf.LogFormat, "log-format", "", "")
	MountCommands(root, gf)
	return root
}

func execute(t *testing.T, args ...string) (stdout string, err error) {
	t.Helper()
	root := NewRootCommand()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs(args)
	err = root.Execute()
	return out.String(), err
}

func exitCodeOf(t *testing.T, args ...string) int {
	t.Helper()
	root := NewRootCommand()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs(args)
	return ExitCode(root, args, root.Execute())
}

func findSubcommand(cmds []*cobra.Command, name string) *cobra.Command {
	for _, cmd := range cmds {
		if cmd.Name() == name {
			return cmd
		}
	}
	panic("command not found: " + name)
}

func newGrantsCmd(gf *GlobalFlags) *cobra.Command {
	return findSubcommand(principal.Commands(principalOpener(gf)), "grants")
}

func newAPIKeyCmd(gf *GlobalFlags) *cobra.Command {
	return findSubcommand(principal.Commands(principalOpener(gf)), "apikey")
}

func newPairingCmd(gf *GlobalFlags) *cobra.Command {
	return findSubcommand(principal.Commands(principalOpener(gf)), "pairing")
}

func newPrincipalCmd(gf *GlobalFlags) *cobra.Command {
	return findSubcommand(principal.Commands(principalOpener(gf)), "principal")
}

func newOwnerCmd(gf *GlobalFlags) *cobra.Command {
	return findSubcommand(principal.Commands(principalOpener(gf)), "owner")
}

func newModelsCmd(gf *GlobalFlags) *cobra.Command {
	return findSubcommand(model.Commands(modelOpener(gf)), "models")
}

func newAgentsCmd(gf *GlobalFlags) *cobra.Command {
	return findSubcommand(auraagent.Commands(agentOpener(gf)), "agents")
}

func newPersonaCmd(gf *GlobalFlags) *cobra.Command {
	return findSubcommand(auraagent.Commands(agentOpener(gf), personaeval.Commands(personaEvalOpener(gf))...), "persona")
}

func newBindingsCmd(gf *GlobalFlags) *cobra.Command {
	return findSubcommand(auraagent.Commands(agentOpener(gf)), "bindings")
}

func storagePaths(cfg *appconfig.Config) (dbPath, artifactRoot, backupDir string, err error) {
	return storage.ResolvePaths(&cfg.Storage)
}

func resolveDataRoot(cfg *appconfig.Config) (string, error) {
	return storage.ResolveDataRoot(&cfg.Storage)
}

func buildSecretVault(cfg *appconfig.Config) *secret.Vault {
	return appconfig.BuildSecretVault(cfg)
}

func buildStrictSecretVault(cfg *appconfig.Config) (*secret.Vault, error) {
	return appconfig.BuildStrictSecretVault(cfg)
}

func additionalConfiguredSecrets(cfg *appconfig.Config) []appconfig.ConfiguredSecret {
	return appconfig.AdditionalConfiguredSecrets(cfg)
}

func newAccessPolicy(registry *auraagent.Registry, db *sql.DB, onStandingGrantUsed func(context.Context)) *auraagent.AccessPolicy {
	return auraagent.NewAccessPolicy(registry, principal.NewStandingGrantStore(db), onStandingGrantUsed)
}

func newStandingGrantRecorder(db *sql.DB) *principal.StandingGrantRecorder {
	return principal.NewStandingGrantRecorder(principal.NewStandingGrantStore(db), turnstore.NewOwnerActionRecorder(db))
}

func newPairingService(db *sql.DB, observer principal.PairingObserver) *principal.PairingService {
	pairings, err := principal.NewPairingStore(db, turnstore.NewTurnStore(db))
	if err != nil {
		panic(err)
	}
	return principal.NewPairingService(pairings, principal.NewPrincipalGrantStore(db), observer)
}

func recordAccessAction(ctx context.Context, db *sql.DB, action *turnstore.AccessAction, subject string, at time.Time) error {
	return turnstore.NewOwnerActionRecorder(db).RecordAccessAction(ctx, action, subject, at)
}

func newPairingCode() (string, error) {
	return principal.NewPairingCode()
}

func pairingCodeDigest(code string) string {
	return principal.PairingCodeDigest(code)
}

func pairingLockedUntil(failures []time.Time, now time.Time) (time.Time, bool) {
	return principal.PairingLockedUntil(failures, now)
}

func pairingLock(ctx context.Context, pairings principal.PairingStore, channel string, now time.Time) (time.Time, bool, error) {
	return principal.PairingLock(ctx, pairings, channel, now)
}

func resolveChatPersona(ctx context.Context, registry *auraagent.Registry, sessions turnstore.SessionService, sessionID, requested string) (string, error) {
	return auraagent.ResolveChatPersona(ctx, registry, sessionAgentGetter{sessions: sessions}, sessionID, requested)
}

func forwardInterrupts(ctx context.Context, signals <-chan os.Signal) (forwarded <-chan struct{}, stop func()) {
	return terminal.ForwardInterrupts(ctx, signals)
}

func filterFindingsByCheck(findings []health.Finding, check string) []health.Finding {
	return health.FilterFindingsByCheck(findings, check)
}

func formatDoctorFinding(f *health.Finding) string {
	return health.FormatDoctorFinding(f)
}

func runProcessProbe(cmd *cobra.Command, listen, probe string) error {
	return health.RunProcessProbe(cmd.Context(), cmd.OutOrStdout(), listen, probe)
}
