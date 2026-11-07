package config

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	yamlv3 "gopkg.in/yaml.v3"
)

const (
	EmbeddingRoleNone      = "none"
	EmbeddingRoleInputType = "input_type"
	EmbeddingRolePrefix    = "prefix"

	DefaultEmbeddingDimensionsField = "dimensions"
	DefaultEmbeddingBatchSize       = 96
	DefaultEmbeddingMaxMemory       = ByteSize(256 << 20)

	MaxEmbeddingBatchSize  = 1000
	MaxEmbeddingDimensions = 8192
	MinEmbeddingMaxMemory  = ByteSize(1 << 20)

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
	Embedding *KnowledgeEmbedding `koanf:"embedding" yaml:"embedding,omitempty"`
}

type KnowledgeEmbedding struct {
	BaseURL         string   `koanf:"base_url" yaml:"base_url"`
	Model           string   `koanf:"model" yaml:"model"`
	Dimensions      int      `koanf:"dimensions" yaml:"dimensions"`
	DimensionsField string   `koanf:"dimensions_field" yaml:"dimensions_field"`
	CredentialRef   string   `koanf:"credential_ref" yaml:"credential_ref,omitempty"`
	RoleStyle       string   `koanf:"role_style" yaml:"role_style"`
	QueryPrefix     string   `koanf:"query_prefix" yaml:"query_prefix,omitempty"`
	DocumentPrefix  string   `koanf:"document_prefix" yaml:"document_prefix,omitempty"`
	BatchSize       int      `koanf:"batch_size" yaml:"batch_size"`
	MaxMemory       ByteSize `koanf:"max_memory" yaml:"max_memory"`
	PublicQueries   bool     `koanf:"public_queries" yaml:"public_queries"`
}

func (e *KnowledgeEmbedding) Endpoint() (ServiceEndpoint, error) {
	if e == nil {
		return ServiceEndpoint{}, &Error{Code: ErrorCodeConfigInvalid, Detail: "knowledge.embedding is not configured"}
	}
	endpoint, err := parseServiceEndpoint(embeddingBaseURLField, e.BaseURL)
	if err != nil {
		return ServiceEndpoint{}, &Error{Code: ErrorCodeConfigInvalid, Detail: err.Error()}
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
		problems = append(problems, errors.New("knowledge.embedding.model is required and must be a plain model name"))
	}
	if embedding.Dimensions < 1 || embedding.Dimensions > MaxEmbeddingDimensions {
		problems = append(problems, fmt.Errorf("knowledge.embedding.dimensions is required and must be between 1 and %d", MaxEmbeddingDimensions))
	}
	problems = append(problems, validateEmbeddingField(embedding.DimensionsField)...)
	problems = append(problems, validateEmbeddingCredentialRef(embedding.CredentialRef)...)
	problems = append(problems, validateEmbeddingRole(embedding)...)
	if embedding.BatchSize < 1 || embedding.BatchSize > MaxEmbeddingBatchSize {
		problems = append(problems, fmt.Errorf("knowledge.embedding.batch_size must be between 1 and %d", MaxEmbeddingBatchSize))
	}
	if embedding.MaxMemory < MinEmbeddingMaxMemory {
		problems = append(problems, fmt.Errorf("knowledge.embedding.max_memory must be at least %s", MinEmbeddingMaxMemory))
	}
	if err := errors.Join(problems...); err != nil {
		return &Error{Code: ErrorCodeConfigInvalid, Detail: err.Error()}
	}
	return nil
}

func validateEmbeddingField(field string) []error {
	if len(field) > maxEmbeddingFieldLength || !embeddingFieldPattern.MatchString(field) {
		return []error{errors.New("knowledge.embedding.dimensions_field must be a lowercase request field name")}
	}
	if _, reserved := reservedEmbeddingFields[field]; reserved {
		return []error{fmt.Errorf("knowledge.embedding.dimensions_field must not be %q", field)}
	}
	return nil
}

func validateEmbeddingCredentialRef(ref string) []error {
	switch {
	case ref == "":
		return nil
	case strings.HasPrefix(ref, "env://") && len(ref) > len("env://"):
		return nil
	case strings.HasPrefix(ref, "file://") && len(ref) > len("file://"):
		return nil
	}
	return []error{errors.New("knowledge.embedding.credential_ref must use env:// or file:// with a name")}
}

func validateEmbeddingRole(embedding *KnowledgeEmbedding) []error {
	var problems []error
	switch embedding.RoleStyle {
	case EmbeddingRoleNone, EmbeddingRoleInputType:
		if embedding.QueryPrefix != "" || embedding.DocumentPrefix != "" {
			problems = append(problems, errors.New("knowledge.embedding prefixes require role_style prefix"))
		}
	case EmbeddingRolePrefix:
		if embedding.QueryPrefix == "" && embedding.DocumentPrefix == "" {
			problems = append(problems, errors.New("knowledge.embedding.role_style prefix requires query_prefix or document_prefix"))
		}
	default:
		problems = append(problems, errors.New("knowledge.embedding.role_style must be none, input_type or prefix"))
	}
	for _, prefix := range []struct {
		name  string
		value string
	}{{"query_prefix", embedding.QueryPrefix}, {"document_prefix", embedding.DocumentPrefix}} {
		if !validEmbeddingPrefix(prefix.value) {
			problems = append(problems, fmt.Errorf("knowledge.embedding.%s must be at most %d bytes of printable text", prefix.name, maxEmbeddingPrefixLength))
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

func applyKnowledgeDefaults(cfg *Config, doc *yamlv3.Node) {
	if cfg.Knowledge == nil || cfg.Knowledge.Embedding == nil {
		return
	}
	embedding := cfg.Knowledge.Embedding
	if embedding.DimensionsField == "" && !configValuePresent(doc, "knowledge", "embedding", "dimensions_field") && !envValuePresent("knowledge.embedding.dimensions_field") {
		embedding.DimensionsField = DefaultEmbeddingDimensionsField
	}
	if embedding.RoleStyle == "" && !configValuePresent(doc, "knowledge", "embedding", "role_style") && !envValuePresent("knowledge.embedding.role_style") {
		embedding.RoleStyle = EmbeddingRoleNone
	}
	if embedding.BatchSize == 0 && !configValuePresent(doc, "knowledge", "embedding", "batch_size") && !envValuePresent("knowledge.embedding.batch_size") {
		embedding.BatchSize = DefaultEmbeddingBatchSize
	}
	if embedding.MaxMemory == 0 && !configValuePresent(doc, "knowledge", "embedding", "max_memory") && !envValuePresent("knowledge.embedding.max_memory") {
		embedding.MaxMemory = DefaultEmbeddingMaxMemory
	}
}

func validateKnowledgeShapes(doc *yamlv3.Node) error {
	knowledgeNode := mappingValue(doc, "knowledge")
	if knowledgeNode == nil {
		return nil
	}
	if knowledgeNode.Kind != yamlv3.MappingNode {
		return fmt.Errorf("knowledge must be a mapping at line %d", knowledgeNode.Line)
	}
	embeddingNode := mappingValue(knowledgeNode, "embedding")
	if embeddingNode == nil {
		return nil
	}
	if embeddingNode.Kind != yamlv3.MappingNode {
		return fmt.Errorf("knowledge.embedding must be a mapping at line %d", embeddingNode.Line)
	}
	for i := 0; i+1 < len(embeddingNode.Content); i += 2 {
		keyNode := embeddingNode.Content[i]
		valueNode := embeddingNode.Content[i+1]
		switch keyNode.Value {
		case "base_url", "model", "dimensions_field", "credential_ref", "role_style", "query_prefix", "document_prefix":
			if valueNode.Kind != yamlv3.ScalarNode || valueNode.Tag != "!!str" {
				return fmt.Errorf("knowledge.embedding.%s must be a string at line %d", keyNode.Value, valueNode.Line)
			}
		case "dimensions", "batch_size":
			if valueNode.Kind != yamlv3.ScalarNode || valueNode.Tag != "!!int" {
				return fmt.Errorf("knowledge.embedding.%s must be an integer at line %d", keyNode.Value, valueNode.Line)
			}
		case "max_memory":
			if valueNode.Kind != yamlv3.ScalarNode || (valueNode.Tag != "!!int" && valueNode.Tag != "!!str") {
				return fmt.Errorf("knowledge.embedding.max_memory must be a byte size at line %d", valueNode.Line)
			}
		case "public_queries":
			if valueNode.Kind != yamlv3.ScalarNode || valueNode.Tag != "!!bool" {
				return fmt.Errorf("knowledge.embedding.public_queries must be a boolean at line %d", valueNode.Line)
			}
		}
	}
	return nil
}
