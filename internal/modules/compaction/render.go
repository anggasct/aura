package compaction

import (
	"fmt"
	"strings"

	"github.com/anggasct/aura/internal/platform/turn"
)

func RenderSummary(summary *Summary) string {
	if summary == nil {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "[untrusted_context_summary id=%s trust=%s seq=%d-%d]\n",
		turn.NeutralizeUntrustedMarkers(summary.ID),
		summary.Trust,
		summary.StartSequence,
		summary.EndSequence,
	)
	writeSummarySection(&b, "goals", summary.Content.Goals)
	writeSummarySection(&b, "decisions", summary.Content.Decisions)
	writeSummarySection(&b, "constraints", summary.Content.Constraints)
	writeSummarySection(&b, "open_work", summary.Content.OpenWork)
	writeSummarySection(&b, "facts", summary.Content.Facts)
	b.WriteString("[/untrusted_context_summary]")
	return b.String()
}

func RenderUnavailableRange(startSeq, endSeq uint64, eventCount int) string {
	return fmt.Sprintf(
		"[untrusted_context_summary trust=%s seq=%d-%d status=unavailable]\nEarlier session events %d-%d (%d events) were omitted to stay within the model context window; no summary was available.\n[/untrusted_context_summary]",
		TrustDerivedUntrusted, startSeq, endSeq, startSeq, endSeq, eventCount,
	)
}

func writeSummarySection(b *strings.Builder, label string, items []string) {
	if len(items) == 0 {
		return
	}
	b.WriteString(label + ":\n")
	for _, item := range items {
		b.WriteString("- " + turn.QuoteUntrusted(item) + "\n")
	}
}
