package operation_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/operation"
	"github.com/anggasct/aura/internal/platform/operation/operationtest"
	"github.com/anggasct/aura/internal/platform/tools"
)

const shippedSchema = `{"$schema":"https://json-schema.org/draft/2020-12/schema","$id":"aura://tools/legacy_start/v1","type":"object","properties":{"id":{"type":"string","minLength":1},"objective":{"type":"string","maxLength":2000},"mode":{"type":"string","enum":["fast","safe"]}},"required":["id"],"additionalProperties":false}`

func shippedTool(schema string, untrusted bool) tools.Definition {
	return tools.Definition{
		Name: "legacy_start", Version: tools.Version, Schema: json.RawMessage(schema), UntrustedOutput: untrusted,
		Validator: func(raw json.RawMessage) (json.RawMessage, error) { return raw, nil },
	}
}

func projectingDescriptor() operation.Descriptor {
	return operation.Descriptor{
		ID: "wf.start", Summary: "start a run", Exposure: operation.ExposureAgent, Risk: operation.RiskChange, Commit: operation.CommitTransactional,
		ModuleTool: "legacy_start",
		Params: []operation.Param{
			{Name: "id", Kind: operation.KindString, Required: true, Position: 1, MinLength: 1},
			{Name: "objective", Kind: operation.KindString, MaxLength: 2000},
			{Name: "mode", Kind: operation.KindEnum, Enum: []operation.EnumValue{{Value: "fast"}, {Value: "safe"}, {Value: "reckless", Surfaces: []operation.Surface{operation.SurfaceCLI}}}},
			{Name: "resources", Kind: operation.KindJSON, Schema: json.RawMessage(`{"type":"object"}`), Surfaces: []operation.Surface{operation.SurfaceCLI}},
		},
		Output: operation.Output{Kind: operation.OutputRecord},
	}
}

func moduleToolRig(t *testing.T, shipped tools.Definition, mutate func(d *operation.Descriptor)) *operationtest.Rig {
	t.Helper()
	descriptor := projectingDescriptor()
	if mutate != nil {
		mutate(&descriptor)
	}
	return operationtest.NewRig(t, &operationtest.Options{
		AllowInvalid: true,
		Tools:        operationtest.LegacyTools(&shipped),
		Catalogs:     []operation.Catalog{{Group: operation.Group{Name: "wf", Summary: "workflows"}, Descriptors: []operation.Descriptor{descriptor}}},
		Bindings: func(*operationtest.Stubs) []operation.Binding {
			return []operation.Binding{{ID: "wf.start", Handle: func(context.Context, *operation.Call) (operation.Result, error) {
				return operation.Result{}, nil
			}}}
		},
	})
}

func TestModuleToolProjectionMismatchIsRejected(t *testing.T) {
	cases := []struct {
		name    string
		schema  string
		mutate  func(d *operation.Descriptor)
		wantErr bool
	}{
		{name: "a projection that matches the shipped schema", schema: shippedSchema},
		{name: "another maximum length", schema: shippedSchema, mutate: func(d *operation.Descriptor) { d.Params[1].MaxLength = 100 }, wantErr: true},
		{name: "a missing minimum length", schema: shippedSchema, mutate: func(d *operation.Descriptor) { d.Params[0].MinLength = 0 }, wantErr: true},
		{name: "a parameter the shipped schema lacks", schema: shippedSchema, mutate: func(d *operation.Descriptor) {
			d.Params = append(d.Params, operation.Param{Name: "extra", Kind: operation.KindBoolean})
		}, wantErr: true},
		{name: "a property the operation does not declare for the agent", schema: shippedSchema, mutate: func(d *operation.Descriptor) {
			d.Params[1].Surfaces = []operation.Surface{operation.SurfaceCLI}
		}, wantErr: true},
		{name: "another required set", schema: shippedSchema, mutate: func(d *operation.Descriptor) { d.Params[1].Required = true }, wantErr: true},
		{name: "another enum", schema: shippedSchema, mutate: func(d *operation.Descriptor) {
			d.Params[2].Enum = []operation.EnumValue{{Value: "fast"}}
		}, wantErr: true},
		{name: "another type", schema: shippedSchema, mutate: func(d *operation.Descriptor) { d.Params[1].Kind = operation.KindInteger }, wantErr: true},
		{name: "another bound", schema: `{"type":"object","properties":{"id":{"type":"string","minLength":1},"objective":{"type":"integer","minimum":1}},"required":["id"]}`, mutate: func(d *operation.Descriptor) {
			d.Params = d.Params[:2]
			d.Params[1] = operation.Param{Name: "objective", Kind: operation.KindInteger, Minimum: int64Ptr(2)}
		}, wantErr: true},
		{name: "another untrusted output setting", schema: shippedSchema, mutate: func(d *operation.Descriptor) { d.UntrustedOutput = true }, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rig := moduleToolRig(t, shippedTool(tc.schema, false), tc.mutate)
			switch {
			case tc.wantErr && !fault.HasCode(rig.RegistryErr, operation.ErrorCodeInvalid):
				t.Fatalf("NewRegistry = %v, want operation_invalid", rig.RegistryErr)
			case !tc.wantErr && rig.RegistryErr != nil:
				t.Fatalf("NewRegistry = %v, want the projection accepted", rig.RegistryErr)
			}
		})
	}
	t.Run("a module tool that is not registered", func(t *testing.T) {
		rig := operationtest.NewRig(t, &operationtest.Options{
			AllowInvalid: true,
			Catalogs:     []operation.Catalog{{Group: operation.Group{Name: "wf", Summary: "workflows"}, Descriptors: []operation.Descriptor{projectingDescriptor()}}},
			Bindings: func(*operationtest.Stubs) []operation.Binding {
				return []operation.Binding{{ID: "wf.start", Handle: func(context.Context, *operation.Call) (operation.Result, error) { return operation.Result{}, nil }}}
			},
		})
		if !fault.HasCode(rig.RegistryErr, operation.ErrorCodeInvalid) {
			t.Fatalf("NewRegistry = %v, want operation_invalid", rig.RegistryErr)
		}
	})
}

func TestModuleToolProjectionGeneratesNoTool(t *testing.T) {
	rig := moduleToolRig(t, shippedTool(shippedSchema, false), nil)
	if rig.RegistryErr != nil {
		t.Fatalf("NewRegistry = %v", rig.RegistryErr)
	}
	if registrations := rig.Registry.Tools(); len(registrations) != 0 {
		t.Errorf("generated tools = %d, want none: the shipped module tool is the projection", len(registrations))
	}
	descriptor, found := rig.Registry.Descriptor("wf.start")
	if !found || descriptor.GeneratesTool() {
		t.Fatalf("descriptor found = %v, GeneratesTool = %v", found, found && descriptor.GeneratesTool())
	}
	if !descriptor.Projects(operation.SurfaceAgent) {
		t.Error("the module tool projection is not an agent projection")
	}
}
