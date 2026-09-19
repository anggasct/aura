package cli

import (
	"errors"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/anggasct/aura/internal/config"
	"github.com/anggasct/aura/internal/profile"
)

const profileLocalOwner = "local-owner"

func newProfileCmd(gf *globalFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "profile",
		Short: "Inspect and manage the learned owner profile",
	}
	cmd.AddCommand(
		newProfileListCmd(gf),
		newProfileShowCmd(gf),
		newProfileReviewCmd(gf),
		newProfileAcceptCmd(gf),
		newProfileRejectCmd(gf),
		newProfileSetCmd(gf),
		newProfileDeleteCmd(gf),
	)
	return cmd
}

func openProfileService(cmd *cobra.Command, gf *globalFlags) (*profile.Service, func(), error) {
	result, err := config.Load(gf.configPath)
	if err != nil {
		return nil, nil, err
	}
	if result.Config.Profile == nil || !result.Config.Profile.Enabled {
		return nil, nil, errors.New("user profiling is not enabled")
	}
	db, err := openStorage(cmd.Context(), result.Config)
	if err != nil {
		return nil, nil, err
	}
	closer := func() { _ = db.Close() }
	service, err := profile.NewService(newProfileRegistry(db), profile.Config{
		MinConfidence: result.Config.Profile.CandidateMinConfidence,
		Actions:       newProfileActionSink(db, profileLocalOwner),
	})
	if err != nil {
		closer()
		return nil, nil, err
	}
	return service, closer, nil
}

func writeProfileLines(cmd *cobra.Command, lines []string) error {
	out := cmd.OutOrStdout()
	for _, line := range lines {
		if _, err := fmt.Fprintln(out, line); err != nil {
			return err
		}
	}
	return nil
}

func newProfileListCmd(gf *globalFlags) *cobra.Command {
	var category, status string
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List profile facts",
		RunE: func(cmd *cobra.Command, args []string) error {
			service, closer, err := openProfileService(cmd, gf)
			if err != nil {
				return err
			}
			defer closer()
			facts, err := service.ListFacts(cmd.Context(), profileLocalOwner, status, category, 1000)
			if err != nil {
				return err
			}
			if len(facts) == 0 {
				return writeProfileLines(cmd, []string{"no facts"})
			}
			lines := make([]string, 0, len(facts)+1)
			for i := range facts {
				fact := &facts[i]
				lines = append(lines, fmt.Sprintf("%s %s/%s=%s status=%s conf=%.2f origin=%s%s",
					fact.ID, fact.Category, fact.Key, truncateProfileText(fact.Value, 40), fact.Status,
					fact.Confidence, fact.Origin, verifiedMark(fact.OwnerVerified)))
			}
			return writeProfileLines(cmd, lines)
		},
	}
	cmd.Flags().StringVar(&category, "category", "", "filter by category")
	cmd.Flags().StringVar(&status, "status", "", "filter by status (candidate|active|rejected|expired)")
	return cmd
}

func newProfileShowCmd(gf *globalFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "show <fact-id>",
		Short: "Show one fact with provenance",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			service, closer, err := openProfileService(cmd, gf)
			if err != nil {
				return err
			}
			defer closer()
			fact, err := service.GetFact(cmd.Context(), profileLocalOwner, args[0])
			if err != nil {
				return err
			}
			evidence, err := service.EvidenceFor(cmd.Context(), fact.ID)
			if err != nil {
				return err
			}
			lines := []string{
				"id: " + fact.ID,
				"category: " + fact.Category,
				"key: " + fact.Key,
				"value: " + truncateProfileText(fact.Value, 200),
				fmt.Sprintf("status: %s origin: %s%s conf: %.2f", fact.Status, fact.Origin, verifiedMark(fact.OwnerVerified), fact.Confidence),
				"created: " + fact.CreatedAt.Format(time.RFC3339),
			}
			if fact.ConflictsWithID != "" {
				lines = append(lines, "conflicts-with: "+fact.ConflictsWithID)
			}
			if fact.ExpiresAt != nil {
				lines = append(lines, "expires-at: "+fact.ExpiresAt.Format(time.RFC3339))
			}
			lines = append(lines, fmt.Sprintf("evidence: %d", len(evidence)))
			for i := range evidence {
				item := &evidence[i]
				lines = append(lines, fmt.Sprintf("  - event=%s model=%s/%s prompt=%s",
					item.SourceEventID, item.Provider, item.Model, item.PromptVersion))
			}
			return writeProfileLines(cmd, lines)
		},
	}
}

func newProfileReviewCmd(gf *globalFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "review",
		Short: "List candidates needing review, conflicts first",
		RunE: func(cmd *cobra.Command, args []string) error {
			service, closer, err := openProfileService(cmd, gf)
			if err != nil {
				return err
			}
			defer closer()
			candidates, err := service.ListFacts(cmd.Context(), profileLocalOwner, profile.StatusCandidate, "", 1000)
			if err != nil {
				return err
			}
			if len(candidates) == 0 {
				return writeProfileLines(cmd, []string{"no candidates"})
			}
			var conflicted, plain []string
			for i := range candidates {
				fact := &candidates[i]
				line := fmt.Sprintf("%s %s/%s=%s conf=%.2f%s",
					fact.ID, fact.Category, fact.Key, truncateProfileText(fact.Value, 40), fact.Confidence, conflictMark(fact.ConflictsWithID))
				if fact.ConflictsWithID != "" {
					conflicted = append(conflicted, line)
				} else {
					plain = append(plain, line)
				}
			}
			lines := make([]string, 0, len(conflicted)+len(plain))
			lines = append(lines, conflicted...)
			lines = append(lines, plain...)
			return writeProfileLines(cmd, lines)
		},
	}
}

func newProfileAcceptCmd(gf *globalFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "accept <fact-id>",
		Short: "Accept a candidate fact",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			service, closer, err := openProfileService(cmd, gf)
			if err != nil {
				return err
			}
			defer closer()
			fact, err := service.AcceptFact(cmd.Context(), profileLocalOwner, args[0], time.Now().UTC())
			if err != nil {
				return err
			}
			return writeProfileLines(cmd, []string{"accepted: " + fact.ID})
		},
	}
}

func newProfileRejectCmd(gf *globalFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "reject <fact-id>",
		Short: "Reject a candidate fact",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			service, closer, err := openProfileService(cmd, gf)
			if err != nil {
				return err
			}
			defer closer()
			if err := service.RejectFact(cmd.Context(), profileLocalOwner, args[0], time.Now().UTC()); err != nil {
				return err
			}
			return writeProfileLines(cmd, []string{"rejected: " + args[0]})
		},
	}
}

func newProfileSetCmd(gf *globalFlags) *cobra.Command {
	var category, key, value, expiresAt string
	cmd := &cobra.Command{
		Use:   "set",
		Short: "Set an owner-verified fact",
		RunE: func(cmd *cobra.Command, args []string) error {
			if category == "" || key == "" || value == "" {
				return &usageError{err: errors.New("category, key, and value are required")}
			}
			var expires *time.Time
			if expiresAt != "" {
				parsed, err := time.Parse(time.RFC3339, expiresAt)
				if err != nil {
					return &usageError{err: errors.New("expires-at must be RFC3339")}
				}
				if !parsed.After(time.Now().UTC()) {
					return &usageError{err: errors.New("expires-at must be in the future")}
				}
				expires = &parsed
			}
			service, closer, err := openProfileService(cmd, gf)
			if err != nil {
				return err
			}
			defer closer()
			fact, err := service.SetFact(cmd.Context(), profileLocalOwner, category, key, value, expires, time.Now().UTC())
			if err != nil {
				return err
			}
			return writeProfileLines(cmd, []string{"set: " + fact.ID})
		},
	}
	cmd.Flags().StringVar(&category, "category", "", "fact category")
	cmd.Flags().StringVar(&key, "key", "", "fact key")
	cmd.Flags().StringVar(&value, "value", "", "fact value")
	cmd.Flags().StringVar(&expiresAt, "expires-at", "", "RFC3339 expiry")
	return cmd
}

func newProfileDeleteCmd(gf *globalFlags) *cobra.Command {
	var force bool
	cmd := &cobra.Command{
		Use:   "delete <fact-id>",
		Short: "Delete a fact (tombstone)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if !force {
				return &usageError{err: errors.New("pass --force to delete a fact")}
			}
			service, closer, err := openProfileService(cmd, gf)
			if err != nil {
				return err
			}
			defer closer()
			if err := service.DeleteFact(cmd.Context(), profileLocalOwner, args[0], time.Now().UTC()); err != nil {
				return err
			}
			return writeProfileLines(cmd, []string{"deleted: " + args[0]})
		},
	}
	cmd.Flags().BoolVar(&force, "force", false, "confirm deletion")
	return cmd
}

func truncateProfileText(text string, limit int) string {
	runes := []rune(text)
	if len(runes) <= limit {
		return text
	}
	return string(runes[:limit]) + "..."
}

func verifiedMark(verified bool) string {
	if verified {
		return " verified"
	}
	return ""
}

func conflictMark(conflictsWith string) string {
	if conflictsWith != "" {
		return " conflicts-with=" + conflictsWith
	}
	return ""
}
