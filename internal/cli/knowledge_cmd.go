package cli

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/spf13/cobra"

	"github.com/anggasct/aura/internal/config"
	"github.com/anggasct/aura/internal/modules/knowledge"
)

const digestPrefixLength = 12

var knowledgeRemovalNotes = []string{
	"not removed: earlier replies, recorded tool results, the durable runtime journal and older backups may still hold copies",
	"not removed: copies an embedding provider holds under its own terms, and vectors a running server keeps in memory until its next retrieval or restart",
	"raw database pages can keep fragments until the database is vacuumed",
}

func newKBCmd(gf *globalFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "kb",
		Short: "Manage knowledge collections",
	}
	cmd.AddCommand(
		newKBAddCmd(gf),
		newKBListCmd(gf),
		newKBShowCmd(gf),
		newKBRemoveCmd(gf),
		newKBRefreshCmd(gf),
		newKBEmbedCmd(gf),
		newKBCheckCmd(gf),
	)
	return cmd
}

func withKnowledge(cmd *cobra.Command, gf *globalFlags, fn func(context.Context, *slog.Logger, *knowledge.Service) error) error {
	return withStorage(cmd, gf, true, func(ctx context.Context, logger *slog.Logger, cfg *config.Config, opened *sql.DB) error {
		service, err := newKnowledgeService(cfg, opened, nil)
		if err != nil {
			return err
		}
		return fn(ctx, logger, service)
	})
}

func ingestLine(result *knowledge.IngestResult) string {
	return fmt.Sprintf("%s: %s chunks=%d added=%d removed=%d kept=%d",
		result.Status, result.Label, result.Chunks, result.ChunksAdded, result.ChunksRemoved, result.ChunksKept)
}

func newKBAddCmd(gf *globalFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "add <collection> <path|url>",
		Short: "Ingest a local file or an https URL into a collection",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return withKnowledge(cmd, gf, func(ctx context.Context, logger *slog.Logger, service *knowledge.Service) error {
				result, err := service.Add(ctx, args[0], args[1])
				if result.SourceID != "" {
					logger.InfoContext(ctx, "knowledge source ingested",
						"component", "knowledge", "operation", "add", "collection", result.Collection,
						"status", result.Status, "chunks", result.Chunks)
					lines := append([]string{ingestLine(&result)}, embedOutcomeLines(args[0], result.Embedding)...)
					if writeErr := writeProfileLines(cmd, lines); writeErr != nil {
						return writeErr
					}
				}
				return err
			})
		},
	}
}

type kbListRow struct {
	ID         string `json:"id"`
	Sources    int    `json:"sources"`
	Chunks     int    `json:"chunks"`
	Generation int64  `json:"generation"`
}

type kbListOutput struct {
	Collections []kbListRow `json:"collections"`
}

func newKBListCmd(gf *globalFlags) *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List collections",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return withKnowledge(cmd, gf, func(ctx context.Context, _ *slog.Logger, service *knowledge.Service) error {
				collections, err := service.List(ctx)
				if err != nil {
					return err
				}
				rows := make([]kbListRow, 0, len(collections))
				for i := range collections {
					rows = append(rows, kbListRow{
						ID: collections[i].ID, Sources: collections[i].Sources,
						Chunks: collections[i].Chunks, Generation: collections[i].Generation,
					})
				}
				if asJSON {
					encoder := json.NewEncoder(cmd.OutOrStdout())
					encoder.SetIndent("", "  ")
					return encoder.Encode(kbListOutput{Collections: rows})
				}
				if len(rows) == 0 {
					return writeProfileLines(cmd, []string{"no collections"})
				}
				lines := []string{"COLLECTION\tSOURCES\tCHUNKS\tGENERATION"}
				for i := range rows {
					lines = append(lines, fmt.Sprintf("%s\t%d\t%d\t%d", rows[i].ID, rows[i].Sources, rows[i].Chunks, rows[i].Generation))
				}
				return writeProfileLines(cmd, lines)
			})
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit machine-readable output")
	return cmd
}

func newKBShowCmd(gf *globalFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "show <collection>",
		Short: "Show the sources of a collection",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return withKnowledge(cmd, gf, func(ctx context.Context, _ *slog.Logger, service *knowledge.Service) error {
				detail, err := service.Show(ctx, args[0])
				if err != nil {
					return err
				}
				lines := []string{fmt.Sprintf("collection: %s sources=%d chunks=%d", detail.ID, len(detail.Sources), detail.Chunks)}
				for i := range detail.Sources {
					source := &detail.Sources[i]
					lines = append(lines, fmt.Sprintf("source: %s label=%s kind=%s chunks=%d digest=%s ingested=%s",
						source.URI, source.Label, source.Kind, source.Chunks,
						source.ContentSHA256[:min(digestPrefixLength, len(source.ContentSHA256))],
						source.IngestedAt.UTC().Format(time.RFC3339)))
				}
				return writeProfileLines(cmd, lines)
			})
		},
	}
}

func newKBRemoveCmd(gf *globalFlags) *cobra.Command {
	var apply bool
	cmd := &cobra.Command{
		Use:   "remove <collection> <source>",
		Short: "Preview or apply removal of a source and its chunks",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return withKnowledge(cmd, gf, func(ctx context.Context, logger *slog.Logger, service *knowledge.Service) error {
				outcome, err := service.Remove(ctx, args[0], args[1], apply)
				if err != nil && !outcome.Applied {
					return err
				}
				if !apply {
					return writeProfileLines(cmd, []string{
						fmt.Sprintf("dry-run: would remove %d chunks of the source; rerun with --apply", outcome.Chunks),
					})
				}
				logger.InfoContext(ctx, "knowledge source removed",
					"component", "knowledge", "operation", "remove", "collection", args[0], "chunks", outcome.Chunks)
				lines := []string{fmt.Sprintf("removed: %d chunks", outcome.Chunks)}
				if !outcome.WALTruncated {
					lines = append(lines, "warning: the write-ahead log could not be truncated because a reader was active")
				}
				lines = append(lines, knowledgeRemovalNotes...)
				if writeErr := writeProfileLines(cmd, lines); writeErr != nil {
					return writeErr
				}
				return err
			})
		},
	}
	cmd.Flags().BoolVar(&apply, "apply", false, "apply removal (default dry-run)")
	return cmd
}

func newKBRefreshCmd(gf *globalFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "refresh <collection> [source]",
		Short: "Re-read the sources of a collection",
		Args:  cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			source := ""
			if len(args) == 2 {
				source = args[1]
			}
			return withKnowledge(cmd, gf, func(ctx context.Context, logger *slog.Logger, service *knowledge.Service) error {
				results, err := service.Refresh(ctx, args[0], source)
				lines := make([]string, 0, len(results))
				for i := range results {
					logger.InfoContext(ctx, "knowledge source refreshed",
						"component", "knowledge", "operation", "refresh", "collection", results[i].Collection,
						"status", results[i].Status, "chunks", results[i].Chunks)
					lines = append(lines, ingestLine(&results[i]))
				}
				if len(results) > 0 {
					lines = append(lines, embedOutcomeLines(args[0], results[0].Embedding)...)
				}
				if len(lines) > 0 {
					if writeErr := writeProfileLines(cmd, lines); writeErr != nil {
						return writeErr
					}
				}
				return err
			})
		},
	}
}

func newKBCheckCmd(gf *globalFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "check",
		Short: "Verify the knowledge index against its content",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return withKnowledge(cmd, gf, func(ctx context.Context, _ *slog.Logger, service *knowledge.Service) error {
				report, err := service.Check(ctx)
				if code, coded := knowledge.CodeOf(err); err != nil && (!coded || code != knowledge.ErrorCodeIndexDamaged) {
					return err
				}
				lines := []string{fmt.Sprintf("collections=%d sources=%d chunks=%d indexed=%d",
					report.Collections, report.Sources, report.Chunks, report.IndexedChunks)}
				for _, problem := range report.Problems {
					lines = append(lines, "problem: "+problem)
				}
				if err == nil {
					lines = append(lines, "ok")
				}
				if writeErr := writeProfileLines(cmd, lines); writeErr != nil {
					return writeErr
				}
				return err
			})
		},
	}
}
