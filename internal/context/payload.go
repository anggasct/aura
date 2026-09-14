package context

import (
	"encoding/json"
	"strings"
	"time"
)

type summarySource struct {
	StartSequence uint64   `json:"start_sequence"`
	EndSequence   uint64   `json:"end_sequence"`
	EventIDs      []string `json:"event_ids"`
	Digest        string   `json:"digest"`
}

type summaryProducer struct {
	Provider      string `json:"provider"`
	Model         string `json:"model"`
	ModelVersion  string `json:"model_version"`
	PromptVersion string `json:"prompt_version"`
	PromptDigest  string `json:"prompt_digest"`
}

type summaryTokens struct {
	Source     int    `json:"source"`
	Summary    int    `json:"summary"`
	Accounting string `json:"accounting"`
}

type summaryPayload struct {
	Source      summarySource   `json:"source"`
	Producer    summaryProducer `json:"producer"`
	Tokens      summaryTokens   `json:"tokens"`
	Trust       string          `json:"trust"`
	Summary     SummaryContent  `json:"summary"`
	GeneratedAt string          `json:"generated_at"`
	Kind        string          `json:"kind"`
	Schema      int             `json:"schema_version"`
}

func (s *Summary) payload() ([]byte, error) {
	if s == nil {
		return nil, errNilArgument("summary")
	}
	encoded, err := json.Marshal(summaryPayload{
		Source: summarySource{
			StartSequence: s.StartSequence,
			EndSequence:   s.EndSequence,
			EventIDs:      s.EventIDs,
			Digest:        s.SourceDigest,
		},
		Producer: summaryProducer{
			Provider:      s.Producer.Provider,
			Model:         s.Producer.Model,
			ModelVersion:  s.Producer.ModelVersion,
			PromptVersion: s.PromptVersion,
			PromptDigest:  s.PromptDigest,
		},
		Tokens: summaryTokens{
			Source:     s.SourceTokens,
			Summary:    s.SummaryTokens,
			Accounting: string(s.Accounting),
		},
		Trust:       string(s.Trust),
		Summary:     s.Content,
		GeneratedAt: s.GeneratedAt.UTC().Format(time.RFC3339Nano),
		Kind:        SummaryKind,
		Schema:      SummarySchema,
	})
	if err != nil {
		return nil, Errorf(ErrorCodeSummaryInvalid, "summary payload is not encodable")
	}
	return encoded, nil
}

func parsePayload(raw []byte) (*Summary, error) {
	var decoded summaryPayload
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return nil, Errorf(ErrorCodeSummaryInvalid, "stored summary is not valid JSON")
	}
	if decoded.Kind != SummaryKind || decoded.Schema != SummarySchema {
		return nil, Errorf(ErrorCodeSummaryStale, "stored summary uses an unsupported envelope")
	}
	if Trust(decoded.Trust) != TrustDerivedUntrusted {
		return nil, Errorf(ErrorCodeSummaryInvalid, "stored summary carries unexpected trust")
	}
	if strings.TrimSpace(decoded.Source.Digest) == "" || strings.TrimSpace(decoded.Producer.Model) == "" {
		return nil, Errorf(ErrorCodeSummaryInvalid, "stored summary misses provenance")
	}
	generated, err := time.Parse(time.RFC3339Nano, decoded.GeneratedAt)
	if err != nil {
		return nil, Errorf(ErrorCodeSummaryInvalid, "stored summary carries no valid timestamp")
	}
	return &Summary{
		StartSequence: decoded.Source.StartSequence,
		EndSequence:   decoded.Source.EndSequence,
		EventIDs:      decoded.Source.EventIDs,
		SourceDigest:  decoded.Source.Digest,
		Producer: Producer{
			Provider:     decoded.Producer.Provider,
			Model:        decoded.Producer.Model,
			ModelVersion: decoded.Producer.ModelVersion,
		},
		PromptVersion: decoded.Producer.PromptVersion,
		PromptDigest:  decoded.Producer.PromptDigest,
		SourceTokens:  decoded.Tokens.Source,
		SummaryTokens: decoded.Tokens.Summary,
		Accounting:    AccountingClass(decoded.Tokens.Accounting),
		Trust:         Trust(decoded.Trust),
		Content:       decoded.Summary,
		GeneratedAt:   generated,
	}, nil
}
