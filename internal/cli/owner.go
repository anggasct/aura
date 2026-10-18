package cli

import (
	"context"
	"log/slog"

	"github.com/anggasct/aura/internal/config"
	"github.com/anggasct/aura/internal/runtime/adk"
)

const ownerContextWarnBytes = 4096

func ownerContextOption(ctx context.Context, owner *config.Owner, logger *slog.Logger) runtimeadk.ExecutorOption {
	if logger == nil {
		logger = slog.Default()
	}
	if owner == nil {
		return runtimeadk.WithOwnerContext("")
	}
	block := runtimeadk.RenderOwnerContext(&runtimeadk.OwnerContext{
		Name:        owner.Name,
		Language:    owner.Language,
		Timezone:    owner.Timezone,
		Preferences: owner.Preferences,
	})
	if block == "" {
		return runtimeadk.WithOwnerContext("")
	}
	logger.InfoContext(ctx, "owner context attached",
		"component", "owner",
		"digest", runtimeadk.OwnerContextDigest(block),
		"bytes", len(block),
	)
	if len(block) > ownerContextWarnBytes {
		logger.WarnContext(ctx, "owner context exceeds the size guideline",
			"component", "owner",
			"bytes", len(block),
			"limit", ownerContextWarnBytes,
		)
	}
	return runtimeadk.WithOwnerContext(block)
}
