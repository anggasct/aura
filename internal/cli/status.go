package cli

import (
	"errors"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/anggasct/aura/internal/sandbox"
)

// newStatusCmd builds the status command bound to negotiate. The negotiator is
// injected so tests can fix the reported surface without depending on the host
// the suite runs on; the composition root passes sandbox.Negotiate.
func newStatusCmd(negotiate func() (sandbox.Primitives, error)) *cobra.Command {
	return &cobra.Command{
		Use:           "status",
		Short:         "Report sandbox containment availability",
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			primitives, err := negotiate()
			text, available := formatSandboxStatus(primitives, err)
			if _, werr := fmt.Fprintln(cmd.OutOrStdout(), text); werr != nil {
				return werr
			}
			if !available {
				return errors.New("sandbox unavailable")
			}
			return nil
		},
	}
}

// formatSandboxStatus renders the exact containment state an operator can act
// on: when a mandatory primitive is absent the line names every one of them,
// matching the Require gate's vocabulary so the status surface and the
// fail-closed gate never disagree.
func formatSandboxStatus(have sandbox.Primitives, negotiateErr error) (string, bool) {
	if negotiateErr != nil {
		return "sandbox: unavailable\nreason: " + negotiateErr.Error(), false
	}
	missing := sandbox.MissingMandatory(have)
	if len(missing) == 0 {
		return "sandbox: available", true
	}
	return "sandbox: unavailable\nmissing: " + strings.Join(missing, ", "), false
}
