package chatcommand

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/anggasct/aura/internal/platform/command"
	"github.com/anggasct/aura/internal/platform/effect"
	"github.com/anggasct/aura/internal/platform/fault"
	runtimeingress "github.com/anggasct/aura/internal/platform/turn/ingress"
)

const (
	Provider            = "command"
	conflictReplyPrefix = "conflict:"
)

type ErrorCode string

const (
	ErrorCodeInvalidArgument ErrorCode = "invalid_argument"
	ErrorCodeReceiptInvalid  ErrorCode = "receipt_invalid"
)

type Effects interface {
	Execute(ctx context.Context, req *effect.PrepareRequest, provider effect.Provider) (*effect.Intent, error)
}

type Request struct {
	Command      string          `json:"command"`
	Conversation string          `json:"conversation"`
	Target       string          `json:"target,omitempty"`
	Operation    string          `json:"operation,omitempty"`
	Input        json.RawMessage `json:"input,omitempty"`
}

type Receipt struct {
	Lines []string                 `json:"lines"`
	Code  string                   `json:"code,omitempty"`
	Skill *runtimeingress.SkillPin `json:"skill,omitempty"`
}

type Run struct {
	Effects        Effects
	Dispatcher     *command.Dispatcher
	Logger         *slog.Logger
	Caller         *command.Caller
	Invocation     *command.Invocation
	IdempotencyKey string
	EffectSession  string
}

type Outcome struct {
	IntentID string
	Receipt  *Receipt
}

func Execute(ctx context.Context, run *Run) (Outcome, error) {
	if run == nil || run.Effects == nil || run.Dispatcher == nil || run.Logger == nil || run.Caller == nil || run.Invocation == nil {
		return Outcome{}, fault.New(ErrorCodeInvalidArgument, "command run is missing a dependency")
	}
	inv := *run.Invocation
	inv.IdempotencyKey = run.IdempotencyKey
	def, ok := run.Dispatcher.Registry().Lookup(inv.Name)
	if !ok {
		return Outcome{}, fault.New(ErrorCodeInvalidArgument, "command is not registered")
	}
	request := Request{Command: def.Name, Conversation: run.Caller.ConversationKey}
	if def.Name == "session" && len(inv.Args) > 0 {
		request.Target = inv.Args[0]
	}
	operationName := def.Name
	ref, isOperation := run.Dispatcher.Operation(run.Caller, &inv)
	if isOperation {
		request.Operation, request.Input = ref.ID, ref.Input
		operationName = ref.ID
	}
	raw, err := json.Marshal(request)
	if err != nil {
		return Outcome{}, fault.New(ErrorCodeInvalidArgument, "command request is not serializable")
	}
	provider := &commandProvider{takeover: isOperation, run: func(ctx context.Context) command.Result {
		return run.Dispatcher.Execute(ctx, run.Caller, &inv)
	}}
	intent, err := run.Effects.Execute(ctx, &effect.PrepareRequest{
		SessionID:      run.EffectSession,
		IdempotencyKey: run.IdempotencyKey,
		Provider:       Provider,
		Operation:      operationName,
		Classification: effect.ClassificationEffectful,
		Request:        raw,
		EventKind:      effect.EventKindChannelRequested,
	}, provider)
	if fault.HasCode(err, effect.ErrorCodeIdempotencyConflict) {
		run.Logger.WarnContext(ctx, "chat command conflicts with its earlier request", "component", "command", "session_id", run.EffectSession)
		return Outcome{IntentID: conflictReplyPrefix + run.IdempotencyKey, Receipt: &Receipt{Lines: []string{command.ReplyConflict}, Code: string(command.ErrorCodeConflict)}}, nil
	}
	if err != nil {
		return Outcome{}, fmt.Errorf("command effect: %w", err)
	}
	if !provider.ran && isOperation && intent != nil && (intent.State == effect.StateStarted || intent.State == effect.StateUnknown) {
		return resumeOperation(ctx, run, &resumption{inv: &inv, intent: intent, provider: provider})
	}
	result := command.ResultReplayed
	if provider.ran {
		result = command.ResultOf(command.ErrorCode(provider.code))
	} else {
		run.Dispatcher.Replayed(ctx, run.Caller, &inv)
	}
	logResult(ctx, run.Logger, run.Caller, run.Dispatcher.Registry().MetricName(run.Caller.Channel, inv.Name), result, run.EffectSession)
	if intent == nil || intent.State != effect.StateSucceeded {
		return Outcome{}, nil
	}
	receipt, ok := decodeReceipt(intent.ProviderReceipt)
	if !ok {
		run.Logger.WarnContext(ctx, "command receipt unreadable", "component", "command", "session_id", run.EffectSession)
		return Outcome{}, nil
	}
	return Outcome{IntentID: intent.ID, Receipt: &receipt}, nil
}

type resumption struct {
	inv      *command.Invocation
	intent   *effect.Intent
	provider *commandProvider
}

func resumeOperation(ctx context.Context, run *Run, resumed *resumption) (Outcome, error) {
	inv, intent := resumed.inv, resumed.intent
	resumed.provider.resuming = true
	recovery := &command.OperationRecovery{
		Provider: resumed.provider,
		Settle: func(lines []string, record json.RawMessage) (effect.Outcome, error) {
			receipt, err := json.Marshal(Receipt{Lines: lines})
			if err != nil {
				return effect.Outcome{}, fault.New(ErrorCodeReceiptInvalid, "command receipt is not serializable")
			}
			return effect.Outcome{Succeeded: true, Receipt: receipt, Result: record}, nil
		},
	}
	result, replayed := run.Dispatcher.Resume(ctx, &command.ResumeRequest{Caller: run.Caller, Invocation: inv, Intent: intent, Recovery: recovery})
	name := run.Dispatcher.Registry().MetricName(run.Caller.Channel, inv.Name)
	if replayed {
		logResult(ctx, run.Logger, run.Caller, name, command.ResultReplayed, run.EffectSession)
		return Outcome{}, nil
	}
	logResult(ctx, run.Logger, run.Caller, name, command.ResultOf(result.Code), run.EffectSession)
	if len(result.Lines) == 0 {
		return Outcome{}, nil
	}
	return Outcome{IntentID: intent.ID, Receipt: &Receipt{Lines: result.Lines, Code: string(result.Code)}}, nil
}

func decodeReceipt(raw json.RawMessage) (Receipt, bool) {
	var receipt Receipt
	if json.Unmarshal(raw, &receipt) != nil || (len(receipt.Lines) == 0 && receipt.Skill == nil) {
		return Receipt{}, false
	}
	return receipt, true
}

func logResult(ctx context.Context, logger *slog.Logger, caller *command.Caller, name, result, sessionID string) {
	attrs := []any{
		"component", "command",
		"name", name,
		"channel", caller.Channel,
		"class", string(caller.Class),
		"result", result,
		"session_id", sessionID,
	}
	switch result {
	case command.ResultDenied, command.ResultUnavailable, command.ResultFailed:
		logger.WarnContext(ctx, "chat command", attrs...)
	default:
		logger.InfoContext(ctx, "chat command", attrs...)
	}
}

type commandProvider struct {
	run      func(ctx context.Context) command.Result
	ran      bool
	code     string
	takeover bool
	resuming bool
}

func (p *commandProvider) SupportsIdempotency() bool { return false }

func (p *commandProvider) SupportsTakeover() bool { return p.takeover }

func (p *commandProvider) Invoke(ctx context.Context, _ *effect.Invocation) (effect.Outcome, error) {
	result := p.run(ctx)
	p.ran = true
	p.code = string(result.Code)
	if p.resuming {
		switch result.Fate {
		case command.FatePerformed:
		case command.FateNotPerformed:
			return effect.Outcome{SafeErrorCode: p.code}, nil
		case command.FateAmbiguous:
			return effect.Outcome{Ambiguous: true}, nil
		}
	}
	var skill *runtimeingress.SkillPin
	if result.Turn != nil {
		skill = result.Turn.Skill
	}
	receipt, err := json.Marshal(Receipt{Lines: result.Lines, Code: p.code, Skill: skill})
	if err != nil {
		return effect.Outcome{}, fault.New(ErrorCodeReceiptInvalid, "command receipt is not serializable")
	}
	return effect.Outcome{Succeeded: true, Receipt: receipt, Result: result.Record}, nil
}
