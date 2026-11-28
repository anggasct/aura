package runtimeadk

import (
	"sync/atomic"

	auratools "github.com/anggasct/aura/internal/platform/tools"

	"google.golang.org/adk/v2/tool"
)

type OperationPort struct {
	bound atomic.Pointer[operationBinding]
}

type operationBinding struct {
	surface   OperationSurface
	executor  BuiltinToolExecutor
	tools     map[string]tool.Tool
	order     []string
	generated map[string]bool
}

func NewOperationPort() *OperationPort {
	return &OperationPort{}
}

func WithOperations(port *OperationPort) ExecutorOption {
	return func(e *ADKExecutor) error {
		if port == nil {
			return invalidArgument("operation port must not be nil")
		}
		e.operations = port
		return nil
	}
}

func (p *OperationPort) Bind(surface OperationSurface, executor BuiltinToolExecutor) error {
	if p == nil {
		return invalidArgument("operation port must not be nil")
	}
	if surface == nil || executor == nil {
		return invalidArgument("operations need a surface and a builtin tool executor")
	}
	var definitions []BuiltinToolDefinition
	all := cloneBuiltinDefinitions(executor.Definitions())
	for i := range all {
		if isOperationDefinition(&all[i]) {
			definitions = append(definitions, all[i])
		}
	}
	if len(definitions) == 0 {
		return invalidArgument("the builtin tool executor registers no operation tool")
	}
	built, err := buildBuiltinTools(definitions)
	if err != nil {
		return err
	}
	binding := &operationBinding{
		surface:   surface,
		executor:  executor,
		tools:     make(map[string]tool.Tool, len(built)),
		generated: map[string]bool{},
	}
	for i := range definitions {
		name := definitions[i].Name
		binding.tools[name] = built[i]
		binding.order = append(binding.order, name)
		binding.generated[name] = definitions[i].Operation != ""
	}
	if !p.bound.CompareAndSwap(nil, binding) {
		return invalidArgument("the operation port is already bound")
	}
	return nil
}

func isOperationDefinition(definition *BuiltinToolDefinition) bool {
	return definition.Operation != "" || definition.Name == auratools.OperationSearchToolName
}

func (b *operationBinding) owns(name string) bool {
	_, owned := b.tools[name]
	return owned
}

func (b *operationBinding) generates(name string) bool {
	return b.generated[name]
}
