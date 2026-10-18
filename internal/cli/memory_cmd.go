package cli

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/spf13/cobra"

	"github.com/anggasct/aura/internal/config"
	"github.com/anggasct/aura/internal/memory"
)

func newMemoryCmd(gf *globalFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "memory",
		Short: "Inspect and forget projected session memory",
	}
	cmd.AddCommand(
		newMemoryInspectCmd(gf),
		newMemoryForgetCmd(gf),
		newMemoryPurgeCmd(gf),
	)
	return cmd
}

func openMemoryServiceForCLI(db *sql.DB, cfg *config.Config) (*memory.Service, error) {
	if db == nil {
		return nil, errors.New("memory database must not be nil")
	}
	return buildMemoryService(cfg, db, nil)
}

func memoryInspectLines(documents []memory.StoredDocument) []string {
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
		lines = append(lines, fmt.Sprintf("%s session=%s kind=%s range=%d-%d trust=%s prompt=%s expires=%s content=%s",
			documents[i].ID, documents[i].SessionID, documents[i].Kind,
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

func newMemoryInspectCmd(gf *globalFlags) *cobra.Command {
	var session, query string
	var limit int
	cmd := &cobra.Command{
		Use:   "inspect",
		Short: "List recall docs with provenance",
		RunE: func(cmd *cobra.Command, args []string) error {
			if limit <= 0 {
				limit = 50
			}
			return withStorage(cmd, gf, true, func(ctx context.Context, logger *slog.Logger, cfg *config.Config, opened *sql.DB) error {
				if !cfg.Memory.Enabled {
					return errors.New("memory is not enabled")
				}
				service, err := openMemoryServiceForCLI(opened, cfg)
				if err != nil {
					return err
				}
				documents, err := service.Inspect(ctx, profileLocalOwner, session, query, limit)
				if err != nil {
					return err
				}
				return writeProfileLines(cmd, memoryInspectLines(documents))
			})
		},
	}
	cmd.Flags().StringVar(&session, "session", "", "filter by session id")
	cmd.Flags().StringVar(&query, "query", "", "search by query text")
	cmd.Flags().IntVar(&limit, "limit", 50, "max documents to list")
	return cmd
}

func newMemoryForgetCmd(gf *globalFlags) *cobra.Command {
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
				return &usageError{err: errors.New("forget requires exactly one of --session, --id, --query")}
			}
			if limit <= 0 {
				limit = 100
			}
			return withStorage(cmd, gf, true, func(ctx context.Context, logger *slog.Logger, cfg *config.Config, opened *sql.DB) error {
				if !cfg.Memory.Enabled {
					return errors.New("memory is not enabled")
				}
				service, err := openMemoryServiceForCLI(opened, cfg)
				if err != nil {
					return err
				}
				var preview []memory.StoredDocument
				var deleted int
				var scope string
				switch {
				case session != "":
					scope = "session=" + session
					preview, deleted, err = service.ForgetBySession(ctx, profileLocalOwner, session, limit, apply)
				case docID != "":
					scope = "id=" + docID
					preview, deleted, err = service.ForgetByID(ctx, profileLocalOwner, docID, apply)
				default:
					scope = "query=" + query
					preview, deleted, err = service.ForgetByQuery(ctx, profileLocalOwner, "", query, limit, apply)
				}
				if err != nil {
					return err
				}
				if !apply {
					if len(preview) == 0 {
						return writeProfileLines(cmd, []string{"dry-run: 0 would be deleted", "no documents"})
					}
					lines := make([]string, 0, len(preview)+1)
					lines = append(lines, fmt.Sprintf("dry-run: %d would be deleted", len(preview)))
					lines = append(lines, memoryInspectLines(preview)...)
					return writeProfileLines(cmd, lines)
				}
				logger.InfoContext(ctx, "memory forget applied",
					"component", "memory",
					"operation", "forget",
					"scope", scope,
					"deleted", deleted,
				)
				return writeProfileLines(cmd, []string{fmt.Sprintf("deleted: %d", deleted)})
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

func newMemoryPurgeCmd(gf *globalFlags) *cobra.Command {
	var limit int
	cmd := &cobra.Command{
		Use:   "purge",
		Short: "Delete expired summary docs",
		RunE: func(cmd *cobra.Command, args []string) error {
			if limit <= 0 {
				limit = 100
			}
			return withStorage(cmd, gf, true, func(ctx context.Context, logger *slog.Logger, cfg *config.Config, opened *sql.DB) error {
				if !cfg.Memory.Enabled {
					return errors.New("memory is not enabled")
				}
				service, err := openMemoryServiceForCLI(opened, cfg)
				if err != nil {
					return err
				}
				pruned, err := service.PurgeExpired(ctx, time.Now().UTC(), limit)
				if err != nil {
					return err
				}
				logger.InfoContext(ctx, "memory purge complete",
					"component", "memory",
					"operation", "purge",
					"pruned", pruned,
				)
				_, err = fmt.Fprintf(cmd.OutOrStdout(), "purged: %d\n", pruned)
				return err
			})
		},
	}
	cmd.Flags().IntVar(&limit, "limit", 100, "max expired docs to delete")
	return cmd
}
