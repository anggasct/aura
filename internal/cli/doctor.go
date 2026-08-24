package cli

import (
	"errors"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/anggasct/aura/internal/config"
	"github.com/anggasct/aura/internal/health"
	"github.com/anggasct/aura/internal/sandbox"
)

// newDoctorCmd builds the doctor command: the detailed local diagnostics
// surface. Unlike status it never probes the running process; it always
// evaluates local checks.
func newDoctorCmd(gf *globalFlags, negotiate func() (sandbox.Primitives, error)) *cobra.Command {
	var checkID string
	cmd := &cobra.Command{
		Use:           "doctor [--check <id>]",
		Short:         "Run local diagnostics and print detailed findings",
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			loaded, err := config.Load(gf.configPath)
			if err != nil {
				return &exitCodeError{code: exitCommand, err: err}
			}
			registry, err := buildHealthRegistry(loaded.Config, mapCapabilityStatuses(loaded.CapabilityReport), negotiate)
			if err != nil {
				return &exitCodeError{code: exitCommand, err: err}
			}
			findings := registry.Evaluate(ctx)
			if checkID != "" {
				findings = filterFindingsByCheck(findings, checkID)
				if len(findings) == 0 {
					return &exitCodeError{code: exitCommand, err: fmt.Errorf("no findings for check %q", checkID)}
				}
			}
			out := cmd.OutOrStdout()
			for i := range findings {
				if _, err := fmt.Fprintln(out, formatDoctorFinding(&findings[i])); err != nil {
					return err
				}
			}
			switch health.WorstSeverity(findings) {
			case health.SeverityCritical:
				return &exitCodeError{code: exitCritical, err: errors.New("critical finding present")}
			case health.SeverityWarning:
				return &exitCodeError{code: exitDegraded, err: errors.New("degraded finding present")}
			default:
				return nil
			}
		},
	}
	cmd.Flags().StringVar(&checkID, "check", "", "report only the named check")
	return cmd
}

func filterFindingsByCheck(findings []health.Finding, checkID string) []health.Finding {
	var filtered []health.Finding
	for i := range findings {
		if findings[i].Component == checkID || strings.HasPrefix(findings[i].ID, checkID+"/") {
			filtered = append(filtered, findings[i])
		}
	}
	return filtered
}

// formatDoctorFinding renders one finding with every registry-owned field:
// the stable ID first so an operator can grep a single finding across runs.
func formatDoctorFinding(f *health.Finding) string {
	var b strings.Builder
	b.WriteString(f.ID)
	b.WriteString("\n  status:     " + string(f.Status))
	b.WriteString("\n  severity:   " + string(f.Severity))
	b.WriteString("\n  component:  " + f.Component)
	b.WriteString("\n  scope:      " + f.Scope)
	b.WriteString("\n  code:       " + f.Code)
	b.WriteString("\n  detail:     " + f.Detail)
	if f.Stale {
		b.WriteString("\n  stale:      true")
	}
	if f.Remediation != "" && f.Status != health.StatusUp {
		b.WriteString("\n  remediation: " + f.Remediation)
	}
	b.WriteString("\n  checked_at: " + f.CheckedAt.Format(timeFormatRFC3339Nano))
	b.WriteString("\n  first_seen: " + f.FirstSeen.Format(timeFormatRFC3339Nano))
	b.WriteString("\n  last_seen:  " + f.LastSeen.Format(timeFormatRFC3339Nano))
	return b.String()
}

const timeFormatRFC3339Nano = "2006-01-02T15:04:05.000000000Z07:00"
