package workflow

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	auraagent "github.com/anggasct/aura/internal/platform/agent"
)

const (
	defaultCompletionAuthor   = "aura"
	DefaultCompletionInterval = 15 * time.Second
	defaultCompletionBatch    = 50
)

type SessionTextAppender interface {
	AppendSessionText(ctx context.Context, sessionID, id, author, text string, at time.Time) error
}

type ReplyDeliverer interface {
	DeliverReply(ctx context.Context, target *ReplyTarget, text string) error
}

type CompletionNotifierDeps struct {
	Runs     *Store
	Registry *auraagent.Registry
	Appender SessionTextAppender
	Deliver  ReplyDeliverer
	Logger   *slog.Logger
	Now      func() time.Time
	Batch    int
}

type CompletionNotifier struct {
	runs     *Store
	registry *auraagent.Registry
	appender SessionTextAppender
	deliver  ReplyDeliverer
	logger   *slog.Logger
	now      func() time.Time
	batch    int
}

func NewCompletionNotifier(deps *CompletionNotifierDeps) (*CompletionNotifier, error) {
	if deps == nil || deps.Runs == nil || deps.Registry == nil || deps.Appender == nil || deps.Deliver == nil {
		return nil, codedError(ErrorCodeInvalidArgument, "workflow completion notifier requires a store, registry, session appender, and deliverer")
	}
	logger := deps.Logger
	if logger == nil {
		logger = slog.Default()
	}
	now := deps.Now
	if now == nil {
		now = time.Now
	}
	batch := deps.Batch
	if batch <= 0 {
		batch = defaultCompletionBatch
	}
	return &CompletionNotifier{
		runs:     deps.Runs,
		registry: deps.Registry,
		appender: deps.Appender,
		deliver:  deps.Deliver,
		logger:   logger,
		now:      now,
		batch:    batch,
	}, nil
}

func (n *CompletionNotifier) Run(ctx context.Context, interval time.Duration) {
	if n == nil {
		return
	}
	if interval <= 0 {
		interval = DefaultCompletionInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		n.Sweep(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (n *CompletionNotifier) Sweep(ctx context.Context) {
	pending, err := n.runs.PendingCompletions(ctx, n.batch)
	if err != nil {
		n.logger.WarnContext(ctx, "workflow completion sweep failed", "component", "workflow", "error", err)
		return
	}
	for _, run := range pending {
		n.notify(ctx, run)
	}
}

func (n *CompletionNotifier) notify(ctx context.Context, run *RunSummary) {
	claimed, err := n.runs.ClaimCompletion(ctx, run.ID, n.now())
	if err != nil || !claimed {
		return
	}
	definition, _ := n.registry.Lookup(run.Origin.AgentID)
	attempts, err := n.runs.CompletionAttempts(ctx, run.ID)
	if err == nil && attempts == 0 {
		author := definition.Name
		if author == "" {
			author = defaultCompletionAuthor
		}
		if err := n.appender.AppendSessionText(ctx, run.Origin.SessionID, "workflow-"+run.ID, author,
			fmt.Sprintf("workflow %s (run %s) %s", run.DefinitionID, run.ID, run.Status), n.now().UTC()); err != nil {
			n.logger.WarnContext(ctx, "workflow completion event failed", "component", "workflow", "run_id", run.ID, "error", err)
		}
	}
	if run.Origin.Reply.IsZero() {
		return
	}
	if err := n.deliver.DeliverReply(ctx, &run.Origin.Reply, completionMessage(&definition, run)); err != nil {
		exhausted, releaseErr := n.runs.ReleaseCompletion(ctx, run.ID)
		n.logger.WarnContext(ctx, "workflow completion delivery failed", "component", "workflow", "run_id", run.ID, "error", err,
			"gave_up", exhausted, "release_error", releaseErr)
	}
}

func completionMessage(definition *auraagent.Definition, run *RunSummary) string {
	key := auraagent.MessageWorkflowSucceeded
	switch run.Status {
	case RunFailed:
		key = auraagent.MessageWorkflowFailed
	case RunCancelled:
		key = auraagent.MessageWorkflowCancelled
	}
	return strings.ReplaceAll(definition.Message(key), auraagent.WorkflowGoalPlaceholder, run.Goal)
}
