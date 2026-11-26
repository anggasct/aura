package codeexec

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/spf13/cobra"

	"github.com/anggasct/aura/internal/modules/codeexec/interpreter"
	"github.com/anggasct/aura/internal/platform/fault"
)

type Opener func(cmd *cobra.Command, fn func(ctx context.Context, logger *slog.Logger, store *interpreter.Store) error) error

type ListRow struct {
	Runtime string `json:"runtime"`
	Version string `json:"version"`
	State   string `json:"state"`
	Reason  string `json:"reason,omitempty"`
	Archive string `json:"archive"`
	SHA256  string `json:"sha256"`
}

type ListOutput struct {
	Runtimes []ListRow `json:"runtimes"`
}

func Commands(open Opener) []*cobra.Command {
	cmd := &cobra.Command{
		Use:   "interpreter",
		Short: "Manage the pinned code interpreter runtimes",
	}
	cmd.AddCommand(
		newInstallCmd(open),
		newListCmd(open),
		newRemoveCmd(open),
	)
	return []*cobra.Command{cmd}
}

func runWithOpener(cmd *cobra.Command, open Opener, fn func(ctx context.Context, logger *slog.Logger, store *interpreter.Store) error) error {
	if open == nil {
		return fault.New(ErrorCodeInvalidArgument, "interpreter command opener must not be nil")
	}
	return open(cmd, func(ctx context.Context, logger *slog.Logger, store *interpreter.Store) error {
		if logger == nil {
			logger = slog.Default()
		}
		if store == nil {
			return fault.New(ErrorCodeInvalidArgument, "interpreter store must not be nil")
		}
		return fn(ctx, logger, store)
	})
}

func writeLines(cmd *cobra.Command, lines []string) error {
	out := cmd.OutOrStdout()
	for _, line := range lines {
		if _, err := fmt.Fprintln(out, line); err != nil {
			return err
		}
	}
	return nil
}

func newInstallCmd(open Opener) *cobra.Command {
	var archive string
	cmd := &cobra.Command{
		Use:   "install <python|javascript|go> --archive <path>",
		Short: "Install a runtime from a local .tar.gz whose SHA-256 matches the pinned value",
		Long: "Copies the archive while hashing it, refuses a digest that differs from the value pinned in this binary,\n" +
			"extracts it with strict path and type checks, records a tree digest and makes the files read-only.\n" +
			"Nothing is downloaded. A running server verifies runtimes at startup, so restart it to pick up a new install.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runWithOpener(cmd, open, func(ctx context.Context, logger *slog.Logger, store *interpreter.Store) error {
				result, err := store.Install(ctx, args[0], archive)
				if err != nil {
					return err
				}
				logger.InfoContext(ctx, "interpreter runtime installed",
					"component", ComponentInterpreter, "runtime", result.Runtime, "version", result.Version)
				return writeLines(cmd, []string{
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

func newListCmd(open Opener) *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "list",
		Short: "Show each runtime, its pinned version and its state",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runWithOpener(cmd, open, func(ctx context.Context, _ *slog.Logger, store *interpreter.Store) error {
				verifications, err := store.List(ctx)
				if err != nil {
					return err
				}
				rows := make([]ListRow, 0, len(verifications))
				for _, verification := range verifications {
					pinned, _ := store.Manifest().Lookup(verification.Runtime)
					rows = append(rows, ListRow{
						Runtime: verification.Runtime, Version: verification.Version,
						State: string(verification.State), Reason: string(verification.Reason),
						Archive: pinned.Archive, SHA256: pinned.SHA256,
					})
				}
				if asJSON {
					encoder := json.NewEncoder(cmd.OutOrStdout())
					encoder.SetIndent("", "  ")
					return encoder.Encode(ListOutput{Runtimes: rows})
				}
				lines := []string{"RUNTIME\tVERSION\tSTATE\tINSTALL FROM"}
				for i := range rows {
					line := fmt.Sprintf("%s\t%s\t%s", rows[i].Runtime, rows[i].Version, rows[i].State)
					if rows[i].Reason != "" {
						line += " (" + rows[i].Reason + ")"
					}
					if rows[i].State != string(interpreter.StateInstalled) {
						line += fmt.Sprintf("\t%s sha256:%s", rows[i].Archive, rows[i].SHA256)
					}
					lines = append(lines, line)
				}
				return writeLines(cmd, lines)
			})
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit machine-readable output")
	return cmd
}

func newRemoveCmd(open Opener) *cobra.Command {
	var apply bool
	cmd := &cobra.Command{
		Use:   "remove <python|javascript|go>",
		Short: "Preview or apply removal of an installed runtime",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runWithOpener(cmd, open, func(ctx context.Context, logger *slog.Logger, store *interpreter.Store) error {
				removal, err := store.Remove(ctx, args[0], apply)
				if err != nil {
					return err
				}
				if len(removal.Versions) == 0 && removal.Temporary == 0 {
					return writeLines(cmd, []string{fmt.Sprintf("nothing to remove: %s is not installed", removal.Runtime)})
				}
				description := fmt.Sprintf("%s versions=%v interrupted_installs=%d", removal.Runtime, removal.Versions, removal.Temporary)
				if !apply {
					return writeLines(cmd, []string{"dry-run: would remove " + description + "; rerun with --apply"})
				}
				logger.InfoContext(ctx, "interpreter runtime removed",
					"component", ComponentInterpreter, "runtime", removal.Runtime, "versions", len(removal.Versions))
				return writeLines(cmd, []string{"removed: " + description})
			})
		},
	}
	cmd.Flags().BoolVar(&apply, "apply", false, "apply removal (default dry-run)")
	return cmd
}
