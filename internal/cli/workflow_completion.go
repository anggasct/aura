package cli

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"strings"
	"time"

	auraagent "github.com/anggasct/aura/internal/agent"
	"github.com/anggasct/aura/internal/channel/access"
	"github.com/anggasct/aura/internal/config"
	runtimechannelhost "github.com/anggasct/aura/internal/runtime/channelhost"
	"github.com/anggasct/aura/internal/store"
	"github.com/anggasct/aura/internal/workflow"
)

const (
	workflowCompletionInterval = 15 * time.Second
	workflowCompletionBatch    = 50
)

type replyTargeter interface {
	ReplyTarget(sessionID string) (access.HandoffTarget, bool)
}

func replyTargeters(ports []runtimechannelhost.ChannelPort) []replyTargeter {
	var targeters []replyTargeter
	for _, port := range ports {
		if targeter, ok := port.(replyTargeter); ok {
			targeters = append(targeters, targeter)
		}
	}
	return targeters
}

type workflowCompletionNotifier struct {
	cfg      *config.Config
	db       *sql.DB
	runs     *workflow.Store
	registry *auraagent.Registry
	deliver  func(ctx context.Context, account *config.DiscordAccount, channelID, text string) error
	logger   *slog.Logger
	now      func() time.Time
}

func newWorkflowCompletionNotifier(cfg *config.Config, db *sql.DB, registry *auraagent.Registry, logger *slog.Logger) *workflowCompletionNotifier {
	return &workflowCompletionNotifier{cfg: cfg, db: db, runs: workflow.NewStore(db), registry: registry, deliver: deliverText, logger: logger, now: time.Now}
}

func (n *workflowCompletionNotifier) run(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		n.sweep(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (n *workflowCompletionNotifier) sweep(ctx context.Context) {
	pending, err := n.runs.PendingCompletions(ctx, workflowCompletionBatch)
	if err != nil {
		n.logger.WarnContext(ctx, "workflow completion sweep failed", "component", "workflow", "error", err)
		return
	}
	for _, run := range pending {
		n.notify(ctx, run)
	}
}

func (n *workflowCompletionNotifier) notify(ctx context.Context, run *workflow.RunSummary) {
	claimed, err := n.runs.ClaimCompletion(ctx, run.ID, n.now())
	if err != nil || !claimed {
		return
	}
	definition, _ := n.registry.Lookup(run.Origin.AgentID)
	attempts, err := n.runs.CompletionAttempts(ctx, run.ID)
	if err == nil && attempts == 0 {
		author := definition.Name
		if author == "" {
			author = handoffReplyAuthorFallback
		}
		if err := appendSessionText(ctx, n.db, run.Origin.SessionID, "workflow-"+run.ID, author,
			fmt.Sprintf("workflow %s (run %s) %s", run.DefinitionID, run.ID, run.Status), n.now().UTC()); err != nil {
			n.logger.WarnContext(ctx, "workflow completion event failed", "component", "workflow", "run_id", run.ID, "error", err)
		}
	}
	if run.Origin.ReplyChannelID == "" {
		return
	}
	account, ok := discordAccount(n.cfg, run.Origin.ReplyAccount)
	if ok {
		err = n.deliver(ctx, account, run.Origin.ReplyChannelID, completionMessage(&definition, run))
	} else {
		err = fmt.Errorf("discord account %q is not configured", run.Origin.ReplyAccount)
	}
	if err == nil {
		return
	}
	exhausted, releaseErr := n.runs.ReleaseCompletion(ctx, run.ID)
	n.logger.WarnContext(ctx, "workflow completion delivery failed", "component", "workflow", "run_id", run.ID, "error", err,
		"gave_up", exhausted, "release_error", releaseErr)
}

func completionMessage(definition *auraagent.Definition, run *workflow.RunSummary) string {
	key := config.MessageWorkflowSucceeded
	switch run.Status {
	case workflow.RunFailed:
		key = config.MessageWorkflowFailed
	case workflow.RunCancelled:
		key = config.MessageWorkflowCancelled
	}
	return strings.ReplaceAll(definition.Message(key), config.WorkflowGoalPlaceholder, run.Goal)
}

func appendSessionText(ctx context.Context, db *sql.DB, sessionID, id, author, text string, at time.Time) error {
	event, err := store.RuntimeEventFromADK(sessionID, id, sessionEventWithText(id, author, text, at))
	if err != nil {
		return err
	}
	_, err = store.NewEventStore(db).AppendSequenced(ctx, sessionID, &event)
	return err
}
