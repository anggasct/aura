package knowledge

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/spf13/cobra"

	"github.com/anggasct/aura/internal/platform/fault"
)

const digestPrefixLength = 12

var removalNotes = []string{
	"not removed: earlier replies, recorded tool results, the durable runtime journal and older backups may still hold copies",
	"not removed: copies an embedding provider holds under its own terms, and vectors a running server keeps in memory until its next retrieval or restart",
	"raw database pages can keep fragments until the database is vacuumed",
}

type Opener func(cmd *cobra.Command, fn func(ctx context.Context, logger *slog.Logger, service *Service) error) error

type ListRow struct {
	ID         string `json:"id"`
	Sources    int    `json:"sources"`
	Chunks     int    `json:"chunks"`
	Generation int64  `json:"generation"`
}

type ListOutput struct {
	Collections []ListRow `json:"collections"`
}

func Commands(open Opener) []*cobra.Command {
	cmd := &cobra.Command{
		Use:   "kb",
		Short: "Manage knowledge collections",
	}
	cmd.AddCommand(
		newAddCmd(open),
		newListCmd(open),
		newShowCmd(open),
		newRemoveCmd(open),
		newRefreshCmd(open),
		newEmbedCmd(open),
		newCheckCmd(open),
	)
	return []*cobra.Command{cmd}
}

func runWithOpener(cmd *cobra.Command, open Opener, fn func(ctx context.Context, logger *slog.Logger, service *Service) error) error {
	if open == nil {
		return fault.New(ErrorCodeInvalidArgument, "knowledge command opener must not be nil")
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

func ingestLine(result *IngestResult) string {
	return fmt.Sprintf("%s: %s chunks=%d added=%d removed=%d kept=%d",
		result.Status, result.Label, result.Chunks, result.ChunksAdded, result.ChunksRemoved, result.ChunksKept)
}

func embedOutcomeLines(collection string, outcome EmbedOutcome) []string {
	switch outcome.State {
	case EmbedOutcomeEmbedded:
		return []string{fmt.Sprintf("embedding: %d chunks sent for vectors", outcome.Sent)}
	case EmbedOutcomeReembedRequired:
		return []string{fmt.Sprintf("embedding: skipped (%s): the collection was embedded under another configuration; nothing was sent, run aura kb embed %s --apply", ErrorCodeReembedRequired, collection)}
	case EmbedOutcomeUnavailable:
		return []string{fmt.Sprintf("embedding: skipped (%s): embedding is not configured or its credential could not be resolved; nothing was sent", ErrorCodeEmbeddingUnavailable)}
	default:
		return nil
	}
}

func destinationLine(destination string, loopback bool) string {
	if loopback {
		return fmt.Sprintf("destination: %s (this host; nothing leaves it)", destination)
	}
	return fmt.Sprintf("destination: %s (text leaves this host)", destination)
}

func queryPolicyLine(publicQueries bool) string {
	if publicQueries {
		return "queries: owner, trusted and public queries are sent to the same destination once the collection is fully embedded (knowledge.embedding.public_queries is true)"
	}
	return "queries: owner and trusted queries are sent to the same destination once the collection is fully embedded; public queries are not sent (knowledge.embedding.public_queries is false)"
}

func newAddCmd(open Opener) *cobra.Command {
	return &cobra.Command{
		Use:   "add <collection> <path|url>",
		Short: "Ingest a local file or an https URL into a collection",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runWithOpener(cmd, open, func(ctx context.Context, logger *slog.Logger, service *Service) error {
				result, err := service.Add(ctx, args[0], args[1])
				if result.SourceID != "" {
					logger.InfoContext(ctx, "knowledge source ingested",
						"component", "knowledge", "operation", "add", "collection", result.Collection,
						"status", result.Status, "chunks", result.Chunks)
					lines := append([]string{ingestLine(&result)}, embedOutcomeLines(args[0], result.Embedding)...)
					if writeErr := writeLines(cmd, lines); writeErr != nil {
						return writeErr
					}
				}
				return err
			})
		},
	}
}

func newListCmd(open Opener) *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List collections",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runWithOpener(cmd, open, func(ctx context.Context, _ *slog.Logger, service *Service) error {
				collections, err := service.List(ctx)
				if err != nil {
					return err
				}
				rows := make([]ListRow, 0, len(collections))
				for i := range collections {
					rows = append(rows, ListRow{
						ID: collections[i].ID, Sources: collections[i].Sources,
						Chunks: collections[i].Chunks, Generation: collections[i].Generation,
					})
				}
				if asJSON {
					encoder := json.NewEncoder(cmd.OutOrStdout())
					encoder.SetIndent("", "  ")
					return encoder.Encode(ListOutput{Collections: rows})
				}
				if len(rows) == 0 {
					return writeLines(cmd, []string{"no collections"})
				}
				lines := []string{"COLLECTION\tSOURCES\tCHUNKS\tGENERATION"}
				for i := range rows {
					lines = append(lines, fmt.Sprintf("%s\t%d\t%d\t%d", rows[i].ID, rows[i].Sources, rows[i].Chunks, rows[i].Generation))
				}
				return writeLines(cmd, lines)
			})
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit machine-readable output")
	return cmd
}

func newShowCmd(open Opener) *cobra.Command {
	return &cobra.Command{
		Use:   "show <collection>",
		Short: "Show the sources of a collection",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runWithOpener(cmd, open, func(ctx context.Context, _ *slog.Logger, service *Service) error {
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
				return writeLines(cmd, lines)
			})
		},
	}
}

func newRemoveCmd(open Opener) *cobra.Command {
	var apply bool
	cmd := &cobra.Command{
		Use:   "remove <collection> <source>",
		Short: "Preview or apply removal of a source and its chunks",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runWithOpener(cmd, open, func(ctx context.Context, logger *slog.Logger, service *Service) error {
				outcome, err := service.Remove(ctx, args[0], args[1], apply)
				if err != nil && !outcome.Applied {
					return err
				}
				if !apply {
					return writeLines(cmd, []string{
						fmt.Sprintf("dry-run: would remove %d chunks of the source; rerun with --apply", outcome.Chunks),
					})
				}
				logger.InfoContext(ctx, "knowledge source removed",
					"component", "knowledge", "operation", "remove", "collection", args[0], "chunks", outcome.Chunks)
				lines := []string{fmt.Sprintf("removed: %d chunks", outcome.Chunks)}
				if !outcome.WALTruncated {
					lines = append(lines, "warning: the write-ahead log could not be truncated because a reader was active")
				}
				lines = append(lines, removalNotes...)
				if writeErr := writeLines(cmd, lines); writeErr != nil {
					return writeErr
				}
				return err
			})
		},
	}
	cmd.Flags().BoolVar(&apply, "apply", false, "apply removal (default dry-run)")
	return cmd
}

func newRefreshCmd(open Opener) *cobra.Command {
	return &cobra.Command{
		Use:   "refresh <collection> [source]",
		Short: "Re-read the sources of a collection",
		Args:  cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			source := ""
			if len(args) == 2 {
				source = args[1]
			}
			return runWithOpener(cmd, open, func(ctx context.Context, logger *slog.Logger, service *Service) error {
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
					if writeErr := writeLines(cmd, lines); writeErr != nil {
						return writeErr
					}
				}
				return err
			})
		},
	}
}

func newEmbedCmd(open Opener) *cobra.Command {
	var apply, off bool
	cmd := &cobra.Command{
		Use:   "embed <collection>",
		Short: "Preview or apply embedding of a collection, or switch it off",
		Long: "Without --apply, print where chunks would be sent and how many, and change nothing.\n" +
			"With --apply, opt the collection in and store vectors for every chunk, resuming a failed run.\n" +
			"The same command rebuilds a collection whose model or destination changed.\n" +
			"With --off --apply, clear the opt-in and erase the vectors.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runWithOpener(cmd, open, func(ctx context.Context, logger *slog.Logger, service *Service) error {
				if off {
					return runEmbedOff(ctx, cmd, logger, service, args[0], apply)
				}
				if !apply {
					return runEmbedPreview(ctx, cmd, service, args[0])
				}
				return runEmbedApply(ctx, cmd, logger, service, args[0])
			})
		},
	}
	cmd.Flags().BoolVar(&apply, "apply", false, "send chunks and store vectors (default dry-run)")
	cmd.Flags().BoolVar(&off, "off", false, "clear the opt-in and erase the vectors; needs --apply to take effect")
	return cmd
}

func runEmbedPreview(ctx context.Context, cmd *cobra.Command, service *Service, collection string) error {
	preview, err := service.EmbedPreview(ctx, collection)
	if err != nil {
		return err
	}
	lines := []string{
		fmt.Sprintf("dry-run: collection %s would send %d of %d chunks (about %d tokens, estimated) using model %s at %d dimensions",
			preview.Collection, preview.Pending, preview.Chunks, preview.EstimatedTokens, preview.Model, preview.Dimensions),
		destinationLine(preview.Destination, preview.Loopback),
	}
	if preview.Reembed {
		lines = append(lines, "reembed: the stored model or destination differs from the configuration; vectors are rebuilt and none are searched until the run completes")
	}
	lines = append(lines, queryPolicyLine(preview.PublicQueries))
	if preview.Pending == 0 {
		lines = append(lines, "nothing to send: every chunk already has a vector")
	} else {
		lines = append(lines, "rerun with --apply to send")
	}
	return writeLines(cmd, lines)
}

func runEmbedApply(ctx context.Context, cmd *cobra.Command, logger *slog.Logger, service *Service, collection string) error {
	result, err := service.Embed(ctx, collection)
	if result.Destination == "" {
		return err
	}
	logger.InfoContext(ctx, "knowledge collection embedded",
		"component", "knowledge", "operation", "embed", "collection", collection,
		"sent", result.Sent, "batches", result.Batches, "completed", result.Completed)
	lines := []string{fmt.Sprintf("embedded: %d chunks in %d batches sent to %s", result.Sent, result.Batches, result.Destination)}
	if result.Completed {
		lines = append(lines, "complete: every chunk has a vector")
	} else {
		lines = append(lines, fmt.Sprintf("partial: rerun aura kb embed %s --apply to resume", collection))
	}
	if writeErr := writeLines(cmd, lines); writeErr != nil {
		return writeErr
	}
	return err
}

func runEmbedOff(ctx context.Context, cmd *cobra.Command, logger *slog.Logger, service *Service, collection string, apply bool) error {
	outcome, err := service.EmbedOff(ctx, collection, apply)
	if err != nil && !outcome.Applied {
		return err
	}
	if !apply {
		return writeLines(cmd, []string{
			fmt.Sprintf("dry-run: would delete %d vectors of collection %s and clear its embedding opt-in; rerun with --apply", outcome.Vectors, collection),
		})
	}
	logger.InfoContext(ctx, "knowledge embedding switched off",
		"component", "knowledge", "operation", "embed_off", "collection", collection, "vectors", outcome.Vectors)
	lines := []string{fmt.Sprintf("embedding off: %d vectors deleted and the opt-in cleared", outcome.Vectors)}
	if !outcome.WALTruncated {
		lines = append(lines, "warning: the write-ahead log could not be truncated because a reader was active")
	}
	lines = append(lines, removalNotes...)
	if writeErr := writeLines(cmd, lines); writeErr != nil {
		return writeErr
	}
	return err
}

func newCheckCmd(open Opener) *cobra.Command {
	return &cobra.Command{
		Use:   "check",
		Short: "Verify the knowledge index against its content",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runWithOpener(cmd, open, func(ctx context.Context, _ *slog.Logger, service *Service) error {
				report, err := service.Check(ctx)
				if err != nil && !fault.HasCode(err, ErrorCodeIndexDamaged) {
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
				if writeErr := writeLines(cmd, lines); writeErr != nil {
					return writeErr
				}
				return err
			})
		},
	}
}
