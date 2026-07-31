package model

import (
	"iter"
	"sync"
	"testing"

	adkmodel "google.golang.org/adk/v2/model"
	"google.golang.org/genai"
)

func TestUsageAccumulator_ConcurrentAdd(t *testing.T) {
	acc := NewUsageAccumulator()
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				acc.Add(Usage{InputTokens: 1, OutputTokens: 2})
			}
		}()
	}
	wg.Wait()
	u := acc.Usage()
	if u.InputTokens != 800 || u.OutputTokens != 1600 {
		t.Errorf("usage = %+v, want input 800 output 1600", u)
	}
}

func TestUsageAccumulator_ZeroForMissingFields(t *testing.T) {
	acc := NewUsageAccumulator()
	acc.AddResponse(nil)
	acc.AddResponse(&adkmodel.LLMResponse{})
	if u := acc.Usage(); u.InputTokens != 0 || u.OutputTokens != 0 {
		t.Errorf("usage = %+v, want zero", u)
	}
	if u := UsageFromResponse(nil); u != (Usage{}) {
		t.Errorf("UsageFromResponse(nil) = %+v", u)
	}
}

func TestUsageAccumulator_AddResponseAndReset(t *testing.T) {
	acc := NewUsageAccumulator()
	acc.AddResponse(&adkmodel.LLMResponse{
		UsageMetadata: &genai.GenerateContentResponseUsageMetadata{
			PromptTokenCount:     10,
			CandidatesTokenCount: 5,
		},
	})
	u := acc.Usage()
	if u.InputTokens != 10 || u.OutputTokens != 5 {
		t.Errorf("usage = %+v, want 10/5", u)
	}
	acc.Reset()
	if u := acc.Usage(); u != (Usage{}) {
		t.Errorf("usage after reset = %+v, want zero", u)
	}
}

func TestUsageAccumulator_Track(t *testing.T) {
	seq := iter.Seq2[*adkmodel.LLMResponse, error](func(yield func(*adkmodel.LLMResponse, error) bool) {
		yield(&adkmodel.LLMResponse{UsageMetadata: &genai.GenerateContentResponseUsageMetadata{
			PromptTokenCount: 10, CandidatesTokenCount: 2,
		}}, nil)
		yield(&adkmodel.LLMResponse{UsageMetadata: &genai.GenerateContentResponseUsageMetadata{
			PromptTokenCount: 3, CandidatesTokenCount: 4,
		}}, nil)
	})

	acc := NewUsageAccumulator()
	for range acc.Track(seq) {
	}
	if got := acc.Usage(); got != (Usage{InputTokens: 13, OutputTokens: 6}) {
		t.Fatalf("usage = %+v, want 13/6", got)
	}
}
