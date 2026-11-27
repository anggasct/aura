package memory

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/spf13/cobra"

	"github.com/anggasct/aura/internal/platform/command"
	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/principal"
)

type Opener func(cmd *cobra.Command, fn func(ctx context.Context, logger *slog.Logger, service *Service) error) error

func Commands(open Opener) []*cobra.Command {
	cmd := &cobra.Command{
		Use:   "memory",
		Short: "Inspect and forget projected session memory",
	}
	cmd.AddCommand(
		newInspectCmd(open),
		newForgetCmd(open),
		newPurgeCmd(open),
	)
	return []*cobra.Command{cmd}
}

func runWithOpener(cmd *cobra.Command, open Opener, fn func(ctx context.Context, logger *slog.Logger, service *Service) error) error {
	if open == nil {
		return fault.New(ErrorCodeInvalidArgument, "memory command opener must not be nil")
	}
	return open(cmd, fn)
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

func memoryInspectLines(documents []StoredDocument) []string {
	if len(documents) == 0 {
		return []string{"no documents"}
	}
	lines := make([]string, 0, len(documents))
	for i := range documents {
		expires := "never"
		if documents[i].ExpiresAt != nil {
			expires = documents[i].ExpiresAt.UTC().Format(time.RFC3339)
		}
		prompt := documents[i].PromptVersion
		if prompt == "" {
			prompt = "-"
		}
		lines = append(lines, fmt.Sprintf("%s agent=%s session=%s kind=%s range=%d-%d trust=%s prompt=%s expires=%s content=%s",
			documents[i].ID, documents[i].AgentID, documents[i].SessionID, documents[i].Kind,
			documents[i].FromSequence, documents[i].ToSequence,
			documents[i].TrustLabel, prompt, expires,
			truncateMemoryText(documents[i].Content, 120)))
	}
	return lines
}

func truncateMemoryText(text string, limit int) string {
	runes := []rune(text)
	if len(runes) <= limit {
		return text
	}
	return string(runes[:limit]) + "..."
}

func newInspectCmd(open Opener) *cobra.Command {
	var session, query string
	var limit int
	cmd := &cobra.Command{
		Use:   "inspect",
		Short: "List recall docs with provenance",
		RunE: func(cmd *cobra.Command, args []string) error {
			if limit <= 0 {
				limit = 50
			}
			return runWithOpener(cmd, open, func(ctx context.Context, _ *slog.Logger, service *Service) error {
				documents, err := service.Inspect(ctx, principal.OwnerID, session, query, limit)
				if err != nil {
					return err
				}
				return writeLines(cmd, memoryInspectLines(documents))
			})
		},
	}
	cmd.Flags().StringVar(&session, "session", "", "filter by session id")
	cmd.Flags().StringVar(&query, "query", "", "search by query text")
	cmd.Flags().IntVar(&limit, "limit", 50, "max documents to list")
	return cmd
}

func newForgetCmd(open Opener) *cobra.Command {
	var session, docID, query string
	var limit int
	var apply bool
	cmd := &cobra.Command{
		Use:   "forget",
		Short: "Preview or apply forgetting of recall docs",
		RunE: func(cmd *cobra.Command, args []string) error {
			scopes := 0
			if session != "" {
				scopes++
			}
			if docID != "" {
				scopes++
			}
			if query != "" {
				scopes++
			}
			if scopes != 1 {
				return command.NewUsageError(errors.New("forget requires exactly one of --session, --id, --query"))
			}
			if limit <= 0 {
				limit = 100
			}
			return runWithOpener(cmd, open, func(ctx context.Context, logger *slog.Logger, service *Service) error {
				var preview []StoredDocument
				var deleted int
				var scope string
				var err error
				switch {
				case session != "":
					scope = "session=" + session
					preview, deleted, err = service.ForgetBySession(ctx, principal.OwnerID, session, limit, apply)
				case docID != "":
					scope = "id=" + docID
					preview, deleted, err = service.ForgetByID(ctx, principal.OwnerID, docID, apply)
				default:
					scope = "query=" + query
					preview, deleted, err = service.ForgetByQuery(ctx, principal.OwnerID, "", query, limit, apply)
				}
				if err != nil {
					return err
				}
				if !apply {
					if len(preview) == 0 {
						return writeLines(cmd, []string{"dry-run: 0 would be deleted", "no documents"})
					}
					lines := make([]string, 0, len(preview)+1)
					lines = append(lines, fmt.Sprintf("dry-run: %d would be deleted", len(preview)))
					lines = append(lines, memoryInspectLines(preview)...)
					return writeLines(cmd, lines)
				}
				if logger != nil {
					logger.InfoContext(ctx, "memory forget applied",
						"component", "memory",
						"operation", "forget",
						"scope", scope,
						"deleted", deleted,
					)
				}
				return writeLines(cmd, []string{fmt.Sprintf("deleted: %d", deleted)})
			})
		},
	}
	cmd.Flags().StringVar(&session, "session", "", "forget one session")
	cmd.Flags().StringVar(&docID, "id", "", "forget one document id")
	cmd.Flags().StringVar(&query, "query", "", "forget by query text")
	cmd.Flags().IntVar(&limit, "limit", 100, "max documents in preview")
	cmd.Flags().BoolVar(&apply, "apply", false, "apply deletion (default dry-run)")
	return cmd
}

func newPurgeCmd(open Opener) *cobra.Command {
	var limit int
	cmd := &cobra.Command{
		Use:   "purge",
		Short: "Delete expired summary docs",
		RunE: func(cmd *cobra.Command, args []string) error {
			if limit <= 0 {
				limit = 100
			}
			return runWithOpener(cmd, open, func(ctx context.Context, logger *slog.Logger, service *Service) error {
				pruned, err := service.PurgeExpired(ctx, time.Now().UTC(), limit)
				if err != nil {
					return err
				}
				if logger != nil {
					logger.InfoContext(ctx, "memory purge complete",
						"component", "memory",
						"operation", "purge",
						"pruned", pruned,
					)
				}
				_, err = fmt.Fprintf(cmd.OutOrStdout(), "purged: %d\n", pruned)
				return err
			})
		},
	}
	cmd.Flags().IntVar(&limit, "limit", 100, "max expired docs to delete")
	return cmd
}
