package plugin

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"github.com/anggasct/aura/internal/modules/plugin/auth"
	"github.com/anggasct/aura/internal/platform/command"
	"github.com/anggasct/aura/internal/platform/fault"
)

const (
	AuditActionApprove = "plugin_approve"
	AuditActionDisable = "plugin_disable"
	AuditActionRemove  = "plugin_remove"
	AuditActionLogin   = "plugin_login"
	AuditActionLogout  = "plugin_logout"

	restartNotice = "takes effect at the next server start"
)

type AuditRecorder interface {
	Record(ctx context.Context, action, pluginID, version string) error
}

type Opener func(cmd *cobra.Command, fn func(ctx context.Context, service *Service, audit AuditRecorder) error) error

type ListRow struct {
	ID              string `json:"id"`
	Name            string `json:"name"`
	State           string `json:"state"`
	Version         string `json:"version"`
	ApprovedVersion string `json:"approved_version"`
	DigestPrefix    string `json:"digest_prefix"`
}

type ListOutput struct {
	Plugins []ListRow `json:"plugins"`
}

func Commands(open Opener) []*cobra.Command {
	cmd := &cobra.Command{
		Use:   "plugin",
		Short: "Install, review and approve plugins",
	}
	cmd.AddCommand(
		newListCmd(open),
		newInstallCmd(open),
		newReviewCmd(open),
		newApproveCmd(open),
		newDisableCmd(open),
		newRemoveCmd(open),
		newLoginCmd(open),
		newLogoutCmd(open),
	)
	return []*cobra.Command{cmd}
}

func runWithOpener(cmd *cobra.Command, open Opener, fn func(ctx context.Context, service *Service, audit AuditRecorder) error) error {
	if open == nil {
		return fault.New(ErrInvalidArgument, "plugin command opener must not be nil")
	}
	return open(cmd, fn)
}

func noArgs(cmd *cobra.Command, args []string) error {
	if err := cobra.NoArgs(cmd, args); err != nil {
		return command.NewUsageError(err)
	}
	return nil
}

func exactOneArg(cmd *cobra.Command, args []string) error {
	if err := cobra.ExactArgs(1)(cmd, args); err != nil {
		return command.NewUsageError(err)
	}
	return nil
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

func recordAudit(ctx context.Context, audit AuditRecorder, action, pluginID, version string) error {
	if audit == nil {
		return nil
	}
	return audit.Record(ctx, action, pluginID, version)
}

func newListCmd(open Opener) *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List installed plugins with their state",
		Args:  noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runWithOpener(cmd, open, func(ctx context.Context, service *Service, _ AuditRecorder) error {
				summaries, err := service.List(ctx)
				if err != nil {
					return err
				}
				rows := make([]ListRow, 0, len(summaries))
				for i := range summaries {
					rows = append(rows, ListRow{
						ID:              summaries[i].ID,
						Name:            summaries[i].Name,
						State:           summaries[i].State,
						Version:         summaries[i].Version,
						ApprovedVersion: summaries[i].ApprovedVersion,
						DigestPrefix:    summaries[i].DigestPrefix,
					})
				}
				if asJSON {
					encoder := json.NewEncoder(cmd.OutOrStdout())
					encoder.SetIndent("", "  ")
					return encoder.Encode(ListOutput{Plugins: rows})
				}
				if len(rows) == 0 {
					return writeLines(cmd, []string{"no plugins installed"})
				}
				lines := []string{"PLUGIN\tSTATE\tVERSION\tAPPROVED\tDIGEST"}
				for i := range rows {
					lines = append(lines, fmt.Sprintf("%s\t%s\t%s\t%s\t%s", rows[i].ID, rows[i].State, rows[i].Version, dashIfEmpty(rows[i].ApprovedVersion), rows[i].DigestPrefix))
				}
				return writeLines(cmd, lines)
			})
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit machine-readable output")
	return cmd
}

func dashIfEmpty(value string) string {
	if value == "" {
		return "-"
	}
	return value
}

func diffLines(diff *Diff) []string {
	var lines []string
	add := func(label string, changes []Change) {
		for i := range changes {
			line := fmt.Sprintf("  %s %s %s: %s", label, changes[i].Surface, changes[i].Value, changes[i].Kind)
			if changes[i].Detail != "" {
				line += " (" + changes[i].Detail + ")"
			}
			lines = append(lines, line)
		}
	}
	add("restart", diff.Restart)
	add("widened", diff.Widened)
	add("removed", diff.Removed)
	return lines
}

func newInstallCmd(open Opener) *cobra.Command {
	return &cobra.Command{
		Use:   "install <directory>",
		Short: "Copy a plugin bundle into quarantine without running it",
		Args:  exactOneArg,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runWithOpener(cmd, open, func(ctx context.Context, service *Service, _ AuditRecorder) error {
				result, err := service.Install(ctx, args[0])
				if err != nil {
					return err
				}
				if result.Unchanged {
					return writeLines(cmd, []string{fmt.Sprintf("%s %s is already installed (digest %s); no change", result.ID, result.Version, result.Digest)})
				}
				lines := []string{fmt.Sprintf("installed %s %s as %s (digest %s)", result.ID, result.Version, result.State, result.Digest)}
				lines = append(lines, diffLines(&result.Diff)...)
				if result.Accepted {
					lines = append(lines, "the update only removed surface and was accepted; "+restartNotice)
				} else {
					lines = append(lines, "review it with: aura plugin review "+result.ID)
				}
				return writeLines(cmd, lines)
			})
		},
	}
}

func FormatReview(review *Review) []string {
	return reviewLines(review)
}

func reviewLines(review *Review) []string {
	lines := []string{
		fmt.Sprintf("Plugin: %s (%s)", review.ID, review.Name),
		"State: " + review.State,
		fmt.Sprintf("Version: %s (approved: %s)", review.Version, dashIfEmpty(review.ApprovedVersion)),
		"Digest: " + review.Digest,
		"Description: " + review.Description,
		fmt.Sprintf("Backend: %s %s", review.Backend.Kind, backendTarget(&review.Backend)),
		"Auth: " + authSummary(&review.Auth),
		"Capabilities: " + dashIfEmpty(strings.Join(review.Capabilities, ", ")),
		"Egress: " + dashIfEmpty(strings.Join(review.Egress, ", ")),
	}
	if len(review.Secrets) == 0 {
		lines = append(lines, "Secrets: -")
	} else {
		lines = append(lines, "Secrets:")
		for i := range review.Secrets {
			lines = append(lines, fmt.Sprintf("  %s -> %s", review.Secrets[i].Name, review.Secrets[i].Ref))
		}
	}
	if len(review.Tools) == 0 {
		lines = append(lines, "Tools: -")
	} else {
		lines = append(lines, "Tools:")
		for i := range review.Tools {
			lines = append(lines, fmt.Sprintf("  %s [%s]: %s", review.Tools[i].ID, review.Tools[i].Tier, review.Tools[i].Description))
			if len(review.Tools[i].Display) > 0 {
				lines = append(lines, "    approval shows, in order: "+strings.Join(review.Tools[i].Display, ", "))
			}
		}
	}
	if review.Diff != nil {
		if review.Diff.Empty() {
			lines = append(lines, "Changes since approval: none")
		} else {
			lines = append(lines, "Changes since approval:")
			lines = append(lines, diffLines(review.Diff)...)
		}
	}
	return lines
}

func backendTarget(backend *BackendSummary) string {
	if backend.URL != "" {
		return backend.URL
	}
	return strings.TrimSpace(backend.Command + " " + strings.Join(backend.Args, " "))
}

func authSummary(spec *Auth) string {
	switch spec.Kind {
	case "", AuthNone:
		return "none"
	case AuthOAuth2:
		return fmt.Sprintf("oauth2 (authorize %s, token %s, scopes %s, redirect %s, client id secret %s, client secret secret %s%s%s)",
			spec.AuthorizeURL, spec.TokenURL, strings.Join(sortedStrings(spec.Scopes), " "), spec.RedirectURI,
			spec.ClientIDSecret, dashIfEmpty(spec.ClientSecretSecret), oauthExtrasSummary(spec), authHostsSummary(spec.Hosts))
	default:
		return fmt.Sprintf("%s (secret %s%s)", spec.Kind, spec.Secret, authHostsSummary(spec.Hosts))
	}
}

func oauthExtrasSummary(spec *Auth) string {
	var extras string
	if len(spec.AuthorizeParams) > 0 {
		keys := slices.Sorted(maps.Keys(spec.AuthorizeParams))
		pairs := make([]string, 0, len(keys))
		for _, key := range keys {
			pairs = append(pairs, key+"="+spec.AuthorizeParams[key])
		}
		extras += ", authorize params " + strings.Join(pairs, " ")
	}
	if spec.Issuer != "" {
		extras += ", issuer " + spec.Issuer
	}
	if spec.AccessOnly {
		extras += ", access only (no refresh token required)"
	}
	return extras
}

func authHostsSummary(hosts []string) string {
	if len(hosts) == 0 {
		return ""
	}
	return ", credential sent to " + strings.Join(sortedStrings(hosts), ", ")
}

func sortedStrings(values []string) []string {
	out := slices.Clone(values)
	slices.Sort(out)
	return out
}

func newReviewCmd(open Opener) *cobra.Command {
	return &cobra.Command{
		Use:   "review <plugin>",
		Short: "Show what a plugin declares and what changed since approval",
		Args:  exactOneArg,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runWithOpener(cmd, open, func(ctx context.Context, service *Service, _ AuditRecorder) error {
				review, err := service.Review(ctx, args[0])
				if err != nil {
					return err
				}
				return writeLines(cmd, reviewLines(review))
			})
		},
	}
}

func newApproveCmd(open Opener) *cobra.Command {
	var digest string
	cmd := &cobra.Command{
		Use:   "approve <plugin> --digest <prefix>",
		Short: "Approve the reviewed version of a plugin",
		Args:  exactOneArg,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runWithOpener(cmd, open, func(ctx context.Context, service *Service, audit AuditRecorder) error {
				approved, err := service.Approve(ctx, args[0], digest)
				if err != nil {
					return err
				}
				if err := recordAudit(ctx, audit, AuditActionApprove, approved.ID, approved.Version); err != nil {
					return err
				}
				return writeLines(cmd, []string{fmt.Sprintf("approved %s %s (digest %s, %d tools: %s); %s", approved.ID, approved.Version, approved.Digest, len(approved.Tools), strings.Join(approved.Tools, ", "), restartNotice)})
			})
		},
	}
	cmd.Flags().StringVar(&digest, "digest", "", "digest prefix shown by review (at least 12 characters)")
	return cmd
}

func newDisableCmd(open Opener) *cobra.Command {
	return &cobra.Command{
		Use:   "disable <plugin>",
		Short: "Switch a plugin off",
		Args:  exactOneArg,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runWithOpener(cmd, open, func(ctx context.Context, service *Service, audit AuditRecorder) error {
				if err := service.Disable(ctx, args[0]); err != nil {
					return err
				}
				if err := recordAudit(ctx, audit, AuditActionDisable, args[0], ""); err != nil {
					return err
				}
				return writeLines(cmd, []string{fmt.Sprintf("disabled %s; %s", args[0], restartNotice)})
			})
		},
	}
}

func newRemoveCmd(open Opener) *cobra.Command {
	var apply bool
	cmd := &cobra.Command{
		Use:   "remove <plugin>",
		Short: "Show, or with --apply perform, the removal of a plugin and its bundle",
		Args:  exactOneArg,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runWithOpener(cmd, open, func(ctx context.Context, service *Service, audit AuditRecorder) error {
				plan, err := service.Remove(ctx, args[0], apply)
				if err != nil {
					return err
				}
				if !plan.Applied {
					return writeLines(cmd, []string{fmt.Sprintf("would remove %s (state %s, version %s); rerun with --apply to delete the registry rows and the bundle", plan.ID, plan.State, plan.Version)})
				}
				if err := recordAudit(ctx, audit, AuditActionRemove, plan.ID, plan.Version); err != nil {
					return err
				}
				return writeLines(cmd, []string{fmt.Sprintf("removed %s; %s", plan.ID, restartNotice)})
			})
		},
	}
	cmd.Flags().BoolVar(&apply, "apply", false, "perform the removal")
	return cmd
}

func newLoginCmd(open Opener) *cobra.Command {
	var paste bool
	cmd := &cobra.Command{
		Use:   "login <plugin>",
		Short: "Authorize an active oauth2 plugin and store its token",
		Args:  exactOneArg,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runWithOpener(cmd, open, func(ctx context.Context, service *Service, audit AuditRecorder) error {
				out := cmd.OutOrStdout()
				opts := &auth.LoginOptions{
					Paste: paste,
					Input: cmd.InOrStdin(),
					Prompt: func(authorizeURL string) {
						if paste {
							_, _ = fmt.Fprintf(out, "Open this URL, approve access, then paste the URL your browser is redirected to:\n%s\n", authorizeURL)
							return
						}
						_, _ = fmt.Fprintf(out, "Open this URL to approve access; this command waits up to %s for the callback:\n%s\n", auth.LoginTimeout, authorizeURL)
					},
				}
				if err := service.Login(ctx, args[0], opts); err != nil {
					return err
				}
				if err := recordAudit(ctx, audit, AuditActionLogin, args[0], ""); err != nil {
					return err
				}
				return writeLines(cmd, []string{fmt.Sprintf("logged in %s; %s", args[0], restartNotice)})
			})
		},
	}
	cmd.Flags().BoolVar(&paste, "paste", false, "read the redirected url from standard input instead of listening on the redirect address")
	return cmd
}

func newLogoutCmd(open Opener) *cobra.Command {
	return &cobra.Command{
		Use:   "logout <plugin>",
		Short: "Delete the stored token of a plugin",
		Args:  exactOneArg,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runWithOpener(cmd, open, func(ctx context.Context, service *Service, audit AuditRecorder) error {
				if err := service.Logout(ctx, args[0]); err != nil {
					return err
				}
				if err := recordAudit(ctx, audit, AuditActionLogout, args[0], ""); err != nil {
					return err
				}
				return writeLines(cmd, []string{fmt.Sprintf("logged out %s; %s", args[0], restartNotice)})
			})
		},
	}
}
