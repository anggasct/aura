package app

import (
	"context"
	"encoding/json"
	"fmt"
	"iter"
	"slices"
	"strings"
	"sync"

	adkmodel "google.golang.org/adk/v2/model"
	"google.golang.org/genai"
)

const agentScriptModelName = "agent-script-model"

type agentScriptStep struct {
	text string
	call *genai.FunctionCall
}

type agentModelRequest struct {
	Text    string   `json:"text"`
	Tools   []string `json:"tools"`
	Results []string `json:"results"`
}

type agentScriptModel struct {
	steps  []agentScriptStep
	arrive func(request *agentModelRequest) (position int, err error)
}

func (m *agentScriptModel) Name() string { return agentScriptModelName }

func (m *agentScriptModel) GenerateContent(_ context.Context, req *adkmodel.LLMRequest, _ bool) iter.Seq2[*adkmodel.LLMResponse, error] {
	return func(yield func(*adkmodel.LLMResponse, error) bool) {
		position, err := m.arrive(describeAgentModelRequest(req))
		if err != nil {
			yield(nil, err)
			return
		}
		part := &genai.Part{Text: "done"}
		if position < len(m.steps) {
			step := m.steps[position]
			part = &genai.Part{Text: step.text}
			if step.call != nil {
				call := *step.call
				call.ID = fmt.Sprintf("call-%d", position+1)
				part = &genai.Part{FunctionCall: &call}
			}
		}
		yield(&adkmodel.LLMResponse{
			Content:       &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{part}},
			TurnComplete:  true,
			UsageMetadata: &genai.GenerateContentResponseUsageMetadata{PromptTokenCount: 1, CandidatesTokenCount: 1, TotalTokenCount: 2},
		}, nil)
	}
}

func describeAgentModelRequest(req *adkmodel.LLMRequest) *agentModelRequest {
	described := &agentModelRequest{Tools: []string{}, Results: []string{}}
	if req == nil {
		return described
	}
	var text strings.Builder
	for _, content := range req.Contents {
		if content == nil {
			continue
		}
		for _, part := range content.Parts {
			if part == nil {
				continue
			}
			text.WriteString(part.Text)
			text.WriteString("\n")
			if part.FunctionResponse != nil {
				encoded, err := json.Marshal(part.FunctionResponse.Response)
				if err != nil {
					encoded = []byte(err.Error())
				}
				described.Results = append(described.Results, part.FunctionResponse.Name+" "+string(encoded))
			}
		}
	}
	described.Text = text.String()
	if req.Config != nil {
		for _, declared := range req.Config.Tools {
			for _, declaration := range declared.FunctionDeclarations {
				described.Tools = append(described.Tools, declaration.Name)
			}
		}
	}
	slices.Sort(described.Tools)
	return described
}

type agentModelLog struct {
	mu       sync.Mutex
	requests []*agentModelRequest
}

func (l *agentModelLog) arrive(request *agentModelRequest) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.requests = append(l.requests, request)
	return len(l.requests) - 1, nil
}

func (l *agentModelLog) seen() []*agentModelRequest {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.requests)
}
