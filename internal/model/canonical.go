package model

import (
	adkmodel "google.golang.org/adk/v2/model"
	"google.golang.org/genai"
)

// CloneCanonicalRequest produces a cleanly isolated copy of an LLM request for
// dispatch to a candidate model. It preserves canonical messages, parts, tool
// declarations, and configurations while ensuring provider-specific continuation
// tokens, vendor-specific hidden fields, and response buffers do not leak across
// provider boundaries.
func CloneCanonicalRequest(req *adkmodel.LLMRequest) *adkmodel.LLMRequest {
	if req == nil {
		return nil
	}
	clone := &adkmodel.LLMRequest{
		Model: req.Model,
	}

	if len(req.Contents) > 0 {
		clone.Contents = make([]*genai.Content, 0, len(req.Contents))
		for _, c := range req.Contents {
			if c == nil {
				continue
			}
			clone.Contents = append(clone.Contents, cloneContent(c))
		}
	}

	if req.Config != nil {
		clone.Config = cloneConfig(req.Config)
	}

	if len(req.Tools) > 0 {
		clone.Tools = make(map[string]any, len(req.Tools))
		for k, v := range req.Tools {
			clone.Tools[k] = v
		}
	}

	return clone
}

func cloneContent(c *genai.Content) *genai.Content {
	if c == nil {
		return nil
	}
	res := &genai.Content{
		Role: c.Role,
	}
	if len(c.Parts) > 0 {
		res.Parts = make([]*genai.Part, 0, len(c.Parts))
		for _, p := range c.Parts {
			if p == nil {
				continue
			}
			res.Parts = append(res.Parts, clonePart(p))
		}
	}
	return res
}

func clonePart(p *genai.Part) *genai.Part {
	if p == nil {
		return nil
	}
	cloned := &genai.Part{
		Text: p.Text,
	}
	if p.InlineData != nil {
		blob := &genai.Blob{
			MIMEType: p.InlineData.MIMEType,
		}
		if len(p.InlineData.Data) > 0 {
			blob.Data = make([]byte, len(p.InlineData.Data))
			copy(blob.Data, p.InlineData.Data)
		}
		cloned.InlineData = blob
	}
	if p.FunctionCall != nil {
		fc := &genai.FunctionCall{
			ID:   p.FunctionCall.ID,
			Name: p.FunctionCall.Name,
		}
		if len(p.FunctionCall.Args) > 0 {
			fc.Args = make(map[string]any, len(p.FunctionCall.Args))
			for k, v := range p.FunctionCall.Args {
				fc.Args[k] = v
			}
		}
		cloned.FunctionCall = fc
	}
	if p.FunctionResponse != nil {
		fr := &genai.FunctionResponse{
			ID:   p.FunctionResponse.ID,
			Name: p.FunctionResponse.Name,
		}
		if len(p.FunctionResponse.Response) > 0 {
			fr.Response = make(map[string]any, len(p.FunctionResponse.Response))
			for k, v := range p.FunctionResponse.Response {
				fr.Response[k] = v
			}
		}
		cloned.FunctionResponse = fr
	}
	if p.FileData != nil {
		cloned.FileData = &genai.FileData{
			FileURI:  p.FileData.FileURI,
			MIMEType: p.FileData.MIMEType,
		}
	}
	return cloned
}

func cloneConfig(cfg *genai.GenerateContentConfig) *genai.GenerateContentConfig {
	if cfg == nil {
		return nil
	}
	cloned := &genai.GenerateContentConfig{
		Temperature:        cfg.Temperature,
		TopP:               cfg.TopP,
		TopK:               cfg.TopK,
		CandidateCount:     cfg.CandidateCount,
		MaxOutputTokens:    cfg.MaxOutputTokens,
		ResponseLogprobs:   cfg.ResponseLogprobs,
		Logprobs:           cfg.Logprobs,
		PresencePenalty:    cfg.PresencePenalty,
		FrequencyPenalty:   cfg.FrequencyPenalty,
		Seed:               cfg.Seed,
		ResponseMIMEType:   cfg.ResponseMIMEType,
		ResponseSchema:     cfg.ResponseSchema,
		ResponseJsonSchema: cfg.ResponseJsonSchema,
		CachedContent:      cfg.CachedContent,
	}
	if cfg.SystemInstruction != nil {
		cloned.SystemInstruction = cloneContent(cfg.SystemInstruction)
	}
	if len(cfg.StopSequences) > 0 {
		cloned.StopSequences = make([]string, len(cfg.StopSequences))
		copy(cloned.StopSequences, cfg.StopSequences)
	}
	if len(cfg.Tools) > 0 {
		cloned.Tools = make([]*genai.Tool, len(cfg.Tools))
		copy(cloned.Tools, cfg.Tools)
	}
	if len(cfg.Labels) > 0 {
		cloned.Labels = make(map[string]string, len(cfg.Labels))
		for k, v := range cfg.Labels {
			cloned.Labels[k] = v
		}
	}
	return cloned
}
