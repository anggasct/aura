package model

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"iter"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	adkmodel "google.golang.org/adk/v2/model"
	"google.golang.org/genai"
)

// errStopYield is a distinct sentinel for "the caller stopped iterating"; it
// must never alias io.EOF, or a dropped connection would look like a clean
// stop.
var errStopYield = errors.New("model: stream iterator stopped")

const maxResponseBytes = 64 << 20

// providerCodec is the provider-specific surface a coreClient needs:
// request construction, endpoint selection, non-streaming decode, stream
// event decoding, and auth headers. HTTP, retry, SSE, timeout, and error
// classification live once in the core.
type providerCodec interface {
	protocol() string
	endpoint(baseURL string, req *adkmodel.LLMRequest, stream bool) string
	buildRequest(req *adkmodel.LLMRequest, stream bool) ([]byte, error)
	decodeResponse(body []byte) (*adkmodel.LLMResponse, error)
	// decodeStreamEvent maps one SSE data payload into accumulator
	// operations. terminal marks the provider's end-of-stream marker
	// (message_stop, [DONE], response.completed, a finish-reason chunk).
	decodeStreamEvent(data []byte) ([]streamOp, bool, error)
	setAuthHeaders(req *http.Request, apiKey string)
}

type streamOpKind int

const (
	opModel streamOpKind = iota
	opText
	opToolStart
	opToolArgs
	opUsage
	opStop
	opDone
)

// streamOp is one accumulator operation decoded from a provider event. idx
// carries the provider's tool-call index where it has one (OpenAI), or -1.
type streamOp struct {
	kind     streamOpKind
	idx      int
	text     string
	model    string
	toolID   string
	toolName string
	toolArgs string
	usageIn  int32
	usageOut int32
	stop     string
	final    *adkmodel.LLMResponse
}

type coreClient struct {
	name         string
	baseURL      string
	apiKey       string
	httpClient   *http.Client
	streamClient *http.Client
	timeout      time.Duration
	retry        RetryConfig
	idleTimeout  time.Duration
	codec        providerCodec
	logger       *slog.Logger
}

// newCoreClient resolves a nil logger to the process default once, here, so
// no request path reaches for a global while serving.
func newCoreClient(logger *slog.Logger, name, baseURL, apiKey string, timeout, idleTimeout time.Duration, codec providerCodec) *coreClient {
	if logger == nil {
		logger = slog.Default()
	}
	if timeout <= 0 {
		timeout = defaultRequestTimeout
	}
	if idleTimeout <= 0 {
		idleTimeout = defaultStreamingIdleTimeout
	}
	checkRedirect := rejectCrossOriginRedirect
	return &coreClient{
		name:         name,
		baseURL:      strings.TrimRight(baseURL, "/"),
		apiKey:       apiKey,
		httpClient:   &http.Client{Timeout: timeout, CheckRedirect: checkRedirect},
		streamClient: &http.Client{CheckRedirect: checkRedirect},
		timeout:      timeout,
		retry:        defaultRetryConfig(),
		idleTimeout:  idleTimeout,
		codec:        codec,
		logger:       logger,
	}
}

func (c *coreClient) GenerateContent(ctx context.Context, req *adkmodel.LLMRequest, stream bool) iter.Seq2[*adkmodel.LLMResponse, error] {
	return func(yield func(*adkmodel.LLMResponse, error) bool) {
		telemetry := newModelTelemetry(c.logger, c.codec.protocol(), c.name)
		resp, retries, err := c.do(ctx, req, stream)
		telemetry.retries = retries
		if err != nil {
			telemetry.finish(ctx, nil, err)
			yield(nil, err)
			return
		}
		defer func() { _ = resp.Body.Close() }()

		if cErr := classifyHTTPResponse(resp, c.codec.protocol()); cErr != nil {
			telemetry.finish(ctx, nil, cErr)
			yield(nil, cErr)
			return
		}
		if cErr := validateContentType(resp.Header.Get("Content-Type"), stream, c.codec.protocol()); cErr != nil {
			telemetry.finish(ctx, nil, cErr)
			yield(nil, cErr)
			return
		}

		if stream {
			c.stream(ctx, resp.Body, telemetry, yield)
			return
		}
		payload, err := readCapped(resp.Body)
		if err != nil {
			telemetry.finish(ctx, nil, err)
			yield(nil, fmt.Errorf("model: failed to read %s response: %w", c.codec.protocol(), err))
			return
		}
		out, err := c.codec.decodeResponse(payload)
		if err != nil {
			telemetry.finish(ctx, nil, err)
			yield(nil, err)
			return
		}
		telemetry.finish(ctx, out, nil)
		yield(out, nil)
	}
}

func (c *coreClient) do(ctx context.Context, req *adkmodel.LLMRequest, stream bool) (*http.Response, int, error) {
	retryCount := 0
	body, err := c.codec.buildRequest(req, stream)
	if err != nil {
		return nil, retryCount, err
	}
	if !stream {
		// A long stream is gated by the idle timeout, never by a total
		// deadline; non-streaming calls get a per-request deadline.
		if _, hasDeadline := ctx.Deadline(); !hasDeadline {
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeout(ctx, c.timeout)
			defer cancel()
		}
	}
	client := c.httpClient
	if stream {
		client = c.streamClient
	}
	retry := retryConfigForRequest(c.retry, req)
	retry.RetryCount = &retryCount
	resp, err := retryHTTP(ctx, retry, func() (*http.Response, error) {
		httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.codec.endpoint(c.baseURL, req, stream), bytes.NewReader(body))
		if err != nil {
			return nil, fmt.Errorf("model: failed to create %s request: %w", c.codec.protocol(), err)
		}
		httpReq.Header.Set("Content-Type", "application/json")
		if c.apiKey != "" {
			c.codec.setAuthHeaders(httpReq, c.apiKey)
		}
		resp, err := client.Do(httpReq)
		if err != nil {
			return nil, classifyRequestError(err)
		}
		return resp, nil
	})
	return resp, retryCount, err
}

func (c *coreClient) stream(ctx context.Context, body io.ReadCloser, telemetry *modelTelemetry, yield func(*adkmodel.LLMResponse, error) bool) {
	acc := &streamAccumulator{}
	completed := false
	err := scanSSE(ctx, body, c.idleTimeout, func(data []byte) error {
		ops, terminal, err := c.codec.decodeStreamEvent(data)
		if err != nil {
			return err
		}
		for i := range ops {
			op := &ops[i]
			switch op.kind {
			case opModel:
				acc.model = op.model
			case opText:
				acc.appendText(op.text)
				partial := &adkmodel.LLMResponse{
					Content: &genai.Content{Role: "model", Parts: []*genai.Part{{Text: op.text}}},
					Partial: true,
				}
				if op.usageIn != 0 || op.usageOut != 0 {
					partial.UsageMetadata = usageMetadata(op.usageIn, op.usageOut)
				}
				if !yield(partial, nil) {
					return errStopYield
				}
			case opToolStart:
				acc.startToolAt(op.idx, op.toolID, op.toolName)
			case opToolArgs:
				acc.appendToolArgsAt(op.idx, op.toolArgs)
			case opUsage:
				acc.setUsage(op.usageIn, op.usageOut)
			case opStop:
				acc.stopReason = op.stop
			case opDone:
				completed = true
				resp := op.final
				if resp == nil {
					var finalErr error
					resp, finalErr = acc.final()
					if finalErr != nil {
						return finalErr
					}
				}
				telemetry.finish(ctx, resp, nil)
				if !yield(resp, nil) {
					return errStopYield
				}
			}
		}
		if terminal {
			completed = true
		}
		return nil
	})
	if err != nil && !errors.Is(err, errStopYield) {
		if errors.Is(err, bufio.ErrTooLong) {
			err = codedError(ErrorCodeStreamInvalid, bufio.ErrTooLong, c.codec.protocol()+": stream line exceeds the buffer cap")
		}
		telemetry.finish(ctx, nil, err)
		yield(nil, err)
		return
	}
	telemetry.finishIfNeeded(ctx)
	if !completed {
		yield(nil, codedError(ErrorCodeStreamInvalid, ErrStreamTruncated, c.codec.protocol()+": stream ended before the terminal event"))
	}
}

func readCapped(body io.Reader) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(body, maxResponseBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxResponseBytes {
		return nil, newError(ErrorCodeProtocolInvalid, "", "", "model: response exceeds the size limit")
	}
	return data, nil
}

// validateContentType rejects a body whose media type cannot be parsed by the
// request path, so a proxy page or error page never reaches a decoder.
func validateContentType(contentType string, stream bool, provider string) error {
	mediaType := strings.ToLower(strings.TrimSpace(strings.Split(contentType, ";")[0]))
	if stream {
		if mediaType != "text/event-stream" {
			return codedError(ErrorCodeStreamInvalid, nil, provider+": unexpected stream content type "+strconv.Quote(contentType))
		}
		return nil
	}
	if mediaType != "application/json" {
		return codedError(ErrorCodeProtocolInvalid, nil, provider+": unexpected content type "+strconv.Quote(contentType))
	}
	return nil
}

type streamPartKind int

const (
	partText streamPartKind = iota
	partTool
)

type streamPart struct {
	kind streamPartKind
	text strings.Builder
	tool *toolCallAccum
}

type toolCallAccum struct {
	id   string
	name string
	args strings.Builder
}

// streamAccumulator folds provider events into one ordered content model:
// text and tool-call parts in appearance order, plus usage and stop reason.
type streamAccumulator struct {
	model        string
	parts        []*streamPart
	toolsByIndex map[int]*toolCallAccum
	inputTokens  int32
	outputTokens int32
	stopReason   string
}

func (a *streamAccumulator) appendText(s string) {
	if len(a.parts) == 0 || a.parts[len(a.parts)-1].kind != partText {
		a.parts = append(a.parts, &streamPart{kind: partText})
	}
	a.parts[len(a.parts)-1].text.WriteString(s)
}

func (a *streamAccumulator) startToolAt(idx int, id, name string) {
	tool := &toolCallAccum{id: id, name: name}
	if idx >= 0 {
		if a.toolsByIndex == nil {
			a.toolsByIndex = map[int]*toolCallAccum{}
		}
		if existing, ok := a.toolsByIndex[idx]; ok {
			existing.id = id
			existing.name = name
			return
		}
		a.toolsByIndex[idx] = tool
	}
	a.parts = append(a.parts, &streamPart{kind: partTool, tool: tool})
}

func (a *streamAccumulator) appendToolArgsAt(idx int, s string) {
	if idx >= 0 && a.toolsByIndex != nil {
		if tool, ok := a.toolsByIndex[idx]; ok {
			tool.args.WriteString(s)
			return
		}
	}
	if len(a.parts) == 0 || a.parts[len(a.parts)-1].kind != partTool {
		a.parts = append(a.parts, &streamPart{kind: partTool, tool: &toolCallAccum{}})
	}
	a.parts[len(a.parts)-1].tool.args.WriteString(s)
}

func (a *streamAccumulator) setUsage(input, output int32) {
	if input != 0 {
		a.inputTokens = input
	}
	if output != 0 {
		a.outputTokens = output
	}
}

func (a *streamAccumulator) final() (*adkmodel.LLMResponse, error) {
	content := &genai.Content{Role: "model"}
	for _, part := range a.parts {
		switch part.kind {
		case partText:
			if part.text.Len() > 0 {
				content.Parts = append(content.Parts, &genai.Part{Text: part.text.String()})
			}
		case partTool:
			args := json.RawMessage(part.tool.args.String())
			if len(bytes.TrimSpace(args)) == 0 {
				args = json.RawMessage("{}")
			}
			if !json.Valid(args) {
				return nil, fmt.Errorf("model: invalid tool call %q: %w", part.tool.name, ErrInvalidToolCall)
			}
			toolCall := ToolCall{ID: part.tool.id, Name: part.tool.name, Arguments: args}
			fc, err := canonicalToFunctionCall(toolCall)
			if err != nil {
				return nil, err
			}
			content.Parts = append(content.Parts, &genai.Part{FunctionCall: fc})
		}
	}
	return &adkmodel.LLMResponse{
		Content:       content,
		ModelVersion:  a.model,
		TurnComplete:  true,
		UsageMetadata: usageMetadata(a.inputTokens, a.outputTokens),
	}, nil
}

func usageMetadata(input, output int32) *genai.GenerateContentResponseUsageMetadata {
	return &genai.GenerateContentResponseUsageMetadata{
		PromptTokenCount:     input,
		CandidatesTokenCount: output,
		TotalTokenCount:      input + output,
	}
}
