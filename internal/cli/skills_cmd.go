package cli

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/anggasct/aura/internal/config"
	"github.com/anggasct/aura/internal/skills"
)

func newSkillsCmd(gf *globalFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "skills",
		Short: "Inspect and review agent skills",
	}
	cmd.AddCommand(
		newSkillsListCmd(gf),
		newSkillsShowCmd(gf),
		newSkillsValidateCmd(gf),
		newSkillsCreateCmd(gf),
		newSkillsReviewCmd(gf),
		newSkillsAcceptCmd(gf),
		newSkillsRejectCmd(gf),
		newSkillsDisableCmd(gf),
		newSkillsDoctorCmd(gf),
	)
	return cmd
}

func openSkillsEngine(cmd *cobra.Command, gf *globalFlags) (*skills.Engine, *sql.DB, error) {
	result, err := config.Load(gf.configPath)
	if err != nil {
		return nil, nil, err
	}
	if result.Config.Skills == nil || !result.Config.Skills.Enabled {
		return nil, nil, errors.New("skills are not enabled")
	}
	db, err := openStorage(cmd.Context(), result.Config)
	if err != nil {
		return nil, nil, err
	}
	engine, err := buildSkillsEngine(cmd.Context(), result.Config.Skills, db, nil)
	if err != nil {
		_ = db.Close()
		return nil, nil, err
	}
	return engine, db, nil
}

func writeSkillLines(cmd *cobra.Command, lines []string) error {
	out := cmd.OutOrStdout()
	for _, line := range lines {
		if _, err := fmt.Fprintln(out, line); err != nil {
			return err
		}
	}
	return nil
}

func skillOriginScope(raw string) string {
	var origin struct {
		Scope string `json:"scope"`
	}
	if err := json.Unmarshal([]byte(raw), &origin); err != nil || origin.Scope == "" {
		return "unknown"
	}
	return origin.Scope
}

func newSkillsListCmd(gf *globalFlags) *cobra.Command {
	var state string
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List skill packages",
		RunE: func(cmd *cobra.Command, args []string) error {
			switch state {
			case "", skills.StateQuarantined, skills.StateActive, skills.StateDisabled, skills.StateRejected, skills.StateConflict:
			default:
				return &usageError{fmt.Errorf("%s: invalid --state %q", cmd.CommandPath(), state)}
			}
			engine, db, err := openSkillsEngine(cmd, gf)
			if err != nil {
				return err
			}
			defer func() { _ = db.Close() }()
			report, err := engine.Doctor(cmd.Context())
			if err != nil {
				return err
			}
			lines := []string{"ID\tNAME\tSTATE\tDIGEST\tORIGIN"}
			for _, row := range report.Rows {
				if state != "" && row.State != state {
					continue
				}
				digest := row.Digest
				if len(digest) > 12 {
					digest = digest[:12]
				}
				lines = append(lines, row.ID+"\t"+row.Name+"\t"+row.State+"\t"+digest+"\t"+skillOriginScope(row.Origin))
			}
			if len(lines) == 1 {
				return writeSkillLines(cmd, []string{"no skills"})
			}
			return writeSkillLines(cmd, lines)
		},
	}
	cmd.Flags().StringVar(&state, "state", "", "filter by state")
	return cmd
}

func newSkillsShowCmd(gf *globalFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "show <id>",
		Short: "Show one skill package",
		Args:  exactOneArg,
		RunE: func(cmd *cobra.Command, args []string) error {
			engine, db, err := openSkillsEngine(cmd, gf)
			if err != nil {
				return err
			}
			defer func() { _ = db.Close() }()
			bundle, err := engine.Review(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			return writeSkillLines(cmd, []string{
				"id: " + bundle.ID,
				"name: " + bundle.Name,
				"state: " + bundle.State,
				"digest: " + bundle.Digest,
				"origin: " + bundle.Origin,
				"description: " + bundle.Description,
				"compatibility: " + bundle.Compatibility,
				"requested: " + strings.Join(bundle.Requested, ","),
				"granted: " + strings.Join(bundle.Granted, ","),
				"reviewed: " + bundle.ReviewedAt,
				"findings: " + strings.Join(bundle.Findings, ","),
			})
		},
	}
}

func newSkillsValidateCmd(gf *globalFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "validate <path>",
		Short: "Validate one skill package directory",
		Args:  exactOneArg,
		RunE: func(cmd *cobra.Command, args []string) error {
			engine, db, err := openSkillsEngine(cmd, gf)
			if err != nil {
				return err
			}
			defer func() { _ = db.Close() }()
			digest, findings, err := engine.ValidateDir(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			if len(findings) > 0 {
				lines := append([]string{}, findings...)
				lines = append(lines, "skill_invalid")
				if err := writeSkillLines(cmd, lines); err != nil {
					return err
				}
				return errors.New("skill_invalid")
			}
			return writeSkillLines(cmd, []string{"valid: " + digest})
		},
	}
}

func newSkillsCreateCmd(gf *globalFlags) *cobra.Command {
	var name, description, root string
	cmd := &cobra.Command{
		Use:   "create",
		Short: "Stage a new skill draft into quarantine",
		RunE: func(cmd *cobra.Command, args []string) error {
			if strings.TrimSpace(name) == "" || strings.TrimSpace(description) == "" {
				return &usageError{fmt.Errorf("%s requires --name and --description", cmd.CommandPath())}
			}
			result, err := config.Load(gf.configPath)
			if err != nil {
				return err
			}
			if result.Config.Skills == nil || !result.Config.Skills.Enabled {
				return errors.New("skills are not enabled")
			}
			if root == "" {
				if len(result.Config.Skills.Roots) == 0 {
					return errors.New("skills roots are not configured")
				}
				root = result.Config.Skills.Roots[0]
			}
			engine, db, err := openSkillsEngine(cmd, gf)
			if err != nil {
				return err
			}
			defer func() { _ = db.Close() }()
			summary, err := engine.StageDraft(cmd.Context(), name, description, root)
			if err != nil {
				return err
			}
			return writeSkillLines(cmd, []string{"id: " + summary.ID, "digest: " + summary.Digest})
		},
	}
	cmd.Flags().StringVar(&name, "name", "", "skill name")
	cmd.Flags().StringVar(&description, "description", "", "skill description")
	cmd.Flags().StringVar(&root, "root", "", "skill root (default: first configured root)")
	return cmd
}

func newSkillsReviewCmd(gf *globalFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "review <id>",
		Short: "Show the review bundle for a quarantined skill",
		Args:  exactOneArg,
		RunE: func(cmd *cobra.Command, args []string) error {
			engine, db, err := openSkillsEngine(cmd, gf)
			if err != nil {
				return err
			}
			defer func() { _ = db.Close() }()
			bundle, err := engine.Review(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			if bundle.State != skills.StateQuarantined {
				if err := writeSkillLines(cmd, []string{"state: " + bundle.State}); err != nil {
					return err
				}
				return fmt.Errorf("skill %q is not reviewable", args[0])
			}
			return writeSkillLines(cmd, []string{
				"id: " + bundle.ID,
				"name: " + bundle.Name,
				"origin: " + bundle.Origin,
				"digest: " + bundle.Digest,
				"description: " + bundle.Description,
				"compatibility: " + bundle.Compatibility,
				"requested: " + strings.Join(bundle.Requested, ","),
				"allowed-tools: " + strings.Join(bundle.AllowedTools, ","),
				"granted: " + strings.Join(bundle.Granted, ","),
				"scripts: " + strings.Join(bundle.Scripts, ","),
				"files: " + strings.Join(bundle.Files, ","),
				"links: " + strings.Join(bundle.Links, ","),
				"findings: " + strings.Join(bundle.Findings, ","),
			})
		},
	}
}

func newSkillsAcceptCmd(gf *globalFlags) *cobra.Command {
	var digest string
	var grants []string
	cmd := &cobra.Command{
		Use:   "accept <id>",
		Short: "Accept a quarantined skill with exact grants",
		Args:  exactOneArg,
		RunE: func(cmd *cobra.Command, args []string) error {
			if strings.TrimSpace(digest) == "" {
				return &usageError{fmt.Errorf("%s requires --digest", cmd.CommandPath())}
			}
			engine, db, err := openSkillsEngine(cmd, gf)
			if err != nil {
				return err
			}
			defer func() { _ = db.Close() }()
			if _, err := engine.Accept(cmd.Context(), args[0], digest, grants); err != nil {
				return err
			}
			return writeSkillLines(cmd, []string{"accepted: " + args[0]})
		},
	}
	cmd.Flags().StringVar(&digest, "digest", "", "exact content digest")
	cmd.Flags().StringSliceVar(&grants, "grant", nil, "granted capability (repeatable)")
	return cmd
}

func newSkillsRejectCmd(gf *globalFlags) *cobra.Command {
	var reason string
	cmd := &cobra.Command{
		Use:   "reject <id>",
		Short: "Reject a quarantined skill",
		Args:  exactOneArg,
		RunE: func(cmd *cobra.Command, args []string) error {
			engine, db, err := openSkillsEngine(cmd, gf)
			if err != nil {
				return err
			}
			defer func() { _ = db.Close() }()
			if _, err := engine.Reject(cmd.Context(), args[0], reason); err != nil {
				return err
			}
			return writeSkillLines(cmd, []string{"rejected: " + args[0]})
		},
	}
	cmd.Flags().StringVar(&reason, "reason", "", "rejection reason")
	return cmd
}

func newSkillsDisableCmd(gf *globalFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "disable <id>",
		Short: "Disable an active skill",
		Args:  exactOneArg,
		RunE: func(cmd *cobra.Command, args []string) error {
			engine, db, err := openSkillsEngine(cmd, gf)
			if err != nil {
				return err
			}
			defer func() { _ = db.Close() }()
			if err := engine.Disable(cmd.Context(), args[0]); err != nil {
				return err
			}
			return writeSkillLines(cmd, []string{"disabled: " + args[0]})
		},
	}
}

func newSkillsDoctorCmd(gf *globalFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "doctor",
		Short: "Report skill registry health",
		RunE: func(cmd *cobra.Command, args []string) error {
			engine, db, err := openSkillsEngine(cmd, gf)
			if err != nil {
				return err
			}
			defer func() { _ = db.Close() }()
			report, err := engine.Doctor(cmd.Context())
			if err != nil {
				return err
			}
			lines := make([]string, 0, len(report.Rows)+len(report.Conflicts))
			for _, row := range report.Rows {
				lines = append(lines, row.ID+" "+row.State+" "+row.Digest+" "+row.Origin+" "+strings.Join(row.Findings, ","))
			}
			for _, group := range report.Conflicts {
				lines = append(lines, "conflict "+group.Name+": "+strings.Join(group.IDs, ","))
			}
			if len(lines) == 0 {
				lines = append(lines, "no skills")
			}
			return writeSkillLines(cmd, lines)
		},
	}
}
