package operationtest

import (
	"context"
	"fmt"
	"iter"
	"slices"
	"sync"

	adkmodel "google.golang.org/adk/v2/model"
	"google.golang.org/genai"
)

type ModelCall struct {
	Tools   []string
	System  string
	Answers []string
}

type ScriptedModel struct {
	mu     sync.Mutex
	script [][]*genai.Part
	calls  []ModelCall
	next   int
}

func (m *ScriptedModel) Name() string { return "operation-model" }

func (m *ScriptedModel) Play(script ...[]*genai.Part) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.script, m.next = script, 0
}

func (m *ScriptedModel) Calls() []ModelCall {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.calls)
}

func CallOf(name string, args map[string]any) []*genai.Part {
	return []*genai.Part{{FunctionCall: &genai.FunctionCall{Name: name, Args: args}}}
}

func (m *ScriptedModel) GenerateContent(_ context.Context, req *adkmodel.LLMRequest, _ bool) iter.Seq2[*adkmodel.LLMResponse, error] {
	return func(yield func(*adkmodel.LLMResponse, error) bool) {
		var seen ModelCall
		if req.Config != nil {
			for _, declared := range req.Config.Tools {
				for _, function := range declared.FunctionDeclarations {
					seen.Tools = append(seen.Tools, function.Name)
				}
			}
			if req.Config.SystemInstruction != nil {
				for _, part := range req.Config.SystemInstruction.Parts {
					seen.System += part.Text
				}
			}
		}
		for _, content := range req.Contents {
			if content == nil {
				continue
			}
			for _, part := range content.Parts {
				if part != nil && part.FunctionResponse != nil {
					seen.Answers = append(seen.Answers, fmt.Sprint(part.FunctionResponse.Response))
				}
			}
		}
		m.mu.Lock()
		m.calls = append(m.calls, seen)
		parts := []*genai.Part{{Text: "done"}}
		if m.next < len(m.script) {
			parts = m.script[m.next]
		}
		m.next++
		m.mu.Unlock()
		yield(&adkmodel.LLMResponse{
			Content:       &genai.Content{Role: genai.RoleModel, Parts: parts},
			TurnComplete:  true,
			UsageMetadata: &genai.GenerateContentResponseUsageMetadata{PromptTokenCount: 1, CandidatesTokenCount: 1, TotalTokenCount: 2},
		}, nil)
	}
}
