package app

import (
	"context"
	"iter"
	"sync"

	adkmodel "google.golang.org/adk/v2/model"
	"google.golang.org/genai"
)

type fakeContextLLM struct {
	mu    sync.Mutex
	texts []string
	err   error
}

func (f *fakeContextLLM) Name() string { return "fake-compression" }

func (f *fakeContextLLM) GenerateContent(_ context.Context, _ *adkmodel.LLMRequest, _ bool) iter.Seq2[*adkmodel.LLMResponse, error] {
	return func(yield func(*adkmodel.LLMResponse, error) bool) {
		if f.err != nil {
			yield(nil, f.err)
			return
		}
		f.mu.Lock()
		texts := f.texts
		f.mu.Unlock()
		for _, text := range texts {
			if !yield(&adkmodel.LLMResponse{Content: &genai.Content{Parts: []*genai.Part{{Text: text}}}, TurnComplete: true}, nil) {
				return
			}
		}
	}
}
