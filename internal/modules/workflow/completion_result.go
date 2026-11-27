package workflow

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/anggasct/aura/internal/platform/principal"
)

const (
	maxCompletionResultRunes = 3500
	taintedResultMarker      = "(from web content, not verified)"
	truncatedResultHint      = "… (truncated; aura workflow inspect %s)"
)

func (n *CompletionNotifier) completionText(ctx context.Context, message string, run *RunSummary) string {
	if run.Status != RunSucceeded {
		return message
	}
	result, err := n.completionResult(ctx, run)
	if err != nil {
		n.logger.WarnContext(ctx, "workflow completion result unavailable", "component", "workflow", "run_id", run.ID, "error", err)
		return message
	}
	if result == "" {
		return message
	}
	return message + "\n\n" + result
}

func (n *CompletionNotifier) completionResult(ctx context.Context, run *RunSummary) (string, error) {
	spec, err := n.runs.Definition(ctx, run.DefinitionID, run.DefinitionVersion)
	if err != nil {
		return "", err
	}
	steps, err := n.runs.Steps(ctx, run.ID)
	if err != nil {
		return "", err
	}
	step, text := lastProducingStep(spec, steps)
	if step == nil {
		return "", nil
	}
	if step.Private && run.Origin.PrincipalClass != string(principal.ClassOwner) {
		return "", nil
	}
	result := boundResult(text, run.ID)
	if step.Tainted {
		result = taintedResultMarker + "\n" + result
	}
	return result, nil
}

func lastProducingStep(spec *Spec, steps []*StepRun) (step *StepRun, text string) {
	kinds := make(map[string]Kind, len(spec.Steps))
	positions := make(map[string]int, len(spec.Steps))
	for index := range spec.Steps {
		kinds[spec.Steps[index].ID] = spec.Steps[index].Executor.Kind
		positions[spec.Steps[index].ID] = index
	}
	var lastEnded time.Time
	for _, candidate := range steps {
		if candidate.Status != StepSucceeded || candidate.EndedAt == nil {
			continue
		}
		candidateText := stepResultText(kinds[candidate.StepID], candidate.Output)
		if candidateText == "" {
			continue
		}
		later := step == nil || candidate.EndedAt.After(lastEnded) ||
			(candidate.EndedAt.Equal(lastEnded) && positions[candidate.StepID] > positions[step.StepID])
		if later {
			step, text, lastEnded = candidate, candidateText, *candidate.EndedAt
		}
	}
	return step, text
}

func stepResultText(kind Kind, output []byte) string {
	switch kind {
	case KindAgent:
		var envelope agentOutput
		if err := json.Unmarshal(output, &envelope); err != nil {
			return ""
		}
		if len(envelope.JSON) > 0 {
			return compactJSON(envelope.JSON)
		}
		return strings.TrimSpace(envelope.Text)
	case KindTool:
		return compactJSON(output)
	default:
		return ""
	}
}

func compactJSON(raw []byte) string {
	var compacted bytes.Buffer
	if err := json.Compact(&compacted, raw); err != nil {
		return ""
	}
	return compacted.String()
}

func boundResult(text, runID string) string {
	runes := []rune(text)
	if len(runes) <= maxCompletionResultRunes {
		return text
	}
	return string(runes[:maxCompletionResultRunes]) + fmt.Sprintf(truncatedResultHint, runID)
}
