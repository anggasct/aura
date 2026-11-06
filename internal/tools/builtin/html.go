package toolsbuiltin

import (
	"context"

	fetchTool "github.com/anggasct/aura/internal/tools/fetch"
)

func ReduceHTML(ctx context.Context, source []byte, maxBytes int64) (markdown string, truncated bool, err error) {
	return fetchTool.HTMLToMarkdown(ctx, source, maxBytes)
}
