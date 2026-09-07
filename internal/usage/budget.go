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

const defaultMaxOutputBound = 4096

type Budgeted struct {
	inner             adkmodel.LLM
	ledger            *Ledger
	modelDefinitionID string
	logger            *slog.Logger
	attempts          atomic.Int64
}

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
			yield(nil, err)
			return
		}

		var final *adkmodel.LLMResponse
		for resp, err := range b.inner.GenerateContent(ctx, req, stream) {
			if err != nil {
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
				if _, settleErr := b.ledger.Settle(ctx, &SettleRequest{ReservationID: reservation.ID}); settleErr != nil {
					b.logger.WarnContext(ctx, "usage settlement failed after consumer stop",
						"component", "usage", "reservation_id", reservation.ID, "error", settleErr)
				}
				return
			}
		}
		settleReq := &SettleRequest{ReservationID: reservation.ID}
		if final != nil && final.TurnComplete {
			settleReq = settleForResponse(reservation.ID, final)
		}
		if _, settleErr := b.ledger.Settle(ctx, settleReq); settleErr != nil {
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
