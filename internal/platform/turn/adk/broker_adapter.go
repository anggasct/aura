package runtimeadk

import (
	"cmp"
	"context"
	"encoding/json"
	"slices"

	"github.com/anggasct/aura/internal/platform/approval"
	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/toolbroker"
	auratools "github.com/anggasct/aura/internal/platform/tools"
	"github.com/anggasct/aura/internal/platform/turn"
)

type BrokerExecutor struct {
	broker *toolbroker.Broker
	host   []string
}

var (
	_ BuiltinToolExecutor = (*BrokerExecutor)(nil)
	_ turn.ToolBroker     = (*BrokerExecutor)(nil)
)

func NewBrokerExecutor(broker *toolbroker.Broker, hostCapabilities []string) (*BrokerExecutor, error) {
	if broker == nil {
		return nil, invalidArgument("tool broker must not be nil")
	}
	return &BrokerExecutor{broker: broker, host: slices.Clone(hostCapabilities)}, nil
}

func (e *BrokerExecutor) Broker() *toolbroker.Broker {
	if e == nil {
		return nil
	}
	return e.broker
}

func (e *BrokerExecutor) Definitions() []BuiltinToolDefinition {
	if e == nil || e.broker == nil {
		return nil
	}
	definitions := e.broker.Definitions()
	result := make([]BuiltinToolDefinition, 0, len(definitions))
	for i := range definitions {
		definition := &definitions[i]
		if !e.offers(definition) {
			continue
		}
		description := cmp.Or(definition.Description, "Aura built-in "+definition.Name+" tool")
		plugin := ""
		if definition.Server != "" {
			description = "Tool " + definition.Name + " provided by the external server " + definition.Server
		}
		if definition.Plugin != nil {
			plugin = definition.Plugin.ID
			description = definition.Plugin.Description
		}
		result = append(result, BuiltinToolDefinition{
			Name:                 definition.Name,
			Version:              definition.Version,
			Channel:              definition.Channel,
			Server:               definition.Server,
			Plugin:               plugin,
			Description:          description,
			Schema:               definition.Schema,
			RequiredCapabilities: definition.RequiredCapabilities,
			OptIn:                definition.OptIn,
			OwnerOptIn:           definition.OwnerOptIn,
			Operation:            definition.Operation,
		})
	}
	return result
}

func (e *BrokerExecutor) offers(definition *auratools.Definition) bool {
	for _, required := range definition.RequiredCapabilities {
		if !slices.Contains(e.host, required) {
			return false
		}
	}
	return true
}

func (e *BrokerExecutor) Evaluate(ctx context.Context, request *approval.ToolRequest) (approval.PolicyDecision, error) {
	if e == nil || e.broker == nil {
		return approval.PolicyDecision{}, approval.Errorf(approval.ErrorCodeInvalidArgument, "tool broker must not be nil")
	}
	if request == nil {
		return approval.PolicyDecision{}, approval.Errorf(approval.ErrorCodeInvalidArgument, "tool request must not be nil")
	}
	return e.broker.Evaluate(ctx, &toolbroker.ToolRequest{
		RequestID: request.RequestID, TurnID: request.TurnID, SessionID: request.SessionID,
		PrincipalID: request.PrincipalID, ToolName: request.ToolName, ToolVersion: request.ToolVersion,
		Arguments: request.Arguments, RequestDigest: request.RequestDigest, Capabilities: request.Capabilities,
		Trust: request.Trust, Deadline: request.Deadline, IdempotencyKey: request.IdempotencyKey,
	})
}

func (e *BrokerExecutor) Execute(ctx context.Context, request *BuiltinToolRequest) (json.RawMessage, error) {
	if e == nil || e.broker == nil {
		return nil, invalidArgument("tool broker must not be nil")
	}
	if request == nil {
		return nil, fault.New(turn.ErrorCodeInvalidArgument, "builtin tool request must not be nil")
	}
	result, err := e.broker.Execute(ctx, &toolbroker.ToolRequest{
		RequestID: request.RequestID, TurnID: request.TurnID, SessionID: request.SessionID,
		PrincipalID: request.PrincipalID, PrincipalClass: request.PrincipalClass, AgentID: request.AgentID,
		ToolName: request.ToolName, ToolVersion: request.ToolVersion,
		Arguments: request.Arguments, Capabilities: request.Capabilities, Trust: approval.TrustLabel(request.Trust),
		Deadline: request.Deadline, IdempotencyKey: request.IdempotencyKey,
		AllocateEventSequence: true, EventInvocation: request.EventInvocation,
		EventBranch: request.EventBranch, EventAuthor: request.EventAuthor,
		Knowledge: slices.Clone(request.Knowledge), Tainted: request.Tainted, Unattended: request.Unattended, Origin: request.Origin,
	})
	if err != nil {
		return nil, err
	}
	return result.Output, nil
}
