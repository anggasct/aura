package knowledge

import (
	"errors"
	"fmt"
	"regexp"
	"unicode"
	"unicode/utf8"

	"github.com/anggasct/aura/internal/platform/configkit"
	"github.com/anggasct/aura/internal/platform/egress"
	"github.com/anggasct/aura/internal/platform/embedding"
	"github.com/anggasct/aura/internal/platform/secret"
)

const (
	EmbeddingRoleNone      = embedding.RoleStyleNone
	EmbeddingRoleInputType = embedding.RoleStyleInputType
	EmbeddingRolePrefix    = embedding.RoleStylePrefix

	DefaultEmbeddingDimensionsField = "dimensions"
	DefaultEmbeddingBatchSize       = 96
	DefaultEmbeddingMaxMemory       = configkit.ByteSize(256 << 20)

	MaxEmbeddingBatchSize  = embedding.MaxBatchSize
	MaxEmbeddingDimensions = 8192
	MinEmbeddingMaxMemory  = configkit.ByteSize(1 << 20)

	maxEmbeddingModelLength  = 200
	maxEmbeddingPrefixLength = 256
	maxEmbeddingFieldLength  = 32

	embeddingBaseURLField = "embedding.base_url"
)

var (
	embeddingModelPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/@+-]*$`)
	embeddingFieldPattern = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

	reservedEmbeddingFields = map[string]struct{}{
		"model": {}, "input": {}, "input_type": {}, "encoding_format": {}, "user": {},
	}
)

type Config struct {
	Embedding *EmbeddingConfig `json:"embedding,omitempty"`
}

type EmbeddingConfig struct {
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

func Default() Config {
	return Config{}
}

func (*EmbeddingConfig) Default() any {
	return EmbeddingConfig{
		DimensionsField: DefaultEmbeddingDimensionsField,
		RoleStyle:       EmbeddingRoleNone,
		BatchSize:       DefaultEmbeddingBatchSize,
		MaxMemory:       DefaultEmbeddingMaxMemory,
	}
}

func (e *EmbeddingConfig) Endpoint() (egress.ServiceEndpoint, error) {
	if e == nil {
		return egress.ServiceEndpoint{}, configkit.Invalid("embedding", "is not configured")
	}
	endpoint, err := egress.ParseServiceEndpoint(e.BaseURL)
	if err != nil {
		return egress.ServiceEndpoint{}, configkit.Invalid(embeddingBaseURLField, err.Error())
	}
	return endpoint, nil
}

func (e *EmbeddingConfig) ClientConfig() embedding.Config {
	if e == nil {
		return embedding.Config{}
	}
	return embedding.Config{
		BaseURL:         e.BaseURL,
		Model:           e.Model,
		Dimensions:      e.Dimensions,
		DimensionsField: e.DimensionsField,
		RoleStyle:       e.RoleStyle,
		QueryPrefix:     e.QueryPrefix,
		DocumentPrefix:  e.DocumentPrefix,
	}
}

func (c *Config) Validate() error {
	if c == nil || c.Embedding == nil {
		return nil
	}
	cfg := c.Embedding
	var problems []error
	if _, err := egress.ParseServiceEndpoint(cfg.BaseURL); err != nil {
		problems = append(problems, configkit.Invalid(embeddingBaseURLField, err.Error()))
	}
	if len(cfg.Model) > maxEmbeddingModelLength || !embeddingModelPattern.MatchString(cfg.Model) {
		problems = append(problems, configkit.Invalid("embedding.model", "is required and must be a plain model name"))
	}
	if cfg.Dimensions < 1 || cfg.Dimensions > MaxEmbeddingDimensions {
		problems = append(problems, configkit.Invalid("embedding.dimensions", fmt.Sprintf("is required and must be between 1 and %d", MaxEmbeddingDimensions)))
	}
	problems = append(problems, validateEmbeddingField(cfg.DimensionsField)...)
	problems = append(problems, validateEmbeddingRole(cfg)...)
	if cfg.BatchSize < 1 || cfg.BatchSize > MaxEmbeddingBatchSize {
		problems = append(problems, configkit.Invalid("embedding.batch_size", fmt.Sprintf("must be between 1 and %d", MaxEmbeddingBatchSize)))
	}
	if cfg.MaxMemory < MinEmbeddingMaxMemory {
		problems = append(problems, configkit.Invalid("embedding.max_memory", fmt.Sprintf("must be at least %s", MinEmbeddingMaxMemory)))
	}
	return errors.Join(problems...)
}

func validateEmbeddingField(field string) []error {
	if len(field) > maxEmbeddingFieldLength || !embeddingFieldPattern.MatchString(field) {
		return []error{configkit.Invalid("embedding.dimensions_field", "must be a lowercase request field name")}
	}
	if _, reserved := reservedEmbeddingFields[field]; reserved {
		return []error{configkit.Invalid("embedding.dimensions_field", "must not be a reserved request field")}
	}
	return nil
}

func validateEmbeddingRole(cfg *EmbeddingConfig) []error {
	var problems []error
	switch cfg.RoleStyle {
	case EmbeddingRoleNone, EmbeddingRoleInputType:
		if cfg.QueryPrefix != "" || cfg.DocumentPrefix != "" {
			problems = append(problems, configkit.Invalid("embedding", "prefixes require role_style prefix"))
		}
	case EmbeddingRolePrefix:
		if cfg.QueryPrefix == "" && cfg.DocumentPrefix == "" {
			problems = append(problems, configkit.Invalid("embedding.role_style", "prefix requires query_prefix or document_prefix"))
		}
	default:
		problems = append(problems, configkit.Invalid("embedding.role_style", "must be none, input_type or prefix"))
	}
	for _, prefix := range []struct {
		name  string
		value string
	}{{"query_prefix", cfg.QueryPrefix}, {"document_prefix", cfg.DocumentPrefix}} {
		if !validEmbeddingPrefix(prefix.value) {
			problems = append(problems, configkit.Invalid("embedding."+prefix.name, fmt.Sprintf("must be at most %d bytes of printable text", maxEmbeddingPrefixLength)))
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
