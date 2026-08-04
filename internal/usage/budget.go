package usage

import (
	"context"
	"encoding/json"
	"iter"

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
// conservatively at the reserved amount, never at zero.
type Budgeted struct {
	inner             adkmodel.LLM
	ledger            *Ledger
	modelDefinitionID string
}

// NewBudgeted wraps inner with budget enforcement for the given model
// definition. A nil ledger disables enforcement and returns inner unchanged,
// so an unconfigured deployment adds no accounting.
func NewBudgeted(inner adkmodel.LLM, ledger *Ledger, modelDefinitionID string) (adkmodel.LLM, error) {
	if inner == nil {
		return nil, codedError(ErrorCodeInvalidArgument, "usage: inner model must not be nil", nil)
	}
	if ledger == nil {
		return inner, nil
	}
	if modelDefinitionID == "" {
		return nil, codedError(ErrorCodeInvalidArgument, "usage: model definition id must not be empty", nil)
	}
	return &Budgeted{inner: inner, ledger: ledger, modelDefinitionID: modelDefinitionID}, nil
}

func (b *Budgeted) Name() string {
	return b.inner.Name()
}

func (b *Budgeted) GenerateContent(ctx context.Context, req *adkmodel.LLMRequest, stream bool) iter.Seq2[*adkmodel.LLMResponse, error] {
	return func(yield func(*adkmodel.LLMResponse, error) bool) {
		invocationID, err := randomID()
		if err != nil {
			yield(nil, err)
			return
		}
		reservation, err := b.ledger.Reserve(ctx, ReserveRequest{
			InvocationID:             invocationID,
			Attempt:                  0,
			ModelDefinitionID:        b.modelDefinitionID,
			KnownInputTokens:         estimateInputTokens(req),
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
				// attempt must never release its reservation at zero.
				_, _ = b.ledger.Settle(ctx, &SettleRequest{ReservationID: reservation.ID})
				yield(resp, err)
				return
			}
			if resp != nil {
				final = resp
			}
			if !yield(resp, nil) {
				_, _ = b.ledger.Settle(ctx, settleForResponse(reservation.ID, final))
				return
			}
		}
		_, _ = b.ledger.Settle(ctx, settleForResponse(reservation.ID, final))
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

// estimateInputTokens derives a conservative input token estimate from the
// request contents. It approximates four characters per token; the estimate
// only bounds the reservation, which the settlement releases.
func estimateInputTokens(req *adkmodel.LLMRequest) int64 {
	if req == nil {
		return 0
	}
	chars := 0
	for _, content := range req.Contents {
		if content == nil {
			continue
		}
		for _, part := range content.Parts {
			chars += partSize(part)
		}
	}
	return int64((chars + 3) / 4)
}

func partSize(part *genai.Part) int {
	if part == nil {
		return 0
	}
	size := len(part.Text)
	if part.FunctionCall != nil {
		if data, err := json.Marshal(part.FunctionCall); err == nil {
			size += len(data)
		}
	}
	if part.FunctionResponse != nil {
		if data, err := json.Marshal(part.FunctionResponse); err == nil {
			size += len(data)
		}
	}
	return size
}

func requestedMaxOutput(req *adkmodel.LLMRequest) int64 {
	if req != nil && req.Config != nil && req.Config.MaxOutputTokens > 0 {
		return int64(req.Config.MaxOutputTokens)
	}
	return defaultMaxOutputBound
}
