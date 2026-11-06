package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/spf13/cobra"

	"github.com/anggasct/aura/internal/capability"
	"github.com/anggasct/aura/internal/config"
	"github.com/anggasct/aura/internal/logging"
	"github.com/anggasct/aura/internal/tools/interpreter"
)

type configLoader func(path string) (config.LoadResult, error)

type interpreterDeps struct {
	load              configLoader
	manifest          func() (*interpreter.Manifest, error)
	platformSupported func() bool
}

func newInterpreterCmd(gf *globalFlags, deps interpreterDeps) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "interpreter",
		Short: "Manage the pinned code interpreter runtimes",
	}
	cmd.AddCommand(
		newInterpreterInstallCmd(gf, deps),
		newInterpreterListCmd(gf, deps),
		newInterpreterRemoveCmd(gf, deps),
	)
	return cmd
}

func withInterpreter(cmd *cobra.Command, gf *globalFlags, deps interpreterDeps, fn func(context.Context, *slog.Logger, *interpreter.Store) error) error {
	result, err := deps.load(gf.configPath)
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
	store, err := newInterpreterStore(result.Config, manifest)
	if err != nil {
		return err
	}
	return fn(cmd.Context(), logger, store)
}

func requireInterpreterCapability(report capability.Report, platformSupported bool) error {
	status, ok := report.Status(codeInterpreterCapability)
	if !ok || !status.Compiled {
		return &capability.Error{Code: capability.ErrorCodeCapabilityUnavailable, Capability: codeInterpreterCapability, Detail: "capability is absent from this artifact"}
	}
	if !platformSupported {
		return &capability.Error{Code: capability.ErrorCodeCapabilityUnavailable, Capability: codeInterpreterCapability, Detail: "runtimes need linux/amd64"}
	}
	return nil
}

func newInterpreterInstallCmd(gf *globalFlags, deps interpreterDeps) *cobra.Command {
	var archive string
	cmd := &cobra.Command{
		Use:   "install <python|javascript|go> --archive <path>",
		Short: "Install a runtime from a local .tar.gz whose SHA-256 matches the pinned value",
		Long: "Copies the archive while hashing it, refuses a digest that differs from the value pinned in this binary,\n" +
			"extracts it with strict path and type checks, records a tree digest and makes the files read-only.\n" +
			"Nothing is downloaded. A running server verifies runtimes at startup, so restart it to pick up a new install.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return withInterpreter(cmd, gf, deps, func(ctx context.Context, logger *slog.Logger, store *interpreter.Store) error {
				result, err := store.Install(ctx, args[0], archive)
				if err != nil {
					return err
				}
				logger.InfoContext(ctx, "interpreter runtime installed",
					"component", "interpreter", "runtime", result.Runtime, "version", result.Version)
				return writeProfileLines(cmd, []string{
					fmt.Sprintf("installed: %s %s", result.Runtime, result.Version),
					"restart a running server so it verifies the new runtime",
				})
			})
		},
	}
	cmd.Flags().StringVar(&archive, "archive", "", "path to the local .tar.gz archive")
	_ = cmd.MarkFlagRequired("archive")
	return cmd
}

type interpreterListRow struct {
	Runtime string `json:"runtime"`
	Version string `json:"version"`
	State   string `json:"state"`
	Reason  string `json:"reason,omitempty"`
}

type interpreterListOutput struct {
	Runtimes []interpreterListRow `json:"runtimes"`
}

func newInterpreterListCmd(gf *globalFlags, deps interpreterDeps) *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "list",
		Short: "Show each runtime, its pinned version and its state",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return withInterpreter(cmd, gf, deps, func(ctx context.Context, _ *slog.Logger, store *interpreter.Store) error {
				verifications, err := store.List(ctx)
				if err != nil {
					return err
				}
				rows := make([]interpreterListRow, 0, len(verifications))
				for _, verification := range verifications {
					rows = append(rows, interpreterListRow{
						Runtime: verification.Runtime, Version: verification.Version,
						State: string(verification.State), Reason: string(verification.Reason),
					})
				}
				if asJSON {
					encoder := json.NewEncoder(cmd.OutOrStdout())
					encoder.SetIndent("", "  ")
					return encoder.Encode(interpreterListOutput{Runtimes: rows})
				}
				lines := []string{"RUNTIME\tVERSION\tSTATE"}
				for i := range rows {
					line := fmt.Sprintf("%s\t%s\t%s", rows[i].Runtime, rows[i].Version, rows[i].State)
					if rows[i].Reason != "" {
						line += " (" + rows[i].Reason + ")"
					}
					lines = append(lines, line)
				}
				return writeProfileLines(cmd, lines)
			})
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit machine-readable output")
	return cmd
}

func newInterpreterRemoveCmd(gf *globalFlags, deps interpreterDeps) *cobra.Command {
	var apply bool
	cmd := &cobra.Command{
		Use:   "remove <python|javascript|go>",
		Short: "Preview or apply removal of an installed runtime",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return withInterpreter(cmd, gf, deps, func(ctx context.Context, logger *slog.Logger, store *interpreter.Store) error {
				removal, err := store.Remove(ctx, args[0], apply)
				if err != nil {
					return err
				}
				if len(removal.Versions) == 0 && removal.Temporary == 0 {
					return writeProfileLines(cmd, []string{fmt.Sprintf("nothing to remove: %s is not installed", removal.Runtime)})
				}
				description := fmt.Sprintf("%s versions=%v interrupted_installs=%d", removal.Runtime, removal.Versions, removal.Temporary)
				if !apply {
					return writeProfileLines(cmd, []string{"dry-run: would remove " + description + "; rerun with --apply"})
				}
				logger.InfoContext(ctx, "interpreter runtime removed",
					"component", "interpreter", "runtime", removal.Runtime, "versions", len(removal.Versions))
				return writeProfileLines(cmd, []string{"removed: " + description})
			})
		},
	}
	cmd.Flags().BoolVar(&apply, "apply", false, "apply removal (default dry-run)")
	return cmd
}
