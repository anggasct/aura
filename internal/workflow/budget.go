package workflow

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	auraagent "github.com/anggasct/aura/internal/agent"
)

type Budget struct {
	MaxSteps  int
	MaxTokens int64
}

type MeteredAgentRunner interface {
	RunMetered(ctx context.Context, definition *auraagent.Definition, input *ExecutionContext) (json.RawMessage, int64, error)
}

type runBudget struct {
	mu     sync.Mutex
	limits Budget
	steps  int
	tokens int64
}

func (i *Interpreter) budgetFor(spec *Spec, inherited *runBudget) *runBudget {
	if inherited != nil {
		return inherited
	}
	limits := i.options.GeneratedBudget
	if spec.Source != SourceGenerated || (limits.MaxSteps <= 0 && limits.MaxTokens <= 0) {
		return nil
	}
	return &runBudget{limits: limits}
}

func (b *runBudget) admit() bool {
	if b == nil {
		return true
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.limits.MaxSteps > 0 && b.steps+1 > b.limits.MaxSteps {
		return false
	}
	b.steps++
	return true
}

func (b *runBudget) spend(tokens int64) bool {
	if b == nil {
		return true
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.tokens += tokens
	return b.limits.MaxTokens <= 0 || b.tokens <= b.limits.MaxTokens
}

func (e *stepExecution) exhaustBudget(ctx context.Context, step *StepSpec, attempt int, limit string) {
	detail := "generated run exceeded workflows.generated." + limit
	if err := e.terminal(ctx, step.ID, &stepUpdate{Status: StepFailed, Attempt: attempt, ErrorCode: string(ErrorCodeBudgetExhausted), Detail: detail, EndedAt: nowPtr()}); err != nil {
		e.failRun(ctx, step.ID, err)
		return
	}
	e.failRun(ctx, step.ID, codedError(ErrorCodeBudgetExhausted, detail))
}

func checkStepBudget(spec *Spec, limits Budget) error {
	if limits.MaxSteps > 0 && len(spec.Steps) > limits.MaxSteps {
		return codedError(ErrorCodeBudgetExhausted, fmt.Sprintf("spec has %d steps, more than workflows.generated.max_steps (%d)", len(spec.Steps), limits.MaxSteps))
	}
	return nil
}
