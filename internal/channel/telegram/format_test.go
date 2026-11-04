package telegram

import (
	"strings"
	"testing"
)

func TestRenderHTMLConvertsTheSafeSubsetAndEscapesTheRest(t *testing.T) {
	cases := map[string]struct {
		in   string
		want string
	}{
		"plain":                   {in: "hello world", want: "hello world"},
		"script injection":        {in: "<script>alert(1)</script>", want: "&lt;script&gt;alert(1)&lt;/script&gt;"},
		"raw tag":                 {in: `<b onclick="x">hi</b>`, want: `&lt;b onclick="x"&gt;hi&lt;/b&gt;`},
		"existing entity":         {in: "fish &amp; chips & co", want: "fish &amp;amp; chips &amp; co"},
		"numeric entity":          {in: "&#60;b&#62;", want: "&amp;#60;b&amp;#62;"},
		"bold":                    {in: "**bold** text", want: "<b>bold</b> text"},
		"underscore bold":         {in: "__bold__", want: "<b>bold</b>"},
		"italic star":             {in: "an *italic* word", want: "an <i>italic</i> word"},
		"italic underscore":       {in: "an _italic_ word", want: "an <i>italic</i> word"},
		"strikethrough":           {in: "~~gone~~", want: "<s>gone</s>"},
		"single tilde":            {in: "~approx~", want: "~approx~"},
		"nested":                  {in: "**bold *and italic***", want: "<b>bold <i>and italic</i></b>"},
		"unclosed bold":           {in: "**never closed", want: "**never closed"},
		"unclosed italic":         {in: "*never closed", want: "*never closed"},
		"spaced stars":            {in: "2 * 3 * 4", want: "2 * 3 * 4"},
		"snake case":              {in: "use snake_case_name here", want: "use snake_case_name here"},
		"escaped star":            {in: `\*not italic\*`, want: "*not italic*"},
		"backslash kept":          {in: `C:\Users\me`, want: `C:\Users\me`},
		"inline code":             {in: "run `rm -rf <dir>` now", want: "run <code>rm -rf &lt;dir&gt;</code> now"},
		"code ignores markup":     {in: "`**not bold**`", want: "<code>**not bold**</code>"},
		"double backtick code":    {in: "``a ` b``", want: "<code>a ` b</code>"},
		"unclosed code":           {in: "a `b", want: "a `b"},
		"https link":              {in: "[docs](https://example.com/a?b=1&c=2)", want: `<a href="https://example.com/a?b=1&amp;c=2">docs</a>`},
		"http link":               {in: "[x](http://example.com)", want: `<a href="http://example.com">x</a>`},
		"uppercase scheme":        {in: "[x](HTTPS://example.com)", want: `<a href="HTTPS://example.com">x</a>`},
		"formatted link text":     {in: "[**x**](https://e.com)", want: `<a href="https://e.com"><b>x</b></a>`},
		"javascript link":         {in: "[x](javascript:alert(1))", want: "[x](javascript:alert(1))"},
		"data link":               {in: "[x](data:text/html,<b>)", want: "[x](data:text/html,&lt;b&gt;)"},
		"relative link":           {in: "[x](/etc/passwd)", want: "[x](/etc/passwd)"},
		"quote in link":           {in: `[x](https://e.com/"onmouseover=")`, want: `[x](https://e.com/"onmouseover=")`},
		"credentials in link":     {in: "[x](https://user:pw@e.com)", want: "[x](https://user:pw@e.com)"},
		"nested link text":        {in: "[a [b] c](https://e.com)", want: "[a [b] c](https://e.com)"},
		"fenced code":             {in: "```go\nif a < b {}\n```", want: `<pre><code class="language-go">if a &lt; b {}</code></pre>`},
		"fence without language":  {in: "```\nx & y\n```\nafter", want: "<pre>x &amp; y</pre>\nafter"},
		"fence keeps markup":      {in: "```\n**a** [b](https://e.com)\n```", want: "<pre>**a** [b](https://e.com)</pre>"},
		"fence language injected": {in: "```go\"><script>\nx\n```", want: "<pre>x</pre>"},
		"unclosed fence":          {in: "text\n```py\nprint(1)", want: "text\n" + `<pre><code class="language-py">print(1)</code></pre>`},
		"inline only per line":    {in: "**a\nb**", want: "**a\nb**"},
		"heading stays text":      {in: "# Title", want: "# Title"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got := renderHTML(tc.in)
			if got != tc.want {
				t.Fatalf("renderHTML(%q) = %q, want %q", tc.in, got, tc.want)
			}
			if _, err := visibleHTMLText(got); err != nil {
				t.Fatalf("renderHTML(%q) is not well-formed Telegram HTML: %v", tc.in, err)
			}
		})
	}
}

func FuzzRenderHTMLIsWellFormedAndNeverLonger(f *testing.F) {
	for _, seed := range []string{
		"**a *b* c**", "<a href=x>", "[t](https://e.com)", "```go\nx\n```", "`a`b`", "\\*x\\*", "~~a~~b~~", "__a_b__", "&lt;", "*a **b** c*", "[](https://e.com)", "\x00", "\xff**a**",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, source string) {
		rendered := renderHTML(source)
		visible, err := visibleHTMLText(rendered)
		if err != nil {
			t.Fatalf("renderHTML(%q) = %q is not well-formed: %v", source, rendered, err)
		}
		if utf16Len(visible) > utf16Len(source) {
			t.Fatalf("visible text of %q grew from %d to %d units", source, utf16Len(source), utf16Len(visible))
		}
	})
}

func TestSplitMessageKeepsFencedCodeBalancedPerChunk(t *testing.T) {
	cases := map[string]string{
		"long block":          "intro\n```go\n" + strings.Repeat("line <of> code & more\n", 600) + "```\noutro",
		"long line in block":  "```\n" + strings.Repeat("z", 3*messageLimit) + "\n```",
		"block at boundary":   strings.Repeat("x", messageLimit-6) + "\n```py\n" + strings.Repeat("print(1)\n", 50) + "```",
		"two blocks":          "```a\n" + strings.Repeat("1\n", 3000) + "```\nmid\n```b\n" + strings.Repeat("2\n", 3000) + "```",
		"surrogates in block": "```\n" + strings.Repeat("😀", messageLimit) + "\n```",
	}
	for name, text := range cases {
		t.Run(name, func(t *testing.T) {
			chunks := splitMessage(text)
			if len(chunks) < 2 {
				t.Fatalf("chunks = %d, want a split", len(chunks))
			}
			for i, chunk := range chunks {
				if units := utf16Len(chunk); units > messageLimit {
					t.Fatalf("chunk %d has %d UTF-16 units", i, units)
				}
				if fences := countFenceLines(chunk); fences%2 != 0 {
					t.Fatalf("chunk %d has %d fence lines, want balanced", i, fences)
				}
				visible, err := visibleHTMLText(renderHTML(chunk))
				if err != nil {
					t.Fatalf("chunk %d renders malformed HTML: %v", i, err)
				}
				if units := utf16Len(visible); units > messageLimit {
					t.Fatalf("chunk %d renders %d visible units", i, units)
				}
			}
			if again := splitMessage(text); strings.Join(again, "\x00") != strings.Join(chunks, "\x00") {
				t.Fatal("split is not deterministic")
			}
			if contentWithoutFences(chunks...) != contentWithoutFences(text) {
				t.Fatal("chunks do not preserve the input content in order")
			}
		})
	}
}

func TestSplitMessageReopensTheFenceLanguage(t *testing.T) {
	chunks := splitMessage("```go\n" + strings.Repeat("x := 1\n", 1000) + "```")
	if len(chunks) != 2 {
		t.Fatalf("chunks = %d, want 2", len(chunks))
	}
	if !strings.HasSuffix(chunks[0], "\n```") {
		t.Errorf("first chunk does not close the fence: %q", chunks[0][len(chunks[0])-20:])
	}
	if !strings.HasPrefix(chunks[1], "```go\n") {
		t.Errorf("second chunk does not reopen the fence with its language: %.20q", chunks[1])
	}
}

func TestSplitMessageLeavesAnUnclosedFinalFenceAlone(t *testing.T) {
	text := "```\n" + strings.Repeat("y\n", 3000)
	chunks := splitMessage(text)
	last := chunks[len(chunks)-1]
	if strings.HasSuffix(strings.TrimSpace(last), "```") {
		t.Fatalf("splitter closed a fence the source left open: %q", last[len(last)-10:])
	}
	if !strings.Contains(renderHTML(last), "<pre>") {
		t.Fatal("unclosed final fence is not rendered as code")
	}
}

func countFenceLines(chunk string) int {
	n := 0
	for _, line := range strings.SplitAfter(chunk, "\n") {
		if _, ok := parseFence(line); ok {
			n++
		}
	}
	return n
}

func contentWithoutFences(texts ...string) string {
	var out strings.Builder
	for _, text := range texts {
		for _, line := range strings.SplitAfter(text, "\n") {
			if _, ok := parseFence(line); ok {
				continue
			}
			out.WriteString(strings.TrimSuffix(line, "\n"))
		}
	}
	return out.String()
}
