package command

import (
	"context"
	"errors"
	"slices"
	"strings"
	"unicode"
)

const (
	ReplyDirectOnly            = "That command only works in a direct chat with Aura."
	ReplyOperationsUnavailable = "Operations are not ready yet."
	ReplyCommandTooLong        = "That command is too long."
	ReplyTooManyArguments      = "That command has too many arguments."
	ReplyOpenQuote             = "A quote in that command is not closed."
	ReplyDanglingEscape        = "That command ends with a backslash."
)

type operationCall struct {
	caller  *Caller
	inv     *Invocation
	def     *Definition
	binding *Binding
}

func (d *Dispatcher) operationCallFor(caller *Caller, inv *Invocation) (*operationCall, bool) {
	b, ok := d.registry.binding(caller.Channel)
	if !ok {
		return nil, false
	}
	def, ok := d.registry.Lookup(inv.Name)
	if !ok || def.Kind != KindOperation || !b.names[def.Name] {
		return nil, false
	}
	return &operationCall{caller: caller, inv: inv, def: &def, binding: &b.binding}, true
}

func (d *Dispatcher) planOperation(call *operationCall) (plan OperationPlan, tokens []string) {
	caller, inv, def := call.caller, call.inv, call.def
	refuse := func(code ErrorCode, line string) (OperationPlan, []string) {
		refusal := failure(code, line)
		return OperationPlan{Refusal: &refusal}, nil
	}
	if !permits(def.Audience, caller.Class) {
		return refuse(ErrorCodeDenied, ReplyDenied)
	}
	if !caller.Direct {
		return refuse(ErrorCodeScope, ReplyDirectOnly)
	}
	if d.ports.Operations == nil {
		return refuse(ErrorCodeUnavailable, ReplyOperationsUnavailable)
	}
	tokens, err := operationTokens(inv)
	if err != nil {
		return refuse(ErrorCodeUsage, tokenizeReply(err))
	}
	return d.ports.Operations.Plan(def.Name, tokens), tokens
}

func operationTokens(inv *Invocation) ([]string, error) {
	if inv.Text == "" {
		return slices.Clone(inv.Args), nil
	}
	rest, found := strings.CutPrefix(strings.TrimLeftFunc(inv.Text, unicode.IsSpace), inv.Token)
	if !found {
		return nil, NewUsageError(errTokenizeMissingToken)
	}
	return Tokenize(rest)
}

func tokenizeReply(err error) string {
	switch {
	case errors.Is(err, errTokenizeTooLong):
		return ReplyCommandTooLong
	case errors.Is(err, errTokenizeTooMany):
		return ReplyTooManyArguments
	case errors.Is(err, errTokenizeOpenQuote):
		return ReplyOpenQuote
	case errors.Is(err, errTokenizeDanglingEscape):
		return ReplyDanglingEscape
	}
	return "That command could not be read."
}

func (d *Dispatcher) Operation(caller *Caller, inv *Invocation) (OperationRef, bool) {
	if d == nil || caller == nil || inv == nil {
		return OperationRef{}, false
	}
	call, ok := d.operationCallFor(caller, inv)
	if !ok {
		return OperationRef{}, false
	}
	plan, _ := d.planOperation(call)
	if plan.Refusal != nil || plan.Missing != nil || plan.Operation == "" {
		return OperationRef{}, false
	}
	return OperationRef{ID: plan.Operation, Input: slices.Clone(plan.Input)}, true
}

func (d *Dispatcher) runOperation(ctx context.Context, call *operationCall) Result {
	plan, tokens := d.planOperation(call)
	if plan.Refusal != nil {
		return *plan.Refusal
	}
	if plan.Missing != nil {
		return promptOperation(call, &plan, tokens)
	}
	reply := d.ports.Operations.Run(ctx, &OperationRun{Caller: call.caller, Plan: &plan, IdempotencyKey: call.inv.IdempotencyKey})
	return reply.result()
}

func promptOperation(call *operationCall, plan *OperationPlan, tokens []string) Result {
	if !call.binding.Interactive {
		return failure(ErrorCodeUsage, plan.Missing.Usage)
	}
	args := slices.Clone(tokens)
	if plan.Missing.Flag != "" {
		args = append(args, plan.Missing.Flag)
	}
	pending := Invocation{Name: call.def.Name, Token: call.inv.Token, Args: args}
	return Result{Lines: []string{plan.Missing.Text}, Pending: &pending}
}

func (d *Dispatcher) Resume(ctx context.Context, req *ResumeRequest) (result Result, replayed bool) {
	if d == nil || req == nil || req.Caller == nil || req.Invocation == nil || req.Intent == nil || req.Recovery == nil {
		return failure(ErrorCodeInvalidArgument, "aura: resume needs a caller, an invocation, an intent and a recovery"), false
	}
	call, ok := d.operationCallFor(req.Caller, req.Invocation)
	if !ok {
		return unknown(req.Invocation), false
	}
	ctx, finish := d.start(ctx, call.def.Name, req.Caller)
	plan, _ := d.planOperation(call)
	if plan.Refusal != nil {
		finish(ResultOf(plan.Refusal.Code))
		return *plan.Refusal, false
	}
	if plan.Missing != nil {
		finish(ResultUsage)
		return failure(ErrorCodeUsage, plan.Missing.Usage), false
	}
	reply := d.ports.Operations.Run(ctx, &OperationRun{
		Caller: req.Caller, Plan: &plan, IdempotencyKey: req.Invocation.IdempotencyKey, Intent: req.Intent, Recovery: req.Recovery,
	})
	if reply.InProgress {
		finish(ResultReplayed)
		return Result{}, true
	}
	finish(ResultOf(reply.Code))
	return reply.result(), false
}

func (r *OperationReply) result() Result {
	return Result{Lines: r.Lines, Diagnostic: r.Code != ErrorCodeOK, Code: r.Code, Record: r.Record}
}
