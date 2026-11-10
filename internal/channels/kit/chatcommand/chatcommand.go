package chatcommand

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/anggasct/aura/internal/platform/command"
	"github.com/anggasct/aura/internal/platform/effect"
	runtimeingress "github.com/anggasct/aura/internal/platform/turn/ingress"
)

const Provider = "command"

type ErrorCode string

const (
	ErrorCodeInvalidArgument ErrorCode = "invalid_argument"
	ErrorCodeReceiptInvalid  ErrorCode = "receipt_invalid"
)

type Error struct {
	Code   ErrorCode
	Detail string
}

func (e *Error) Error() string {
	return fmt.Sprintf("%s: %s", e.Code, e.Detail)
}

func Errorf(code ErrorCode, format string, args ...any) error {
	return &Error{Code: code, Detail: fmt.Sprintf(format, args...)}
}

type Effects interface {
	Execute(ctx context.Context, req *effect.PrepareRequest, provider effect.Provider) (*effect.Intent, error)
}

type Request struct {
	Command      string `json:"command"`
	Conversation string `json:"conversation"`
	Target       string `json:"target,omitempty"`
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
		return Outcome{}, Errorf(ErrorCodeInvalidArgument, "command run is missing a dependency")
	}
	inv := run.Invocation
	def, ok := run.Dispatcher.Registry().Lookup(inv.Name)
	if !ok {
		return Outcome{}, Errorf(ErrorCodeInvalidArgument, "command is not registered")
	}
	request := Request{Command: def.Name, Conversation: run.Caller.ConversationKey}
	if def.Name == "session" && len(inv.Args) > 0 {
		request.Target = inv.Args[0]
	}
	raw, err := json.Marshal(request)
	if err != nil {
		return Outcome{}, Errorf(ErrorCodeInvalidArgument, "command request is not serializable")
	}
	provider := &commandProvider{run: func(ctx context.Context) command.Result {
		return run.Dispatcher.Execute(ctx, run.Caller, inv)
	}}
	intent, err := run.Effects.Execute(ctx, &effect.PrepareRequest{
		SessionID:      run.EffectSession,
		IdempotencyKey: run.IdempotencyKey,
		Provider:       Provider,
		Operation:      def.Name,
		Classification: effect.ClassificationEffectful,
		Request:        raw,
		EventKind:      effect.EventKindChannelRequested,
	}, provider)
	if err != nil {
		return Outcome{}, fmt.Errorf("command effect: %w", err)
	}
	result := command.ResultReplayed
	if provider.ran {
		result = command.ResultOf(command.ErrorCode(provider.code))
	} else {
		run.Dispatcher.Replayed(ctx, run.Caller, inv)
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
	run  func(ctx context.Context) command.Result
	ran  bool
	code string
}

func (p *commandProvider) SupportsIdempotency() bool { return false }

func (p *commandProvider) Invoke(ctx context.Context, _ *effect.Invocation) (effect.Outcome, error) {
	result := p.run(ctx)
	p.ran = true
	p.code = string(result.Code)
	var skill *runtimeingress.SkillPin
	if result.Turn != nil {
		skill = result.Turn.Skill
	}
	receipt, err := json.Marshal(Receipt{Lines: result.Lines, Code: p.code, Skill: skill})
	if err != nil {
		return effect.Outcome{}, Errorf(ErrorCodeReceiptInvalid, "command receipt is not serializable")
	}
	return effect.Outcome{Succeeded: true, Receipt: receipt}, nil
}
