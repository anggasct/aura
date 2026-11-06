package cli

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/anggasct/aura/internal/config"
	"github.com/anggasct/aura/internal/plugin"
	"github.com/anggasct/aura/internal/plugin/auth"
)

const pluginRestartNotice = "takes effect at the next server start"

func newPluginCmd(gf *globalFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "plugin",
		Short: "Install, review and approve plugins",
	}
	cmd.AddCommand(
		newPluginListCmd(gf),
		newPluginInstallCmd(gf),
		newPluginReviewCmd(gf),
		newPluginApproveCmd(gf),
		newPluginDisableCmd(gf),
		newPluginRemoveCmd(gf),
		newPluginLoginCmd(gf),
		newPluginLogoutCmd(gf),
	)
	return cmd
}

func openPluginService(cmd *cobra.Command, gf *globalFlags) (*plugin.Service, *sql.DB, error) {
	result, err := config.Load(gf.configPath)
	if err != nil {
		return nil, nil, err
	}
	db, err := openStorage(cmd.Context(), result.Config)
	if err != nil {
		return nil, nil, err
	}
	service, err := newPluginService(result.Config, db, discardRedactor{})
	if err != nil {
		_ = db.Close()
		return nil, nil, err
	}
	return service, db, nil
}

type pluginListRow struct {
	ID              string `json:"id"`
	Name            string `json:"name"`
	State           string `json:"state"`
	Version         string `json:"version"`
	ApprovedVersion string `json:"approved_version"`
	DigestPrefix    string `json:"digest_prefix"`
}

type pluginListOutput struct {
	Plugins []pluginListRow `json:"plugins"`
}

func newPluginListCmd(gf *globalFlags) *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List installed plugins with their state",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			service, db, err := openPluginService(cmd, gf)
			if err != nil {
				return err
			}
			defer func() { _ = db.Close() }()
			summaries, err := service.List(cmd.Context())
			if err != nil {
				return err
			}
			rows := make([]pluginListRow, 0, len(summaries))
			for i := range summaries {
				rows = append(rows, pluginListRow{
					ID: summaries[i].ID, Name: summaries[i].Name, State: summaries[i].State, Version: summaries[i].Version,
					ApprovedVersion: summaries[i].ApprovedVersion, DigestPrefix: summaries[i].DigestPrefix,
				})
			}
			out := cmd.OutOrStdout()
			if asJSON {
				encoder := json.NewEncoder(out)
				encoder.SetIndent("", "  ")
				return encoder.Encode(pluginListOutput{Plugins: rows})
			}
			if len(rows) == 0 {
				return writeChildLines(cmd, []string{"no plugins installed"})
			}
			lines := []string{"PLUGIN\tSTATE\tVERSION\tAPPROVED\tDIGEST"}
			for i := range rows {
				lines = append(lines, fmt.Sprintf("%s\t%s\t%s\t%s\t%s", rows[i].ID, rows[i].State, rows[i].Version, dashIfEmpty(rows[i].ApprovedVersion), rows[i].DigestPrefix))
			}
			return writeChildLines(cmd, lines)
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

func diffLines(diff *plugin.Diff) []string {
	var lines []string
	add := func(label string, changes []plugin.Change) {
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

func newPluginInstallCmd(gf *globalFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "install <directory>",
		Short: "Copy a plugin bundle into quarantine without running it",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			service, db, err := openPluginService(cmd, gf)
			if err != nil {
				return err
			}
			defer func() { _ = db.Close() }()
			result, err := service.Install(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			if result.Unchanged {
				return writeChildLines(cmd, []string{fmt.Sprintf("%s %s is already installed (digest %s); no change", result.ID, result.Version, result.Digest)})
			}
			lines := []string{fmt.Sprintf("installed %s %s as %s (digest %s)", result.ID, result.Version, result.State, result.Digest)}
			lines = append(lines, diffLines(&result.Diff)...)
			if result.Accepted {
				lines = append(lines, "the update only removed surface and was accepted; "+pluginRestartNotice)
			} else {
				lines = append(lines, "review it with: aura plugin review "+result.ID)
			}
			return writeChildLines(cmd, lines)
		},
	}
}

func reviewLines(review *plugin.Review) []string {
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

func backendTarget(backend *plugin.BackendSummary) string {
	if backend.URL != "" {
		return backend.URL
	}
	return strings.TrimSpace(backend.Command + " " + strings.Join(backend.Args, " "))
}

func authSummary(spec *plugin.Auth) string {
	switch spec.Kind {
	case "", plugin.AuthNone:
		return "none"
	case plugin.AuthOAuth2:
		return fmt.Sprintf("oauth2 (authorize %s, token %s, scopes %s, redirect %s, client id secret %s, client secret secret %s)",
			spec.AuthorizeURL, spec.TokenURL, strings.Join(sortedStrings(spec.Scopes), " "), spec.RedirectURI,
			spec.ClientIDSecret, dashIfEmpty(spec.ClientSecretSecret))
	default:
		return fmt.Sprintf("%s (secret %s)", spec.Kind, spec.Secret)
	}
}

func sortedStrings(values []string) []string {
	out := slices.Clone(values)
	slices.Sort(out)
	return out
}

func newPluginReviewCmd(gf *globalFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "review <plugin>",
		Short: "Show what a plugin declares and what changed since approval",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			service, db, err := openPluginService(cmd, gf)
			if err != nil {
				return err
			}
			defer func() { _ = db.Close() }()
			review, err := service.Review(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			return writeChildLines(cmd, reviewLines(review))
		},
	}
}

func newPluginApproveCmd(gf *globalFlags) *cobra.Command {
	var digest string
	cmd := &cobra.Command{
		Use:   "approve <plugin> --digest <prefix>",
		Short: "Approve the reviewed version of a plugin",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			service, db, err := openPluginService(cmd, gf)
			if err != nil {
				return err
			}
			defer func() { _ = db.Close() }()
			approved, err := service.Approve(cmd.Context(), args[0], digest)
			if err != nil {
				return err
			}
			if err := recordAccessAction(cmd.Context(), db, &accessAction{Action: accessActionPluginApprove, Plugin: approved.ID, Version: approved.Version}, approved.ID+"-"+approved.Version, time.Now()); err != nil {
				return err
			}
			return writeChildLines(cmd, []string{fmt.Sprintf("approved %s %s (digest %s, %d tools: %s); %s", approved.ID, approved.Version, approved.Digest, len(approved.Tools), strings.Join(approved.Tools, ", "), pluginRestartNotice)})
		},
	}
	cmd.Flags().StringVar(&digest, "digest", "", "digest prefix shown by review (at least 12 characters)")
	return cmd
}

func newPluginDisableCmd(gf *globalFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "disable <plugin>",
		Short: "Switch a plugin off",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			service, db, err := openPluginService(cmd, gf)
			if err != nil {
				return err
			}
			defer func() { _ = db.Close() }()
			if err := service.Disable(cmd.Context(), args[0]); err != nil {
				return err
			}
			if err := recordAccessAction(cmd.Context(), db, &accessAction{Action: accessActionPluginDisable, Plugin: args[0]}, args[0], time.Now()); err != nil {
				return err
			}
			return writeChildLines(cmd, []string{fmt.Sprintf("disabled %s; %s", args[0], pluginRestartNotice)})
		},
	}
}

func newPluginRemoveCmd(gf *globalFlags) *cobra.Command {
	var apply bool
	cmd := &cobra.Command{
		Use:   "remove <plugin>",
		Short: "Show, or with --apply perform, the removal of a plugin and its bundle",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			service, db, err := openPluginService(cmd, gf)
			if err != nil {
				return err
			}
			defer func() { _ = db.Close() }()
			plan, err := service.Remove(cmd.Context(), args[0], apply)
			if err != nil {
				return err
			}
			if !plan.Applied {
				return writeChildLines(cmd, []string{fmt.Sprintf("would remove %s (state %s, version %s); rerun with --apply to delete the registry rows and the bundle", plan.ID, plan.State, plan.Version)})
			}
			if err := recordAccessAction(cmd.Context(), db, &accessAction{Action: accessActionPluginRemove, Plugin: plan.ID, Version: plan.Version}, plan.ID, time.Now()); err != nil {
				return err
			}
			return writeChildLines(cmd, []string{fmt.Sprintf("removed %s; %s", plan.ID, pluginRestartNotice)})
		},
	}
	cmd.Flags().BoolVar(&apply, "apply", false, "perform the removal")
	return cmd
}

func newPluginLoginCmd(gf *globalFlags) *cobra.Command {
	var paste bool
	cmd := &cobra.Command{
		Use:   "login <plugin>",
		Short: "Authorize an active oauth2 plugin and store its token",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			service, db, err := openPluginService(cmd, gf)
			if err != nil {
				return err
			}
			defer func() { _ = db.Close() }()
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
			if err := service.Login(cmd.Context(), args[0], opts); err != nil {
				return err
			}
			return writeChildLines(cmd, []string{fmt.Sprintf("logged in %s; %s", args[0], pluginRestartNotice)})
		},
	}
	cmd.Flags().BoolVar(&paste, "paste", false, "read the redirected url from standard input instead of listening on the redirect address")
	return cmd
}

func newPluginLogoutCmd(gf *globalFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "logout <plugin>",
		Short: "Delete the stored token of a plugin",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			service, db, err := openPluginService(cmd, gf)
			if err != nil {
				return err
			}
			defer func() { _ = db.Close() }()
			if err := service.Logout(cmd.Context(), args[0]); err != nil {
				return err
			}
			return writeChildLines(cmd, []string{fmt.Sprintf("logged out %s; %s", args[0], pluginRestartNotice)})
		},
	}
}
