package cli

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/spf13/cobra"

	"github.com/anggasct/aura/internal/knowledge"
)

func embedOutcomeLines(collection string, outcome knowledge.EmbedOutcome) []string {
	switch outcome.State {
	case knowledge.EmbedOutcomeEmbedded:
		return []string{fmt.Sprintf("embedding: %d chunks sent for vectors", outcome.Sent)}
	case knowledge.EmbedOutcomeReembedRequired:
		return []string{fmt.Sprintf("embedding: skipped (%s): the collection was embedded under another configuration; nothing was sent, run aura kb embed %s --apply", knowledge.ErrorCodeReembedRequired, collection)}
	case knowledge.EmbedOutcomeUnavailable:
		return []string{fmt.Sprintf("embedding: skipped (%s): embedding is not configured or its credential could not be resolved; nothing was sent", knowledge.ErrorCodeEmbeddingUnavailable)}
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

func newKBEmbedCmd(gf *globalFlags) *cobra.Command {
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
			return withKnowledge(cmd, gf, func(ctx context.Context, logger *slog.Logger, service *knowledge.Service) error {
				if off {
					return runKBEmbedOff(ctx, cmd, logger, service, args[0], apply)
				}
				if !apply {
					return runKBEmbedPreview(ctx, cmd, service, args[0])
				}
				return runKBEmbedApply(ctx, cmd, logger, service, args[0])
			})
		},
	}
	cmd.Flags().BoolVar(&apply, "apply", false, "send chunks and store vectors (default dry-run)")
	cmd.Flags().BoolVar(&off, "off", false, "clear the opt-in and erase the vectors; needs --apply to take effect")
	return cmd
}

func queryPolicyLine(publicQueries bool) string {
	if publicQueries {
		return "queries: owner, trusted and public queries are sent to the same destination once the collection is fully embedded (knowledge.embedding.public_queries is true)"
	}
	return "queries: owner and trusted queries are sent to the same destination once the collection is fully embedded; public queries are not sent (knowledge.embedding.public_queries is false)"
}

func runKBEmbedPreview(ctx context.Context, cmd *cobra.Command, service *knowledge.Service, collection string) error {
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
	return writeProfileLines(cmd, lines)
}

func runKBEmbedApply(ctx context.Context, cmd *cobra.Command, logger *slog.Logger, service *knowledge.Service, collection string) error {
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
	if writeErr := writeProfileLines(cmd, lines); writeErr != nil {
		return writeErr
	}
	return err
}

func runKBEmbedOff(ctx context.Context, cmd *cobra.Command, logger *slog.Logger, service *knowledge.Service, collection string, apply bool) error {
	outcome, err := service.EmbedOff(ctx, collection, apply)
	if err != nil && !outcome.Applied {
		return err
	}
	if !apply {
		return writeProfileLines(cmd, []string{
			fmt.Sprintf("dry-run: would delete %d vectors of collection %s and clear its embedding opt-in; rerun with --apply", outcome.Vectors, collection),
		})
	}
	logger.InfoContext(ctx, "knowledge embedding switched off",
		"component", "knowledge", "operation", "embed_off", "collection", collection, "vectors", outcome.Vectors)
	lines := []string{fmt.Sprintf("embedding off: %d vectors deleted and the opt-in cleared", outcome.Vectors)}
	if !outcome.WALTruncated {
		lines = append(lines, "warning: the write-ahead log could not be truncated because a reader was active")
	}
	lines = append(lines, knowledgeRemovalNotes...)
	if writeErr := writeProfileLines(cmd, lines); writeErr != nil {
		return writeErr
	}
	return err
}
