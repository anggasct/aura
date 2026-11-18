package config

import (
	"errors"
	"fmt"
	"regexp"
	"unicode"
	"unicode/utf8"

	"github.com/anggasct/aura/internal/platform/configkit"
	"github.com/anggasct/aura/internal/platform/secret"
)

const (
	EmbeddingRoleNone      = "none"
	EmbeddingRoleInputType = "input_type"
	EmbeddingRolePrefix    = "prefix"

	DefaultEmbeddingDimensionsField = "dimensions"
	DefaultEmbeddingBatchSize       = 96
	DefaultEmbeddingMaxMemory       = configkit.ByteSize(256 << 20)

	MaxEmbeddingBatchSize  = 1000
	MaxEmbeddingDimensions = 8192
	MinEmbeddingMaxMemory  = configkit.ByteSize(1 << 20)

	maxEmbeddingModelLength  = 200
	maxEmbeddingPrefixLength = 256
	maxEmbeddingFieldLength  = 32

	embeddingBaseURLField = "knowledge.embedding.base_url"
)

var (
	embeddingModelPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/@+-]*$`)
	embeddingFieldPattern = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

	reservedEmbeddingFields = map[string]struct{}{
		"model": {}, "input": {}, "input_type": {}, "encoding_format": {}, "user": {},
	}
)

type Knowledge struct {
	Embedding *KnowledgeEmbedding `json:"embedding,omitempty"`
}

type KnowledgeEmbedding struct {
	BaseURL         string             `json:"base_url"`
	Model           string             `json:"model"`
	Dimensions      int                `json:"dimensions"`
	DimensionsField string             `json:"dimensions_field"`
	CredentialRef   secret.Ref         `json:"credential_ref,omitzero"`
	RoleStyle       string             `json:"role_style"`
	QueryPrefix     string             `json:"query_prefix,omitempty"`
	DocumentPrefix  string             `json:"document_prefix,omitempty"`
	BatchSize       int                `json:"batch_size"`
	MaxMemory       configkit.ByteSize `json:"max_memory"`
	PublicQueries   bool               `json:"public_queries"`
}

func (e *KnowledgeEmbedding) Endpoint() (ServiceEndpoint, error) {
	if e == nil {
		return ServiceEndpoint{}, configkit.Invalid("knowledge.embedding", "is not configured")
	}
	endpoint, err := parseServiceEndpoint(embeddingBaseURLField, e.BaseURL)
	if err != nil {
		return ServiceEndpoint{}, err
	}
	return endpoint, nil
}

func validateKnowledge(knowledge *Knowledge) error {
	if knowledge == nil || knowledge.Embedding == nil {
		return nil
	}
	embedding := knowledge.Embedding
	var problems []error
	if _, err := parseServiceEndpoint(embeddingBaseURLField, embedding.BaseURL); err != nil {
		problems = append(problems, err)
	}
	if len(embedding.Model) > maxEmbeddingModelLength || !embeddingModelPattern.MatchString(embedding.Model) {
		problems = append(problems, configkit.Invalid("knowledge.embedding.model", "is required and must be a plain model name"))
	}
	if embedding.Dimensions < 1 || embedding.Dimensions > MaxEmbeddingDimensions {
		problems = append(problems, configkit.Invalid("knowledge.embedding.dimensions", fmt.Sprintf("is required and must be between 1 and %d", MaxEmbeddingDimensions)))
	}
	problems = append(problems, validateEmbeddingField(embedding.DimensionsField)...)
	problems = append(problems, validateEmbeddingRole(embedding)...)
	if embedding.BatchSize < 1 || embedding.BatchSize > MaxEmbeddingBatchSize {
		problems = append(problems, configkit.Invalid("knowledge.embedding.batch_size", fmt.Sprintf("must be between 1 and %d", MaxEmbeddingBatchSize)))
	}
	if embedding.MaxMemory < MinEmbeddingMaxMemory {
		problems = append(problems, configkit.Invalid("knowledge.embedding.max_memory", fmt.Sprintf("must be at least %s", MinEmbeddingMaxMemory)))
	}
	return errors.Join(problems...)
}

func validateEmbeddingField(field string) []error {
	if len(field) > maxEmbeddingFieldLength || !embeddingFieldPattern.MatchString(field) {
		return []error{configkit.Invalid("knowledge.embedding.dimensions_field", "must be a lowercase request field name")}
	}
	if _, reserved := reservedEmbeddingFields[field]; reserved {
		return []error{configkit.Invalid("knowledge.embedding.dimensions_field", "must not be a reserved request field")}
	}
	return nil
}

func validateEmbeddingRole(embedding *KnowledgeEmbedding) []error {
	var problems []error
	switch embedding.RoleStyle {
	case EmbeddingRoleNone, EmbeddingRoleInputType:
		if embedding.QueryPrefix != "" || embedding.DocumentPrefix != "" {
			problems = append(problems, configkit.Invalid("knowledge.embedding", "prefixes require role_style prefix"))
		}
	case EmbeddingRolePrefix:
		if embedding.QueryPrefix == "" && embedding.DocumentPrefix == "" {
			problems = append(problems, configkit.Invalid("knowledge.embedding.role_style", "prefix requires query_prefix or document_prefix"))
		}
	default:
		problems = append(problems, configkit.Invalid("knowledge.embedding.role_style", "must be none, input_type or prefix"))
	}
	for _, prefix := range []struct {
		name  string
		value string
	}{{"query_prefix", embedding.QueryPrefix}, {"document_prefix", embedding.DocumentPrefix}} {
		if !validEmbeddingPrefix(prefix.value) {
			problems = append(problems, configkit.Invalid("knowledge.embedding."+prefix.name, fmt.Sprintf("must be at most %d bytes of printable text", maxEmbeddingPrefixLength)))
		}
	}
	return problems
}

func validEmbeddingPrefix(prefix string) bool {
	if len(prefix) > maxEmbeddingPrefixLength || !utf8.ValidString(prefix) {
		return false
	}
	for _, r := range prefix {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

func (*KnowledgeEmbedding) Default() any {
	return KnowledgeEmbedding{
		DimensionsField: DefaultEmbeddingDimensionsField,
		RoleStyle:       EmbeddingRoleNone,
		BatchSize:       DefaultEmbeddingBatchSize,
		MaxMemory:       DefaultEmbeddingMaxMemory,
	}
}
