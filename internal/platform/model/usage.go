package model

import (
	"iter"
	"sync"

	adkmodel "google.golang.org/adk/v2/model"
)

type UsageAccumulator struct {
	mu     sync.Mutex
	input  int64
	output int64
}

func NewUsageAccumulator() *UsageAccumulator {
	return &UsageAccumulator{}
}

func (a *UsageAccumulator) Add(u Usage) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.input += u.InputTokens
	a.output += u.OutputTokens
}

func (a *UsageAccumulator) AddResponse(r *adkmodel.LLMResponse) {
	a.Add(UsageFromResponse(r))
}

func (a *UsageAccumulator) Usage() Usage {
	a.mu.Lock()
	defer a.mu.Unlock()
	return Usage{InputTokens: a.input, OutputTokens: a.output}
}

func (a *UsageAccumulator) Reset() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.input, a.output = 0, 0
}

func (a *UsageAccumulator) Track(seq iter.Seq2[*adkmodel.LLMResponse, error]) iter.Seq2[*adkmodel.LLMResponse, error] {
	return func(yield func(*adkmodel.LLMResponse, error) bool) {
		for response, err := range seq {
			if response != nil {
				a.AddResponse(response)
			}
			if !yield(response, err) {
				return
			}
		}
	}
}

func UsageFromResponse(r *adkmodel.LLMResponse) Usage {
	if r == nil || r.UsageMetadata == nil {
		return Usage{}
	}
	return Usage{
		InputTokens:  int64(r.UsageMetadata.PromptTokenCount),
		OutputTokens: int64(r.UsageMetadata.CandidatesTokenCount),
	}
}
