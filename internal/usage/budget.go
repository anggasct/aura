package usage

import (
	"context"
	"encoding/json"
	"errors"
	"iter"
	"log/slog"
	"sync/atomic"

	adkmodel "google.golang.org/adk/v2/model"
	"google.golang.org/genai"
)

// defaultMaxOutputBound is the conservative output token bound used when a
// request does not set MaxOutputTokens, so a reservation is never computed
// from an unbounded output.
const defaultMaxOutputBound = 4096

// Budgeted wraps an LLM and enforces the usage budget around each request:
// it reserves a conservative cost before any network dispatch and settles the
// provider-reported usage once the response completes. Budget exhaustion
// rejects the request before dispatch. A stream that ends in error settles
// conservatively at the reserved amount, never at zero. Settlement failures
// are surfaced so a reservation never completes without an observable,
// exactly-once settlement outcome.
type Budgeted struct {
	inner             adkmodel.LLM
	ledger            *Ledger
	modelDefinitionID string
	logger            *slog.Logger
	// attempts assigns a distinct attempt number to each model call that
	// arrives without an explicit idempotency key, so multiple calls within
	// one runtime invocation (a tool loop, a sub-agent) each get their own
	// reservation instead of colliding on the same (invocation_id, attempt).
	// The counter is per Budgeted instance; stable identity across replays
	// comes from WithInvocation or the invocationCarrier context, never from
	// this counter (it only ever increments, so it never reuses).
	attempts atomic.Int64
}

// NewBudgeted wraps inner with budget enforcement for the given model
// definition. A nil ledger disables enforcement and returns inner unchanged,
// so an unconfigured deployment adds no accounting. A nil logger falls back
// to the process default.
func NewBudgeted(inner adkmodel.LLM, ledger *Ledger, modelDefinitionID string, logger *slog.Logger) (adkmodel.LLM, error) {
	if inner == nil {
		return nil, codedError(ErrorCodeInvalidArgument, "usage: inner model must not be nil", nil)
	}
	if ledger == nil {
		return inner, nil
	}
	if modelDefinitionID == "" {
		return nil, codedError(ErrorCodeInvalidArgument, "usage: model definition id must not be empty", nil)
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Budgeted{inner: inner, ledger: ledger, modelDefinitionID: modelDefinitionID, logger: logger}, nil
}

func (b *Budgeted) Name() string {
	return b.inner.Name()
}

func (b *Budgeted) GenerateContent(ctx context.Context, req *adkmodel.LLMRequest, stream bool) iter.Seq2[*adkmodel.LLMResponse, error] {
	return func(yield func(*adkmodel.LLMResponse, error) bool) {
		invocationID, attempt, err := b.resolveInvocation(ctx)
		if err != nil {
			yield(nil, err)
			return
		}
		inputTokens, err := estimateInputTokens(req, b.ledger.capsEnabled())
		if err != nil {
			yield(nil, err)
			return
		}
		reservation, err := b.ledger.Reserve(ctx, ReserveRequest{
			InvocationID:             invocationID,
			Attempt:                  attempt,
			ModelDefinitionID:        b.modelDefinitionID,
			KnownInputTokens:         inputTokens,
			RequestedMaxOutputTokens: requestedMaxOutput(req),
		})
		if err != nil {
			// Budget exhaustion (or an unknown price under a cap) blocks the
			// request before any network dispatch.
			yield(nil, err)
			return
		}

		var final *adkmodel.LLMResponse
		for resp, err := range b.inner.GenerateContent(ctx, req, stream) {
			if err != nil {
				// Settle conservatively at the reserved amount; a failed
				// attempt must never release its reservation at zero. The
				// provider error is preserved and joined with any settlement
				// failure so both remain observable. The response delivered
				// with the error is whatever the inner stream attached to it
				// (a fresh partial chunk); when the error carries none, the
				// last successfully streamed response is yielded instead of
				// a nil payload. Err-first consumers ignore this response.
				last := resp
				if last == nil {
					last = final
				}
				if _, settleErr := b.ledger.Settle(ctx, &SettleRequest{ReservationID: reservation.ID}); settleErr != nil {
					yield(last, errors.Join(err, settleErr))
				} else {
					yield(last, err)
				}
				return
			}
			if resp != nil {
				final = resp
			}
			if !yield(resp, nil) {
				// The consumer stopped iterating; the final outcome is
				// unknown, so settle conservatively at the reserved amount
				// (never release the reserved remainder at partial usage).
				// The outcome can no longer be reported through the stream,
				// so a settlement failure is logged for reconciliation
				// rather than dropped silently.
				if _, settleErr := b.ledger.Settle(ctx, &SettleRequest{ReservationID: reservation.ID}); settleErr != nil {
					b.logger.WarnContext(ctx, "usage settlement failed after consumer stop",
						"component", "usage", "reservation_id", reservation.ID, "error", settleErr)
				}
				return
			}
		}
		// Clean stream exhaustion. A response with a completion marker is
		// the only evidence the turn actually finished; without it the
		// outcome is unknown and the reservation is settled conservatively
		// at the reserved amount instead of releasing the remainder on a
		// possibly-partial response.
		settleReq := &SettleRequest{ReservationID: reservation.ID}
		if final != nil && final.TurnComplete {
			settleReq = settleForResponse(reservation.ID, final)
		}
		if _, settleErr := b.ledger.Settle(ctx, settleReq); settleErr != nil {
			// The stream already delivered its responses; the settlement
			// failure is the only error left, so it is yielded alongside the
			// last streamed response (or nil when nothing was streamed),
			// mirroring the error-path handler above. Err-first consumers
			// read the error and ignore the response payload.
			yield(final, settleErr)
		}
	}
}

func settleForResponse(reservationID string, resp *adkmodel.LLMResponse) *SettleRequest {
	if resp == nil {
		return &SettleRequest{ReservationID: reservationID}
	}
	usage := Usage{}
	if resp.UsageMetadata != nil {
		usage.InputTokens = int64(resp.UsageMetadata.PromptTokenCount)
		usage.OutputTokens = int64(resp.UsageMetadata.CandidatesTokenCount)
		usage.CacheTokens = int64(resp.UsageMetadata.CachedContentTokenCount)
		usage.ReasoningTokens = int64(resp.UsageMetadata.ThoughtsTokenCount)
	}
	payload, err := json.Marshal(resp.UsageMetadata)
	if err != nil {
		payload = []byte("{}")
	}
	return &SettleRequest{
		ReservationID: reservationID,
		Usage:         usage,
		UsageJSON:     payload,
	}
}

// resolveInvocation derives the reservation idempotency key. An explicit key
// set via WithInvocation wins (the retry/fallback contract); otherwise the
// runtime invocation identity is read from the context when present so a
// replay of the same logical turn reuses its reservation. A call with neither
// falls back to a fresh random identity, which never reuses.
func (b *Budgeted) resolveInvocation(ctx context.Context) (invocationID string, attempt int, err error) {
	if id, attempt, ok := invocationFrom(ctx); ok {
		return id, attempt, nil
	}
	if carrier, ok := ctx.(invocationCarrier); ok {
		if id := carrier.InvocationID(); id != "" {
			return id, int(b.attempts.Add(1)), nil
		}
	}
	id, err := randomID()
	if err != nil {
		return "", 0, err
	}
	return id, 0, nil
}

// estimateInputTokens derives a conservative input token estimate from the
// request contents. It approximates four characters per token; the estimate
// only bounds the reservation, which the settlement releases. Every
// content-bearing part is accounted for. A part whose size cannot be bounded
// (file data referenced by URI) is rejected while a cap is enabled, since an
// understated estimate would let the request pass a hard budget check.
func estimateInputTokens(req *adkmodel.LLMRequest, strict bool) (int64, error) {
	if req == nil {
		return 0, nil
	}
	chars := 0
	for _, content := range req.Contents {
		if content == nil {
			continue
		}
		for _, part := range content.Parts {
			n, err := partSize(part, strict)
			if err != nil {
				return 0, err
			}
			chars += n
		}
	}
	return int64((chars + 3) / 4), nil
}

func partSize(part *genai.Part, strict bool) (int, error) {
	if part == nil {
		return 0, nil
	}
	size := len(part.Text)
	if part.FunctionCall != nil {
		size += jsonSize(part.FunctionCall)
	}
	if part.FunctionResponse != nil {
		size += jsonSize(part.FunctionResponse)
	}
	if part.InlineData != nil {
		size += len(part.InlineData.Data)
	}
	if part.ExecutableCode != nil {
		size += len(part.ExecutableCode.Code)
	}
	if part.CodeExecutionResult != nil {
		size += len(part.CodeExecutionResult.Output)
	}
	if part.ToolCall != nil {
		size += jsonSize(part.ToolCall)
	}
	if part.ToolResponse != nil {
		size += jsonSize(part.ToolResponse)
	}
	size += len(part.ThoughtSignature)
	if part.PartMetadata != nil {
		size += jsonSize(part.PartMetadata)
	}
	if part.FileData != nil {
		if strict {
			return 0, codedError(ErrorCodeInvalidArgument,
				"usage: file_data input cannot be conservatively reserved; its size is unknown at reserve time", nil)
		}
		// No cap is enforced, so an unpriced part cannot bypass a budget; the
		// output bound still reserves a non-zero amount.
	}
	return size, nil
}

func jsonSize(v any) int {
	if data, err := json.Marshal(v); err == nil {
		return len(data)
	}
	return 0
}

func requestedMaxOutput(req *adkmodel.LLMRequest) int64 {
	if req != nil && req.Config != nil && req.Config.MaxOutputTokens > 0 {
		return int64(req.Config.MaxOutputTokens)
	}
	return defaultMaxOutputBound
}
