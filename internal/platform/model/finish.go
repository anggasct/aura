package model

import "google.golang.org/genai"

func anthropicFinishReason(reason string) genai.FinishReason {
	switch reason {
	case "":
		return ""
	case "max_tokens":
		return genai.FinishReasonMaxTokens
	case "refusal":
		return genai.FinishReasonSafety
	default:
		return genai.FinishReasonStop
	}
}

func openaiFinishReason(reason string) genai.FinishReason {
	switch reason {
	case "", "null":
		return ""
	case "length":
		return genai.FinishReasonMaxTokens
	case "content_filter":
		return genai.FinishReasonSafety
	default:
		return genai.FinishReasonStop
	}
}

func responsesFinishReason(status, incompleteReason string) genai.FinishReason {
	switch {
	case status == "incomplete" && incompleteReason == "max_output_tokens":
		return genai.FinishReasonMaxTokens
	case status == "incomplete" && incompleteReason == "content_filter":
		return genai.FinishReasonSafety
	case status == "completed":
		return genai.FinishReasonStop
	default:
		return ""
	}
}

func geminiFinishReason(reason string) genai.FinishReason {
	if reason == "" || reason == string(genai.FinishReasonUnspecified) {
		return ""
	}
	return genai.FinishReason(reason)
}
