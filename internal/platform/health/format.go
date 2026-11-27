package health

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/anggasct/aura/internal/platform/clikit"
)

const (
	ExitHealthy  = 0
	ExitDegraded = 1
	ExitCritical = 2
	ExitCommand  = 3

	timeFormatRFC3339Nano = "2006-01-02T15:04:05.000000000Z07:00"
)

type StatusJSON struct {
	Status   string         `json:"status"`
	Severity string         `json:"severity"`
	Live     LiveStatusJSON `json:"live"`
	Findings []Finding      `json:"findings"`
}

type LiveStatusJSON struct {
	Reachable bool   `json:"reachable"`
	Status    string `json:"status,omitempty"`
	Code      string `json:"code,omitempty"`
}

func FilterFindingsByCheck(findings []Finding, checkID string) []Finding {
	var filtered []Finding
	for i := range findings {
		if findings[i].Component == checkID || strings.HasPrefix(findings[i].ID, checkID+"/") {
			filtered = append(filtered, findings[i])
		}
	}
	return filtered
}

func FormatDoctorFinding(f *Finding) string {
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
	if f.Remediation != "" && f.Status != StatusUp {
		b.WriteString("\n  remediation: " + f.Remediation)
	}
	b.WriteString("\n  checked_at: " + f.CheckedAt.Format(timeFormatRFC3339Nano))
	b.WriteString("\n  first_seen: " + f.FirstSeen.Format(timeFormatRFC3339Nano))
	b.WriteString("\n  last_seen:  " + f.LastSeen.Format(timeFormatRFC3339Nano))
	return b.String()
}

func FormatFindingLine(f *Finding) string {
	var b strings.Builder
	b.WriteString(string(f.Severity))
	b.WriteString(" ")
	b.WriteString(f.Component)
	b.WriteString(" ")
	b.WriteString(f.Code)
	if f.Detail != "" {
		b.WriteString(": ")
		b.WriteString(f.Detail)
	}
	if f.Stale {
		b.WriteString(" [stale]")
	}
	if f.Remediation != "" && f.Status != StatusUp {
		b.WriteString(" (remediation: " + f.Remediation + ")")
	}
	return b.String()
}

func WriteStatusText(out io.Writer, sandboxText string, sandboxAvailable bool, findings []Finding, live ProbeBody, liveReachable bool) error {
	if _, err := fmt.Fprintln(out, sandboxText); err != nil {
		return err
	}
	if liveReachable {
		liveLine := "live: " + live.Status + " (" + live.Code + ")"
		if _, err := fmt.Fprintln(out, liveLine); err != nil {
			return err
		}
	} else {
		if _, err := fmt.Fprintln(out, "live: unreachable (local checks only)"); err != nil {
			return err
		}
	}
	for i := range findings {
		if _, err := fmt.Fprintln(out, FormatFindingLine(&findings[i])); err != nil {
			return err
		}
	}
	return StatusExit(findings, sandboxAvailable)
}

func WriteStatusJSON(out io.Writer, sandboxAvailable bool, findings []Finding, live ProbeBody, liveReachable bool) error {
	payload := StatusJSON{
		Status:   string(WorstStatus(findings)),
		Severity: string(WorstSeverity(findings)),
		Live: LiveStatusJSON{
			Reachable: liveReachable,
			Status:    live.Status,
			Code:      live.Code,
		},
		Findings: findings,
	}
	encoded, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return fmt.Errorf("status: encode output: %w", err)
	}
	if _, err := fmt.Fprintln(out, string(encoded)); err != nil {
		return err
	}
	return StatusExit(findings, sandboxAvailable)
}

func StatusExit(findings []Finding, sandboxAvailable bool) error {
	switch WorstSeverity(findings) {
	case SeverityCritical:
		return clikit.NewExitCodeError(ExitCritical, errors.New("critical finding present"))
	case SeverityWarning:
		return clikit.NewExitCodeError(ExitDegraded, errors.New("degraded finding present"))
	default:
		if !sandboxAvailable {
			return clikit.NewExitCodeError(ExitDegraded, errors.New("sandbox unavailable"))
		}
		return nil
	}
}

func DoctorExit(findings []Finding) error {
	switch WorstSeverity(findings) {
	case SeverityCritical:
		return clikit.NewExitCodeError(ExitCritical, errors.New("critical finding present"))
	case SeverityWarning:
		return clikit.NewExitCodeError(ExitDegraded, errors.New("degraded finding present"))
	default:
		return nil
	}
}
