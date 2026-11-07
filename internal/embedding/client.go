package embedding

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"time"

	"github.com/anggasct/aura/internal/config"
	"github.com/anggasct/aura/internal/egress"
)

type Role string

const (
	RoleQuery    Role = "query"
	RoleDocument Role = "document"

	defaultRequestTimeout = 60 * time.Second
	maxBytesPerValue      = 40
	responseSlackBytes    = 1 << 20
	maxProviderBatch      = config.MaxEmbeddingBatchSize
)

type Client struct {
	http            *http.Client
	url             string
	credential      string
	model           string
	dimensions      int
	dimensionsField string
	roleStyle       string
	queryPrefix     string
	documentPrefix  string
	timeout         time.Duration
}

func New(cfg *config.KnowledgeEmbedding, credential string, resolver egress.Resolver) (*Client, error) {
	if cfg == nil {
		return nil, Errorf(ErrorCodeConfigInvalid, "embedding configuration must not be nil")
	}
	endpoint, err := cfg.Endpoint()
	if err != nil {
		return nil, Errorf(ErrorCodeConfigInvalid, "%v", err)
	}
	if endpoint.Loopback {
		return newClient(cfg, endpoint, credential, newLoopbackClient(endpoint)), nil
	}
	return newClient(cfg, endpoint, credential, newRemoteClient(resolver)), nil
}

func newClient(cfg *config.KnowledgeEmbedding, endpoint config.ServiceEndpoint, credential string, httpClient *http.Client) *Client {
	return &Client{
		http:            httpClient,
		url:             endpoint.URL() + "/embeddings",
		credential:      credential,
		model:           cfg.Model,
		dimensions:      cfg.Dimensions,
		dimensionsField: cfg.DimensionsField,
		roleStyle:       cfg.RoleStyle,
		queryPrefix:     cfg.QueryPrefix,
		documentPrefix:  cfg.DocumentPrefix,
		timeout:         defaultRequestTimeout,
	}
}

func (c *Client) Embed(ctx context.Context, role Role, texts []string) ([][]float32, error) {
	input, err := c.input(role, texts)
	if err != nil {
		return nil, err
	}
	body, err := c.requestBody(role, input)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url, bytes.NewReader(body))
	if err != nil {
		return nil, Errorf(ErrorCodeRequestInvalid, "the embedding request could not be built")
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	if c.credential != "" {
		request.Header.Set("Authorization", "Bearer "+c.credential)
	}
	response, err := c.http.Do(request)
	if err != nil {
		return nil, wrapTransportError(err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return nil, Errorf(ErrorCodeStatusRejected, "the embedding provider answered with status %d", response.StatusCode)
	}
	limit := int64(len(texts))*int64(c.dimensions)*maxBytesPerValue + responseSlackBytes
	raw, err := io.ReadAll(io.LimitReader(response.Body, limit+1))
	if err != nil {
		return nil, fmt.Errorf("%w: %w", &Error{Code: ErrorCodeRequestFailed, Detail: "the embedding response could not be read"}, err)
	}
	if int64(len(raw)) > limit {
		return nil, Errorf(ErrorCodeResponseInvalid, "the embedding response is larger than the request allows")
	}
	return c.decode(raw, len(texts))
}

func wrapTransportError(err error) error {
	if _, coded := CodeOf(err); coded {
		return err
	}
	if _, denied := egress.CodeOf(err); denied {
		return fmt.Errorf("%w: %w", &Error{Code: ErrorCodeDestinationDenied, Detail: "the embedding destination is not permitted"}, err)
	}
	return fmt.Errorf("%w: %w", &Error{Code: ErrorCodeRequestFailed, Detail: "the request to the embedding provider failed"}, err)
}

func (c *Client) input(role Role, texts []string) ([]string, error) {
	if role != RoleQuery && role != RoleDocument {
		return nil, Errorf(ErrorCodeRequestInvalid, "role must be query or document")
	}
	if len(texts) == 0 || len(texts) > maxProviderBatch {
		return nil, Errorf(ErrorCodeRequestInvalid, "an embedding request carries between 1 and %d texts", maxProviderBatch)
	}
	prefix := ""
	if c.roleStyle == config.EmbeddingRolePrefix {
		prefix = c.documentPrefix
		if role == RoleQuery {
			prefix = c.queryPrefix
		}
	}
	input := make([]string, len(texts))
	for i, text := range texts {
		if text == "" {
			return nil, Errorf(ErrorCodeRequestInvalid, "an embedding input must not be empty")
		}
		input[i] = prefix + text
	}
	return input, nil
}

func (c *Client) requestBody(role Role, input []string) ([]byte, error) {
	payload := map[string]any{
		"model":           c.model,
		"input":           input,
		c.dimensionsField: c.dimensions,
	}
	if c.roleStyle == config.EmbeddingRoleInputType {
		payload["input_type"] = string(role)
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, Errorf(ErrorCodeRequestInvalid, "the embedding request could not be encoded")
	}
	return body, nil
}

type embeddingResponse struct {
	Data []embeddingItem `json:"data"`
}

type embeddingItem struct {
	Index     *int      `json:"index"`
	Embedding []float64 `json:"embedding"`
}

func (c *Client) decode(raw []byte, want int) ([][]float32, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	var parsed embeddingResponse
	if err := decoder.Decode(&parsed); err != nil {
		return nil, Errorf(ErrorCodeResponseInvalid, "the embedding response is not the expected JSON")
	}
	if decoder.More() {
		return nil, Errorf(ErrorCodeResponseInvalid, "the embedding response has trailing content")
	}
	if len(parsed.Data) != want {
		return nil, Errorf(ErrorCodeResponseInvalid, "the provider returned %d vectors for %d inputs", len(parsed.Data), want)
	}
	vectors := make([][]float32, want)
	for i := range parsed.Data {
		item := &parsed.Data[i]
		if item.Index != nil && *item.Index != i {
			return nil, Errorf(ErrorCodeResponseInvalid, "the provider returned vectors out of order")
		}
		vector, err := c.vector(item.Embedding)
		if err != nil {
			return nil, err
		}
		vectors[i] = vector
	}
	return vectors, nil
}

func (c *Client) vector(values []float64) ([]float32, error) {
	if len(values) != c.dimensions {
		return nil, Errorf(ErrorCodeResponseInvalid, "a vector has %d values, want %d", len(values), c.dimensions)
	}
	vector := make([]float32, len(values))
	nonZero := false
	for i, value := range values {
		narrowed := float32(value)
		if math.IsNaN(value) || math.IsInf(value, 0) || math.IsInf(float64(narrowed), 0) {
			return nil, Errorf(ErrorCodeResponseInvalid, "a vector holds a value that is not finite")
		}
		if narrowed != 0 {
			nonZero = true
		}
		vector[i] = narrowed
	}
	if !nonZero {
		return nil, Errorf(ErrorCodeResponseInvalid, "a vector is all zeros")
	}
	return vector, nil
}
