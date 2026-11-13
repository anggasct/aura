package workflow

import "context"

type StepCall struct {
	RunID             string
	StepID            string
	Attempt           int
	Origin            RunOrigin
	DefinitionDigest  string
	DefinitionID      string
	DefinitionVersion int
	Approval          Approval
}

type stepCallContextKey struct{}

func WithStepCall(ctx context.Context, call *StepCall) context.Context {
	return context.WithValue(ctx, stepCallContextKey{}, *call)
}

func StepCallFrom(ctx context.Context) (StepCall, bool) {
	call, ok := ctx.Value(stepCallContextKey{}).(StepCall)
	return call, ok
}
