package workflow

import (
	"slices"
	"sync"
)

type reportedLabels struct {
	tainted bool
	private bool
}

type labelLedger struct {
	mu      sync.Mutex
	tainted map[string]bool
	private map[string]bool
	steered map[string]bool
}

func newLabelLedger() *labelLedger {
	return &labelLedger{tainted: map[string]bool{}, private: map[string]bool{}, steered: map[string]bool{}}
}

func (l *labelLedger) steer(step *StepSpec, parsed *condition) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	steered := slices.ContainsFunc(step.DependsOn, func(dependency string) bool { return l.steered[dependency] })
	if !steered && step.Executor.Kind == KindAgent {
		steered = slices.ContainsFunc(step.DependsOn, func(dependency string) bool { return l.tainted[dependency] })
	}
	if !steered && parsed != nil {
		steered = slices.ContainsFunc(parsed.references(), func(ref *refOperand) bool {
			if ref.field == "status" {
				return l.steered[ref.stepID]
			}
			return l.tainted[ref.stepID]
		})
	}
	l.steered[step.ID] = steered
	return steered
}

func (l *labelLedger) isSteered(stepID string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.steered[stepID]
}

func (l *labelLedger) outputOf(step *StepSpec, runTainted bool, reported reportedLabels) (tainted, private bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	private = reported.private || (&stepLabels{private: l.private}).isPrivate(step)
	tainted = reported.tainted
	if step.Executor.Kind == KindAgent {
		tainted = tainted || runTainted || l.steered[step.ID]
	}
	return tainted, private
}

func (l *labelLedger) record(stepID string, tainted, private bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.tainted[stepID] = tainted
	l.private[stepID] = private
}
