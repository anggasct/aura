package knowledge

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestSplitChunksSplitsOnHeadingsAndKeepsPreamble(t *testing.T) {
	text := "intro line\n\n# Refunds\nrefund body\n\n## Shipping times\nshipping body\n#nospace stays body\n"
	chunks, exceeded := SplitChunks(text, 100)
	if exceeded {
		t.Fatal("unexpected cap")
	}
	want := []struct{ heading, content string }{
		{"", "intro line"},
		{"Refunds", "refund body"},
		{"Shipping times", "shipping body\n#nospace stays body"},
	}
	if len(chunks) != len(want) {
		t.Fatalf("chunks = %+v, want %d", chunks, len(want))
	}
	for i, chunk := range chunks {
		if chunk.Ordinal != i || chunk.Heading != want[i].heading || chunk.Content != want[i].content {
			t.Errorf("chunk %d = %+v, want heading %q content %q", i, chunk, want[i].heading, want[i].content)
		}
	}
}

func TestSplitChunksIgnoresHeadingsInsideCodeFences(t *testing.T) {
	text := "# Real\nbefore\n```\n# not a heading\n```\nafter\n~~~\n## also not\n~~~\n"
	chunks, _ := SplitChunks(text, 100)
	if len(chunks) != 1 || chunks[0].Heading != "Real" || !strings.Contains(chunks[0].Content, "# not a heading") || !strings.Contains(chunks[0].Content, "## also not") {
		t.Fatalf("chunks = %+v, want a single Real chunk keeping fenced hashes", chunks)
	}
}

func TestSplitChunksHeadingIsSingleLineAndCapped(t *testing.T) {
	long := strings.Repeat("title ", 60)
	chunks, _ := SplitChunks("#   "+long+"  ##\nbody", 100)
	if len(chunks) != 1 {
		t.Fatalf("chunks = %+v", chunks)
	}
	heading := chunks[0].Heading
	if utf8.RuneCountInString(heading) > MaxHeadingRunes || strings.ContainsAny(heading, "\n\r") || strings.HasSuffix(heading, "#") {
		t.Fatalf("heading = %q, want one capped line without closing hashes", heading)
	}
}

func TestSplitChunksSkipsHeadingsWithoutBody(t *testing.T) {
	chunks, _ := SplitChunks("# One\n# Two\n\n# Three\ntext", 100)
	if len(chunks) != 1 || chunks[0].Heading != "Three" {
		t.Fatalf("chunks = %+v, want only the section with a body", chunks)
	}
}

func TestSplitChunksBoundsSizeAndOverlapsOnRuneBoundaries(t *testing.T) {
	cases := []struct {
		name string
		unit string
	}{
		{"ascii words", "lorem ipsum dolor "},
		{"multibyte without spaces", runeText(0x65E5, 0x672C, 0x8A9E, 0x3067, 0x3059)},
		{"four byte runes", runeText(0x1F600, 0x1F601, 0x1F602)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			text := strings.Repeat(tc.unit, 4000)
			chunks, exceeded := SplitChunks(text, 1000)
			if exceeded || len(chunks) < 3 {
				t.Fatalf("chunks = %d exceeded = %v, want several", len(chunks), exceeded)
			}
			for i, chunk := range chunks {
				if !utf8.ValidString(chunk.Content) {
					t.Fatalf("chunk %d is not valid UTF-8", i)
				}
				if count := utf8.RuneCountInString(chunk.Content); count > MaxChunkRunes || count == 0 {
					t.Fatalf("chunk %d has %d runes", i, count)
				}
				if i == 0 {
					continue
				}
				previous := []rune(chunks[i-1].Content)
				head := string([]rune(chunk.Content)[:ChunkOverlapRunes/2])
				tail := string(previous[max(len(previous)-ChunkOverlapRunes-chunkBreakWindow, 0):])
				if !strings.Contains(tail, strings.TrimSpace(head)) {
					t.Fatalf("chunk %d does not overlap the end of chunk %d", i, i-1)
				}
			}
		})
	}
}

func TestSplitChunksStopsAtTheLimit(t *testing.T) {
	var builder strings.Builder
	for range 50 {
		builder.WriteString("# Heading\nbody text\n")
	}
	chunks, exceeded := SplitChunks(builder.String(), 10)
	if !exceeded || len(chunks) != 10 {
		t.Fatalf("chunks = %d exceeded = %v, want 10 and exceeded", len(chunks), exceeded)
	}
	chunks, exceeded = SplitChunks(builder.String(), 50)
	if exceeded || len(chunks) != 50 {
		t.Fatalf("chunks = %d exceeded = %v, want exactly the limit to fit", len(chunks), exceeded)
	}
}

func TestSplitChunksDigestCoversHeadingAndContent(t *testing.T) {
	first, _ := SplitChunks("# A\nsame body", 10)
	again, _ := SplitChunks("# A\nsame body", 10)
	other, _ := SplitChunks("# B\nsame body", 10)
	if first[0].SHA256 != again[0].SHA256 {
		t.Error("digest is not deterministic")
	}
	if first[0].SHA256 == other[0].SHA256 {
		t.Error("digest ignores the heading")
	}
	if len(first[0].SHA256) != 64 {
		t.Errorf("digest length = %d, want 64", len(first[0].SHA256))
	}
}

func TestSplitChunksEmptyTextHasNoChunks(t *testing.T) {
	for _, text := range []string{"", "\n\n  \n", "# only heading"} {
		if chunks, exceeded := SplitChunks(text, 10); len(chunks) != 0 || exceeded {
			t.Errorf("SplitChunks(%q) = %+v, %v; want nothing", text, chunks, exceeded)
		}
	}
}
