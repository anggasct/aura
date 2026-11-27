package runtimeadk

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"google.golang.org/adk/v2/session"

	"github.com/anggasct/aura/internal/platform/durable"
	"github.com/anggasct/aura/internal/platform/toolcall"
	auratools "github.com/anggasct/aura/internal/platform/tools"
	"github.com/anggasct/aura/internal/platform/turn"
)

type ToolBudget struct {
	AnswerReserve            time.Duration
	MaxConsecutiveToolErrors int
	MaxToolErrorsPerTurn     int
}

func WithToolBudget(budget ToolBudget) ExecutorOption {
	return func(e *ADKExecutor) error {
		if budget.AnswerReserve < 0 {
			return invalidArgument("answer reserve must not be negative")
		}
		if budget.MaxConsecutiveToolErrors < 0 {
			return invalidArgument("max consecutive tool errors must not be negative")
		}
		if budget.MaxToolErrorsPerTurn < 0 {
			return invalidArgument("max tool errors per turn must not be negative")
		}
		if budget.MaxToolErrorsPerTurn > 0 && budget.MaxConsecutiveToolErrors > budget.MaxToolErrorsPerTurn {
			return invalidArgument("max tool errors per turn must be at least max consecutive tool errors")
		}
		e.toolBudget = budget
		return nil
	}
}

func WithClock(clock func() time.Time) ExecutorOption {
	return func(e *ADKExecutor) error {
		if clock == nil {
			return invalidArgument("clock must not be nil")
		}
		e.clock = clock
		return nil
	}
}

const (
	budgetActionName    = "budget"
	budgetReasonTime    = "time"
	budgetReasonStreak  = "streak"
	budgetReasonTotal   = "total"
	budgetErrorPrefix   = "tool_budget_exhausted: "
	budgetRefusalSuffix = ". Do not call any more tools in this turn. Reply to the user now, in their language: what you tried, why it did not succeed, and what they can do next."
)

type budgetDecisionRecord struct {
	Allowed      bool       `json:"allowed"`
	SoftDeadline *time.Time `json:"soft_deadline,omitempty"`
	Reason       string     `json:"reason,omitempty"`
}

type budgetDecision struct {
	allowed      bool
	softDeadline time.Time
	refusal      map[string]any
}

type turnToolBudgetKey struct{}

func withTurnToolBudget(ctx context.Context, budget *turnToolBudget) context.Context {
	return context.WithValue(ctx, turnToolBudgetKey{}, budget)
}

func turnToolBudgetFromContext(ctx context.Context) *turnToolBudget {
	budget, _ := ctx.Value(turnToolBudgetKey{}).(*turnToolBudget)
	return budget
}

type turnToolBudget struct {
	cfg             ToolBudget
	turnID          string
	hasHardDeadline bool
	hardDeadline    time.Time
	hasSoftDeadline bool
	softDeadline    time.Time
	now             func() time.Time
	logger          *slog.Logger

	mu              sync.Mutex
	streaks         map[string]int
	lastStreakTool  string
	totalErrors     int
	exhausted       bool
	reason          string
	streakTool      string
	streakCount     int
	totalCount      int
	sawRefusedBatch bool
	logOnce         sync.Once
}

func newTurnToolBudget(ctx context.Context, cfg ToolBudget, turnID string, now func() time.Time, logger *slog.Logger) *turnToolBudget {
	b := &turnToolBudget{
		cfg:     cfg,
		turnID:  turnID,
		now:     now,
		logger:  logger,
		streaks: map[string]int{},
	}
	if deadline, ok := ctx.Deadline(); ok {
		deadline = deadline.UTC()
		b.hasHardDeadline = true
		b.hardDeadline = deadline
		b.hasSoftDeadline = true
		b.softDeadline = computeSoftDeadline(now().UTC(), deadline, cfg.AnswerReserve)
	}
	return b
}

func computeSoftDeadline(start, deadline time.Time, reserve time.Duration) time.Time {
	if reserve <= 0 {
		return deadline
	}
	window := deadline.Sub(start)
	if window <= 0 {
		return deadline
	}
	effective := min(reserve, window/2)
	return deadline.Add(-effective)
}

func isBudgetExempt(toolName string) bool {
	return toolName == auratools.HandoffToolName
}

func isBudgetRefusal(response map[string]any) bool {
	msg, ok := response["error"].(string)
	return ok && strings.HasPrefix(msg, budgetErrorPrefix)
}

func decodeBudgetDecision(raw []byte) (budgetDecisionRecord, bool) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var record budgetDecisionRecord
	if err := decoder.Decode(&record); err != nil {
		return budgetDecisionRecord{}, false
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return budgetDecisionRecord{}, false
	}
	if record.Allowed {
		if record.Reason != "" {
			return budgetDecisionRecord{}, false
		}
		return record, true
	}
	if record.SoftDeadline != nil {
		return budgetDecisionRecord{}, false
	}
	switch record.Reason {
	case budgetReasonTime, budgetReasonStreak, budgetReasonTotal:
		return record, true
	default:
		return budgetDecisionRecord{}, false
	}
}

func (b *turnToolBudget) check(ctx context.Context, slot *toolcall.Slot, toolName string) (budgetDecision, error) {
	scope, scoped := durable.TurnScopeFrom(ctx)
	if scoped && slot == nil {
		return budgetDecision{}, invalidArgument(fmt.Sprintf("tool %q has no call key", toolName))
	}
	var record budgetDecisionRecord
	if scoped {
		raw, err := scope.Invocation().RunAction(ctx, slot.Action(budgetActionName), func(context.Context) ([]byte, error) {
			return json.Marshal(b.evaluate(toolName))
		})
		if err != nil {
			return budgetDecision{}, err
		}
		decoded, ok := decodeBudgetDecision(raw)
		if !ok {
			return budgetDecision{}, codedError(turn.ErrorCodeRuntimeInternal, "recorded tool budget decision is invalid", nil)
		}
		record = decoded
	} else {
		record = b.evaluate(toolName)
	}
	if record.Allowed {
		var sd time.Time
		if record.SoftDeadline != nil {
			sd = record.SoftDeadline.UTC()
		}
		return budgetDecision{allowed: true, softDeadline: sd}, nil
	}
	msg := b.applyRefusal(ctx, record.Reason, toolName)
	return budgetDecision{
		allowed: false,
		refusal: map[string]any{"error": msg},
	}, nil
}

func (b *turnToolBudget) evaluate(toolName string) budgetDecisionRecord {
	if isBudgetExempt(toolName) {
		record := budgetDecisionRecord{Allowed: true}
		if b.hasHardDeadline {
			hd := b.hardDeadline.UTC()
			record.SoftDeadline = &hd
		}
		return record
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.exhausted {
		return budgetDecisionRecord{Allowed: false, Reason: b.reason}
	}
	if b.hasSoftDeadline && b.cfg.AnswerReserve > 0 {
		now := b.now().UTC()
		if !now.Before(b.softDeadline) {
			b.latchLocked(budgetReasonTime, "", 0, 0)
			return budgetDecisionRecord{Allowed: false, Reason: budgetReasonTime}
		}
	}
	if b.cfg.MaxConsecutiveToolErrors > 0 {
		if name, count, hit := b.streakHitLocked(); hit {
			b.latchLocked(budgetReasonStreak, name, count, 0)
			return budgetDecisionRecord{Allowed: false, Reason: budgetReasonStreak}
		}
	}
	if b.cfg.MaxToolErrorsPerTurn > 0 && b.totalErrors >= b.cfg.MaxToolErrorsPerTurn {
		b.latchLocked(budgetReasonTotal, "", 0, b.totalErrors)
		return budgetDecisionRecord{Allowed: false, Reason: budgetReasonTotal}
	}
	record := budgetDecisionRecord{Allowed: true}
	if b.hasSoftDeadline {
		sd := b.softDeadline.UTC()
		record.SoftDeadline = &sd
	}
	return record
}

func (b *turnToolBudget) streakHitLocked() (toolName string, count int, hit bool) {
	if b.lastStreakTool != "" {
		if count := b.streaks[b.lastStreakTool]; count >= b.cfg.MaxConsecutiveToolErrors {
			return b.lastStreakTool, count, true
		}
	}
	names := slices.Sorted(maps.Keys(b.streaks))
	for _, name := range names {
		if count := b.streaks[name]; count >= b.cfg.MaxConsecutiveToolErrors {
			return name, count, true
		}
	}
	return "", 0, false
}

func (b *turnToolBudget) latchLocked(reason, streakTool string, streakCount, totalCount int) {
	if b.exhausted {
		return
	}
	b.exhausted = true
	b.reason = reason
	b.streakTool = streakTool
	b.streakCount = streakCount
	b.totalCount = totalCount
}

func (b *turnToolBudget) applyRefusal(ctx context.Context, reason, fallbackTool string) string {
	b.mu.Lock()
	if !b.exhausted {
		switch reason {
		case budgetReasonStreak:
			name, count, ok := b.streakHitLocked()
			if !ok {
				name = fallbackTool
				count = max(b.cfg.MaxConsecutiveToolErrors, 1)
			}
			b.latchLocked(budgetReasonStreak, name, count, 0)
		case budgetReasonTotal:
			count := b.totalErrors
			if count == 0 {
				count = max(b.cfg.MaxToolErrorsPerTurn, 1)
			}
			b.latchLocked(budgetReasonTotal, "", 0, count)
		default:
			b.latchLocked(budgetReasonTime, "", 0, 0)
		}
	}
	loggedReason := b.reason
	msg := b.refusalMessageLocked()
	b.mu.Unlock()
	b.logOnce.Do(func() {
		if b.logger != nil {
			b.logger.InfoContext(ctx, "tool budget exhausted", "component", "turn", "turn_id", b.turnID, "reason", loggedReason)
		}
	})
	return msg
}

func (b *turnToolBudget) reasonTextLocked() string {
	switch b.reason {
	case budgetReasonStreak:
		return "the tool " + b.streakTool + " did not succeed " + strconv.Itoa(b.streakCount) + " times in a row"
	case budgetReasonTotal:
		return strconv.Itoa(b.totalCount) + " tool calls did not succeed in this turn"
	default:
		return "time for this turn is almost up"
	}
}

func (b *turnToolBudget) refusalMessageLocked() string {
	return budgetErrorPrefix + b.reasonTextLocked() + budgetRefusalSuffix
}

func (b *turnToolBudget) exhaustedDetail() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.reasonTextLocked()
}

func (b *turnToolBudget) recordToolResponse(toolName string, isErr bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !isErr {
		b.streaks[toolName] = 0
		if b.lastStreakTool == toolName {
			b.lastStreakTool = ""
		}
		return
	}
	b.totalErrors++
	b.streaks[toolName]++
	if b.cfg.MaxConsecutiveToolErrors > 0 && b.streaks[toolName] >= b.cfg.MaxConsecutiveToolErrors && b.lastStreakTool == "" {
		b.lastStreakTool = toolName
	}
}

func (b *turnToolBudget) observeEvent(ev *session.Event, origin turn.Origin) error {
	if ev == nil || ev.Partial || ev.Content == nil {
		return nil
	}
	hasNonExemptCall := false
	refusedInEvent := false
	for _, part := range ev.Content.Parts {
		if part == nil {
			continue
		}
		if part.FunctionCall != nil && !isBudgetExempt(part.FunctionCall.Name) {
			hasNonExemptCall = true
		}
		if part.FunctionResponse != nil {
			if isBudgetRefusal(part.FunctionResponse.Response) {
				refusedInEvent = true
				continue
			}
			b.recordToolResponse(part.FunctionResponse.Name, turn.IsToolError(part.FunctionResponse.Response))
		}
	}
	if hasNonExemptCall {
		b.mu.Lock()
		refused := b.sawRefusedBatch
		b.mu.Unlock()
		if refused {
			return codedError(turn.ErrorCodeToolBudgetExhausted, b.exhaustedDetail(), nil)
		}
	}
	if refusedInEvent {
		b.mu.Lock()
		b.sawRefusedBatch = true
		b.mu.Unlock()
		if origin == turn.OriginWorkflow {
			return codedError(turn.ErrorCodeToolBudgetExhausted, b.exhaustedDetail(), nil)
		}
	}
	return nil
}
