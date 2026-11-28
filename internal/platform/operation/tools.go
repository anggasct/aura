package operation

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/anggasct/aura/internal/platform/approval"
	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/principal"
	"github.com/anggasct/aura/internal/platform/toolbroker"
	"github.com/anggasct/aura/internal/platform/tools"
)

func (r *Registry) buildTools() error {
	for _, id := range r.ids {
		found := r.entries[id]
		descriptor := found.descriptor
		if descriptor.ModuleTool != "" {
			r.byTool[descriptor.ModuleTool] = found
		}
		if !descriptor.GeneratesTool() {
			continue
		}
		definition, err := r.definition(descriptor)
		if err != nil {
			return err
		}
		registration := toolbroker.ToolRegistration{
			Definition: definition,
			Adapter:    r.adapter(found),
			Rule:       toolbroker.DefaultRule(&definition),
		}
		if descriptor.Preview {
			registration.Preview = r.previewer(found)
		}
		if definition.Effectful {
			registration.Recovery = r.recovery(found)
		}
		stored := definition
		found.definition = &stored
		r.byTool[definition.Name] = found
		r.registrations = append(r.registrations, registration)
	}
	return nil
}

func (r *Registry) definition(descriptor *Descriptor) (tools.Definition, error) {
	schema, err := descriptor.SchemaFor(SurfaceAgent)
	if err != nil {
		return tools.Definition{}, err
	}
	display := descriptor.Display
	if len(display) == 0 {
		for _, param := range descriptor.paramsOn(SurfaceAgent) {
			display = append(display, param.Name)
		}
	}
	description := descriptor.Summary
	if descriptor.Description != "" {
		description += "\n\n" + descriptor.Description
	}
	definition := tools.Definition{
		Name:                 ToolName(descriptor.ID),
		Version:              tools.Version,
		Schema:               schema,
		RequiredCapabilities: append([]string(nil), descriptor.RequiredCapabilities...),
		ExternalSink:         descriptor.Sink,
		Risk:                 tools.Risk(descriptor.Risk),
		Operation:            descriptor.ID,
		FreshApproval:        descriptor.FreshApproval,
		OwnerNotice:          descriptor.OwnerNotice,
		UntrustedOutput:      descriptor.UntrustedOutput,
		Description:          description,
		Display:              append([]string(nil), display...),
		Validator: func(raw json.RawMessage) (json.RawMessage, error) {
			return descriptor.Validate(SurfaceAgent, raw)
		},
	}
	if err := definition.ApplyOperationFlags(); err != nil {
		return tools.Definition{}, wrapError(ErrorCodeInvalid, descriptor.ID+": the tool definition is invalid", err)
	}
	return definition, nil
}

func agentCall(request *toolbroker.ToolRequest) *Call {
	return &Call{
		Caller: Caller{
			Surface:     SurfaceAgent,
			PrincipalID: request.PrincipalID,
			Class:       classOf(request),
			SessionID:   request.SessionID,
			TurnID:      request.TurnID,
			AgentID:     request.AgentID,
			Origin:      string(request.Origin),
			Tainted:     request.Tainted,
		},
		Input:          request.Arguments,
		IdempotencyKey: request.IdempotencyKey,
		RequestDigest:  request.RequestDigest,
		PreviewDigest:  request.PreviewDigest,
	}
}

func classOf(request *toolbroker.ToolRequest) principal.Class {
	if request.PrincipalClass == "" {
		return principal.ClassOwner
	}
	return principal.ParseClass(string(request.PrincipalClass))
}

func (r *Registry) adapter(found *entry) toolbroker.Adapter {
	return func(ctx context.Context, request *toolbroker.ToolRequest, _ approval.Constraints) (toolbroker.ToolResult, error) {
		if request == nil {
			return toolbroker.ToolResult{}, nilArgument("tool request")
		}
		result, info, err := r.run(ctx, found, agentCall(request))
		if err != nil {
			return toolbroker.ToolResult{}, brokerError(err, info)
		}
		return toolbroker.ToolResult{Class: toolbroker.ResultOK, Output: result.Data}, nil
	}
}

func (r *Registry) previewer(found *entry) toolbroker.Previewer {
	return func(ctx context.Context, request *toolbroker.ToolRequest) (toolbroker.Preview, error) {
		if request == nil {
			return toolbroker.Preview{}, nilArgument("tool request")
		}
		preview, err := r.preview(ctx, found, agentCall(request))
		if err != nil {
			return toolbroker.Preview{}, brokerError(err, runInfo{})
		}
		return toolbroker.Preview{Lines: preview.Lines, Digest: preview.Digest, Empty: preview.Empty}, nil
	}
}

func deniedAtTier(code ErrorCode) bool {
	return code == ErrorCodeNotExposed || code == ErrorCodeDenied || code == ErrorCodeUnknown
}

func brokerError(err error, info runInfo) error {
	mapped := mapToBroker(err)
	if !info.notPerformed || interrupted(err) {
		return mapped
	}
	code := info.code
	if code == "" {
		own, _ := fault.CodeOf[ErrorCode](err)
		code = string(own)
	}
	return toolbroker.NotPerformed(code, mapped)
}

func mapToBroker(err error) error {
	if interrupted(err) {
		return err
	}
	var failure *fault.Error[ErrorCode]
	if !errors.As(err, &failure) {
		return toolbroker.Errorf(toolbroker.ResultExecutionFailed, "%s", string(ErrorCodeFailed))
	}
	detail := safeText(err)
	switch {
	case failure.Code == ErrorCodeInputInvalid:
		return toolbroker.Errorf(toolbroker.ResultInvalidArgument, "%s", detail)
	case failure.Code == ErrorCodePolicyDenied:
		return toolbroker.Errorf(toolbroker.ResultPolicyDenied, "%s", detail)
	case deniedAtTier(failure.Code):
		return toolbroker.Errorf(toolbroker.ResultToolTierDenied, "%s", detail)
	}
	return toolbroker.Errorf(toolbroker.ResultExecutionFailed, "%s", detail)
}
