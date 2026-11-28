package operation_test

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/operation"
	"github.com/anggasct/aura/internal/platform/operation/operationtest"
)

func defectCount(err error) int {
	var joined interface{ Unwrap() []error }
	if errors.As(err, &joined) {
		return len(joined.Unwrap())
	}
	if err != nil {
		return 1
	}
	return 0
}

func descriptorNamed(catalog *operation.Catalog, id string) *operation.Descriptor {
	for i := range catalog.Descriptors {
		if catalog.Descriptors[i].ID == id {
			return &catalog.Descriptors[i]
		}
	}
	return nil
}

func TestRegistryRejectsInvalidDescriptors(t *testing.T) {
	long := strings.Repeat("x", 130)
	manyParams := func(d *operation.Descriptor) {
		d.Params = nil
		for i := range 17 {
			d.Params = append(d.Params, operation.Param{Name: "p" + string(rune('a'+i)), Kind: operation.KindBoolean})
		}
	}
	cases := []struct {
		name   string
		mutate func(c *operation.Catalog)
		want   int
	}{
		{name: "an id outside the pattern", mutate: func(c *operation.Catalog) { descriptorNamed(c, operationtest.Change).ID = "Stub.Change" }, want: 1},
		{name: "an id outside its group", mutate: func(c *operation.Catalog) { descriptorNamed(c, operationtest.Change).ID = "other.change" }, want: 1},
		{name: "a repeated id", mutate: func(c *operation.Catalog) {
			c.Descriptors = append(c.Descriptors, *descriptorNamed(c, operationtest.Host))
			c.Descriptors[len(c.Descriptors)-1].Params = nil
		}, want: 1},
		{name: "a missing exposure", mutate: func(c *operation.Catalog) { descriptorNamed(c, operationtest.Change).Exposure = "" }, want: 1},
		{name: "an unknown risk", mutate: func(c *operation.Catalog) { descriptorNamed(c, operationtest.Change).Risk = "dangerous" }, want: 1},
		{name: "a missing commit class", mutate: func(c *operation.Catalog) { descriptorNamed(c, operationtest.Change).Commit = "" }, want: 1},
		{name: "an unknown commit class", mutate: func(c *operation.Catalog) { descriptorNamed(c, operationtest.Change).Commit = "eventually" }, want: 1},
		{name: "a read that commits", mutate: func(c *operation.Catalog) {
			descriptorNamed(c, operationtest.Read).Commit = operation.CommitTransactional
		}, want: 1},
		{name: "a read with fresh approval", mutate: func(c *operation.Catalog) { descriptorNamed(c, operationtest.Read).FreshApproval = true }, want: 1},
		{name: "an effectful agent operation without a commit class", mutate: func(c *operation.Catalog) {
			descriptorNamed(c, operationtest.Change).Commit = operation.CommitNone
		}, want: 1},
		{name: "an empty summary", mutate: func(c *operation.Catalog) { descriptorNamed(c, operationtest.Read).Summary = "" }, want: 1},
		{name: "a long summary", mutate: func(c *operation.Catalog) { descriptorNamed(c, operationtest.Read).Summary = long }, want: 1},
		{name: "a long description", mutate: func(c *operation.Catalog) {
			descriptorNamed(c, operationtest.Read).Description = strings.Repeat("d", 1025)
		}, want: 1},
		{name: "a parameter name outside the pattern", mutate: func(c *operation.Catalog) { descriptorNamed(c, operationtest.Read).Params[0].Name = "Target" }, want: 1},
		{name: "positions that are not contiguous", mutate: func(c *operation.Catalog) { descriptorNamed(c, operationtest.Read).Params[0].Position = 2 }, want: 1},
		{name: "a rest parameter that is not last", mutate: func(c *operation.Catalog) {
			d := descriptorNamed(c, operationtest.Read)
			d.Params[0].Rest = true
			d.Params = append(d.Params, operation.Param{Name: "later", Kind: operation.KindString, Position: 2})
		}, want: 1},
		{name: "an empty enum", mutate: func(c *operation.Catalog) {
			d := descriptorNamed(c, operationtest.Read)
			d.Params = append(d.Params, operation.Param{Name: "mode", Kind: operation.KindEnum})
		}, want: 1},
		{name: "an enum empty on a surface", mutate: func(c *operation.Catalog) {
			d := descriptorNamed(c, operationtest.Read)
			d.Params = append(d.Params, operation.Param{Name: "mode", Kind: operation.KindEnum, Enum: []operation.EnumValue{{Value: "a", Surfaces: []operation.Surface{operation.SurfaceCLI}}}})
		}, want: 2},
		{name: "an enum value on a surface its parameter lacks", mutate: func(c *operation.Catalog) {
			d := descriptorNamed(c, operationtest.Read)
			d.Params = append(d.Params, operation.Param{Name: "mode", Kind: operation.KindEnum, Surfaces: []operation.Surface{operation.SurfaceCLI},
				Enum: []operation.EnumValue{{Value: "a"}, {Value: "b", Surfaces: []operation.Surface{operation.SurfaceAgent}}}})
		}, want: 1},
		{name: "a default that does not validate", mutate: func(c *operation.Catalog) {
			d := descriptorNamed(c, operationtest.Read)
			d.Params = append(d.Params, operation.Param{Name: "count", Kind: operation.KindInteger, Surfaces: []operation.Surface{operation.SurfaceCLI}, Default: json.RawMessage(`"many"`)})
		}, want: 1},
		{name: "a prompt on an optional parameter", mutate: func(c *operation.Catalog) {
			descriptorNamed(c, operationtest.Read).Params[0].Required = false
			descriptorNamed(c, operationtest.Read).Params[0].Prompt = "which one:"
		}, want: 1},
		{name: "more than sixteen parameters", mutate: func(c *operation.Catalog) { manyParams(descriptorNamed(c, operationtest.Read)) }, want: 1},
		{name: "a secret parameter on an agent operation", mutate: func(c *operation.Catalog) { descriptorNamed(c, operationtest.Read).Params[0].Secret = true }, want: 1},
		{name: "a secret output field on an agent operation", mutate: func(c *operation.Catalog) {
			descriptorNamed(c, operationtest.Read).Output.Fields[0].Secret = true
		}, want: 1},
		{name: "an interactive operation that is not host", mutate: func(c *operation.Catalog) { descriptorNamed(c, operationtest.Read).Interactive = true }, want: 1},
		{name: "an agent trust grant without a preview", mutate: func(c *operation.Catalog) { descriptorNamed(c, operationtest.Grant).Preview = false }, want: 1},
		{name: "a read with a preview", mutate: func(c *operation.Catalog) { descriptorNamed(c, operationtest.Read).Preview = true }, want: 1},
		{name: "a preview-first operation without a preview", mutate: func(c *operation.Catalog) { descriptorNamed(c, operationtest.Change).PreviewFirst = true }, want: 1},
		{name: "a read that is preview-first", mutate: func(c *operation.Catalog) { descriptorNamed(c, operationtest.Read).PreviewFirst = true }, want: 2},
		{name: "a URL parameter on a parameter that is not a path", mutate: func(c *operation.Catalog) {
			d := descriptorNamed(c, operationtest.Read)
			d.Params[0].URLParam = "address"
			d.Params = append(d.Params, operation.Param{Name: "address", Kind: operation.KindString, Surfaces: []operation.Surface{operation.SurfaceChat}})
		}, want: 1},
		{name: "a path whose URL parameter does not exist", mutate: func(c *operation.Catalog) {
			d := descriptorNamed(c, operationtest.Read)
			d.Params = append(d.Params, operation.Param{Name: "file", Kind: operation.KindPath, URLParam: "address", Surfaces: []operation.Surface{operation.SurfaceCLI}})
		}, want: 1},
		{name: "a path whose URL parameter is projected beside it", mutate: func(c *operation.Catalog) {
			d := descriptorNamed(c, operationtest.Read)
			d.Params = append(d.Params,
				operation.Param{Name: "file", Kind: operation.KindPath, URLParam: "address", Surfaces: []operation.Surface{operation.SurfaceCLI}},
				operation.Param{Name: "address", Kind: operation.KindString})
		}, want: 1},
		{name: "a surface text for an unknown surface", mutate: func(c *operation.Catalog) {
			descriptorNamed(c, operationtest.Read).Texts = []operation.SurfaceText{{Surface: "radio", Text: "x"}}
		}, want: 1},
		{name: "two descriptions for one surface", mutate: func(c *operation.Catalog) {
			descriptorNamed(c, operationtest.Read).Texts = []operation.SurfaceText{{Surface: operation.SurfaceCLI, Text: "a"}, {Surface: operation.SurfaceCLI, Text: "b"}}
		}, want: 1},
		{name: "an empty parameter text", mutate: func(c *operation.Catalog) {
			descriptorNamed(c, operationtest.Read).Params[0].Texts = []operation.SurfaceText{{Surface: operation.SurfaceCLI}}
		}, want: 1},
		{name: "a narrowed surface list without a reason", mutate: func(c *operation.Catalog) {
			descriptorNamed(c, operationtest.Read).Surfaces = []operation.Surface{operation.SurfaceCLI}
		}, want: 1},
		{name: "a narrowed list that drops the CLI of an owner operation", mutate: func(c *operation.Catalog) {
			d := descriptorNamed(c, operationtest.OwnerOnly)
			d.Surfaces, d.SurfaceReason = []operation.Surface{operation.SurfaceChat}, "chat only"
		}, want: 1},
		{name: "a module tool on an owner operation", mutate: func(c *operation.Catalog) { descriptorNamed(c, operationtest.OwnerOnly).ModuleTool = "legacy" }, want: 1},
		{name: "an owner notice with a risk that is not change", mutate: func(c *operation.Catalog) { descriptorNamed(c, operationtest.Notice).Risk = operation.RiskTrustGrant }, want: 2},
		{name: "an owner notice that is a sink", mutate: func(c *operation.Catalog) { descriptorNamed(c, operationtest.Notice).Sink = true }, want: 1},
		{name: "an owner notice with a destination parameter", mutate: func(c *operation.Catalog) {
			d := descriptorNamed(c, operationtest.Notice)
			d.Params = append(d.Params, operation.Param{Name: "channel", Kind: operation.KindString})
		}, want: 1},
		{name: "a tool name over sixty-four bytes", mutate: func(c *operation.Catalog) {
			descriptorNamed(c, operationtest.Read).ID = "stub.read-" + strings.Repeat("x", 60)
		}, want: 1},
		{name: "a group that collides with a chat command", mutate: func(c *operation.Catalog) {
			c.Group.Name = "status"
			for i := range c.Descriptors {
				c.Descriptors[i].ID = "status." + strings.TrimPrefix(c.Descriptors[i].ID, "stub.")
			}
		}, want: 1},
		{name: "a group with more than ten generated tools", mutate: func(c *operation.Catalog) {
			for _, id := range []string{"stub.more-a", "stub.more-b", "stub.more-c"} {
				extra := *descriptorNamed(c, operationtest.Read)
				extra.ID = id
				c.Descriptors = append(c.Descriptors, extra)
			}
		}, want: 1},
		{name: "an unknown need", mutate: func(c *operation.Catalog) { descriptorNamed(c, operationtest.Read).Needs = []string{"gizmos"} }, want: 1},
		{name: "an output without a kind", mutate: func(c *operation.Catalog) { descriptorNamed(c, operationtest.Read).Output.Kind = "" }, want: 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			catalog := operationtest.Catalog()
			tc.mutate(&catalog)
			err := operation.ValidateCatalogs([]operation.Catalog{catalog})
			if !fault.HasCode(err, operation.ErrorCodeInvalid) {
				t.Fatalf("ValidateCatalogs = %v, want operation_invalid", err)
			}
			if got := defectCount(err); got != tc.want {
				t.Errorf("defects = %d, want %d: %v", got, tc.want, err)
			}
		})
	}
}

func TestRegistryReportsEveryDefectTogether(t *testing.T) {
	catalog := operationtest.Catalog()
	descriptorNamed(&catalog, operationtest.Read).Summary = ""
	descriptorNamed(&catalog, operationtest.Change).Risk = "dangerous"
	descriptorNamed(&catalog, operationtest.Fresh).Commit = ""
	descriptorNamed(&catalog, operationtest.Host).Interactive = false
	descriptorNamed(&catalog, operationtest.OwnerOnly).Needs = []string{"gizmos"}

	err := operation.ValidateCatalogs([]operation.Catalog{catalog})

	if got := defectCount(err); got != 4 {
		t.Fatalf("defects = %d, want the four independent defects together: %v", got, err)
	}
}

func TestRegistryAcceptsTheStubCatalog(t *testing.T) {
	if err := operation.ValidateCatalogs([]operation.Catalog{operationtest.Catalog()}); err != nil {
		t.Fatalf("ValidateCatalogs: %v", err)
	}
}

func TestRegistryRejectsAnOwnerNoticeWithoutARateLimit(t *testing.T) {
	rig := operationtest.NewRig(t, &operationtest.Options{
		AllowInvalid: true,
		Bindings: func(stubs *operationtest.Stubs) []operation.Binding {
			bindings := stubs.Bindings()
			for i := range bindings {
				bindings[i].RateLimit = nil
			}
			return bindings
		},
	})
	if !fault.HasCode(rig.RegistryErr, operation.ErrorCodeInvalid) || defectCount(rig.RegistryErr) != 1 {
		t.Fatalf("NewRegistry = %v, want one operation_invalid defect", rig.RegistryErr)
	}
}

func TestRegistryBindingIsOneToOne(t *testing.T) {
	cases := []struct {
		name   string
		change func(bindings []operation.Binding) []operation.Binding
		want   int
	}{
		{name: "a descriptor without a binding", change: func(b []operation.Binding) []operation.Binding { return b[1:] }, want: 1},
		{name: "a binding without a descriptor", change: func(b []operation.Binding) []operation.Binding {
			return append(b, operation.Binding{ID: "stub.ghost", Handle: b[0].Handle})
		}, want: 1},
		{name: "two bindings for one descriptor", change: func(b []operation.Binding) []operation.Binding { return append(b, b[0]) }, want: 1},
		{name: "a binding without a handler", change: func(b []operation.Binding) []operation.Binding {
			b[0].Handle = nil
			return b
		}, want: 2},
		{name: "a preview without a declared preview", change: func(b []operation.Binding) []operation.Binding {
			b[0].Preview = b[5].Preview
			return b
		}, want: 1},
		{name: "a declared preview without a previewer", change: func(b []operation.Binding) []operation.Binding {
			b[4].Preview = nil
			return b
		}, want: 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rig := operationtest.NewRig(t, &operationtest.Options{
				AllowInvalid: true,
				Bindings: func(stubs *operationtest.Stubs) []operation.Binding {
					return tc.change(stubs.Bindings())
				},
			})
			if !fault.HasCode(rig.RegistryErr, operation.ErrorCodeInvalid) {
				t.Fatalf("NewRegistry = %v, want operation_invalid", rig.RegistryErr)
			}
			if got := defectCount(rig.RegistryErr); got != tc.want {
				t.Errorf("defects = %d, want %d: %v", got, tc.want, rig.RegistryErr)
			}
		})
	}
}

func TestRegistryNeedsItsDependencies(t *testing.T) {
	if _, err := operation.NewRegistry(nil, nil, nil); !fault.HasCode(err, operation.ErrorCodeInvalidArgument) {
		t.Errorf("NewRegistry without dependencies = %v, want invalid_argument", err)
	}
	if _, err := operation.NewRegistry(nil, nil, &operation.Deps{}); !fault.HasCode(err, operation.ErrorCodeInvalidArgument) {
		t.Errorf("NewRegistry without a database = %v, want invalid_argument", err)
	}
	rig := operationtest.NewRig(t, nil)
	deps := operation.Deps{DB: rig.DB, Effects: rig.Executor}
	if _, err := operation.NewRegistry(nil, nil, &deps); !fault.HasCode(err, operation.ErrorCodeInvalidArgument) {
		t.Errorf("NewRegistry without the tool lookup = %v, want invalid_argument", err)
	}
}

func TestRegistryRejectsTwoOperationsGeneratingOneToolName(t *testing.T) {
	catalog := operationtest.Catalog()
	first := *descriptorNamed(&catalog, operationtest.Read)
	first.ID = "stub.run-now"
	second := first
	second.ID = "stub.run.now"
	catalog.Descriptors = append(catalog.Descriptors, first, second)

	err := operation.ValidateCatalogs([]operation.Catalog{catalog})

	if !fault.HasCode(err, operation.ErrorCodeInvalid) || defectCount(err) != 1 {
		t.Fatalf("ValidateCatalogs = %v, want one operation_invalid defect for the shared tool name", err)
	}
}

func TestRegistryRejectsARequiredPathParameterThatChatProjects(t *testing.T) {
	path := operation.Param{Name: "file", Kind: operation.KindPath, Required: true, Position: 2}
	cases := []struct {
		name   string
		mutate func(d *operation.Descriptor)
		want   int
	}{
		{name: "an operation that projects to chat", mutate: func(d *operation.Descriptor) { d.Params = append(d.Params, path) }, want: 1},
		{name: "an operation that does not project to chat", mutate: func(d *operation.Descriptor) {
			d.Params = append(d.Params, path)
			d.Surfaces, d.SurfaceReason = []operation.Surface{operation.SurfaceCLI, operation.SurfaceAgent}, "files are not chat input"
		}},
		{name: "a required path parameter limited to the CLI and the agent", mutate: func(d *operation.Descriptor) {
			limited := path
			limited.Surfaces = []operation.Surface{operation.SurfaceCLI, operation.SurfaceAgent}
			d.Params = append(d.Params, limited)
		}},
		{name: "an optional path parameter", mutate: func(d *operation.Descriptor) {
			optional := path
			optional.Required = false
			d.Params = append(d.Params, optional)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			catalog := operationtest.Catalog()
			tc.mutate(descriptorNamed(&catalog, operationtest.Read))

			err := operation.ValidateCatalogs([]operation.Catalog{catalog})

			if got := defectCount(err); got != tc.want {
				t.Fatalf("defects = %d, want %d: %v", got, tc.want, err)
			}
		})
	}
}

func TestRegisteredDescriptorsCannotBeChangedAfterwards(t *testing.T) {
	rig := operationtest.NewRig(t, nil)
	before, _ := rig.Registry.Descriptor(operationtest.Read)
	wantSchema, err := before.SchemaFor(operation.SurfaceAgent)
	if err != nil {
		t.Fatalf("SchemaFor: %v", err)
	}

	changed, _ := rig.Registry.Descriptor(operationtest.Read)
	changed.Summary = "changed"
	changed.Risk = operation.RiskDestructive
	changed.Params[0].Name = "renamed"
	changed.Params[0].MaxLength = 1
	changed.Output.Fields[0].Name = "renamed"
	for _, listed := range rig.Registry.Descriptors() {
		listed.Params = nil
		listed.Display = []string{"changed"}
	}

	after, _ := rig.Registry.Descriptor(operationtest.Read)
	gotSchema, err := after.SchemaFor(operation.SurfaceAgent)
	if err != nil {
		t.Fatalf("SchemaFor: %v", err)
	}
	if after.Summary != operationtest.DefaultNote || after.Risk != operation.RiskRead || len(after.Params) == 0 || string(gotSchema) != string(wantSchema) {
		t.Fatalf("the registry's descriptor changed through a returned copy: %+v", after)
	}
	for _, definition := range rig.Broker.Definitions() {
		if definition.Name == operationtest.ToolOf(operationtest.Read) {
			if _, err := rig.Broker.Execute(t.Context(), operationtest.Request(t, definition.Name, "a")); err != nil {
				t.Fatalf("the generated tool no longer validates its original input: %v", err)
			}
		}
	}
}

func TestRegistryRejectsAToolNameThatIsAlreadyRegistered(t *testing.T) {
	rig := operationtest.NewRig(t, nil)
	_, err := operation.NewRegistry([]operation.Catalog{operationtest.Catalog()}, rig.Stubs.Bindings(), &operation.Deps{DB: rig.DB, Effects: rig.Executor, Tools: rig.Broker, Clock: rig.Clock})
	if !fault.HasCode(err, operation.ErrorCodeInvalid) || defectCount(err) != 8 {
		t.Fatalf("a second NewRegistry over the same broker = %v, want one operation_invalid defect per generated tool", err)
	}
}
