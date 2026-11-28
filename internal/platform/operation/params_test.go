package operation_test

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/operation"
)

var updateGolden = flag.Bool("update", false, "rewrite golden files")

func int64Ptr(v int64) *int64 { return &v }

func projectionDescriptor() operation.Descriptor {
	cli := []operation.Surface{operation.SurfaceCLI}
	return operation.Descriptor{
		ID: "proj.notify", Summary: "send a message", Description: "sends a message to the owner",
		Exposure: operation.ExposureAgent, Risk: operation.RiskChange, Commit: operation.CommitExternalIdempotent,
		Params: []operation.Param{
			{Name: "message", Kind: operation.KindString, Summary: "the text", Required: true, Position: 1, Rest: true, MinLength: 1, MaxLength: 500},
			{Name: "priority", Kind: operation.KindEnum, Summary: "how loud", Default: json.RawMessage(`"info"`), Enum: []operation.EnumValue{
				{Value: "info"}, {Value: "warning"}, {Value: "urgent", Surfaces: cli},
			}},
			{Name: "attempts", Kind: operation.KindInteger, Summary: "tries", Minimum: int64Ptr(1), Maximum: int64Ptr(5), Default: json.RawMessage(`3`)},
			{Name: "grace", Kind: operation.KindDuration, Summary: "wait first"},
			{Name: "tags", Kind: operation.KindStrings, Summary: "labels", MaxLength: 16},
			{Name: "payload", Kind: operation.KindJSON, Summary: "extra data", Schema: json.RawMessage(`{"type":"object","additionalProperties":true}`), Surfaces: cli},
			{Name: "probe", Kind: operation.KindBoolean, Summary: "dry run", Surfaces: cli},
			{Name: "attachment", Kind: operation.KindPath, Summary: "a file", Surfaces: []operation.Surface{operation.SurfaceCLI, operation.SurfaceAgent}},
		},
		Output: operation.Output{Kind: operation.OutputRecord, Fields: []operation.OutputField{
			{Name: "id"}, {Name: "status"}, {Name: "trace", Surfaces: cli},
		}},
	}
}

func projectionCatalog() operation.Catalog {
	return operation.Catalog{
		Group:       operation.Group{Name: "proj", Summary: "projection fixtures"},
		Descriptors: []operation.Descriptor{projectionDescriptor()},
	}
}

func describeProjection(t *testing.T, d *operation.Descriptor) string {
	t.Helper()
	var out strings.Builder
	fmt.Fprintf(&out, "== %s\n", d.ID)
	schema, err := d.SchemaFor(operation.SurfaceAgent)
	if err != nil {
		t.Fatalf("SchemaFor: %v", err)
	}
	var pretty bytes.Buffer
	if err := json.Indent(&pretty, schema, "", "  "); err != nil {
		t.Fatalf("indent the schema: %v", err)
	}
	fmt.Fprintf(&out, "schema agent:\n%s\n", pretty.String())
	for _, surface := range []operation.Surface{operation.SurfaceCLI, operation.SurfaceChat, operation.SurfaceAgent} {
		fmt.Fprintf(&out, "%s positionals:\n", surface)
		positionals := d.Positionals(surface)
		for i := range positionals {
			fmt.Fprintf(&out, "  %d %s %s required=%t rest=%t\n", positionals[i].Position, positionals[i].Param, positionals[i].Kind, positionals[i].Required, positionals[i].Rest)
		}
		fmt.Fprintf(&out, "%s flags:\n", surface)
		flags := d.Flags(surface)
		for i := range flags {
			fmt.Fprintf(&out, "  --%s %s repeated=%t values=%v default=%s\n", flags[i].Name, flags[i].Kind, flags[i].Repeated, flags[i].Values, flags[i].Default)
		}
		fmt.Fprintf(&out, "%s output: %v\n", surface, d.OutputFieldsOn(surface))
	}
	return out.String()
}

func compareGolden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", "golden", name)
	if *updateGolden {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatalf("create the golden directory: %v", err)
		}
		if err := os.WriteFile(path, []byte(got), 0o600); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if string(want) != got {
		t.Errorf("%s differs from the generated projection:\n%s", name, got)
	}
}

func TestParamsProjectIdentically(t *testing.T) {
	catalog := projectionCatalog()
	d := &catalog.Descriptors[0]
	if err := operation.ValidateCatalogs([]operation.Catalog{catalog}); err != nil {
		t.Fatalf("ValidateCatalogs: %v", err)
	}

	t.Run("golden schema, flags and positionals", func(t *testing.T) {
		compareGolden(t, "projection.txt", describeProjection(t, d))
	})

	t.Run("equal inputs give identical canonical bytes and digest", func(t *testing.T) {
		toolInput := json.RawMessage(`{"attempts":2,  "message":"hello there","tags":["a","b"],"priority":"warning"}`)
		tool, err := d.Validate(operation.SurfaceAgent, toolInput)
		if err != nil {
			t.Fatalf("tool validation: %v", err)
		}
		for _, surface := range []operation.Surface{operation.SurfaceCLI, operation.SurfaceChat} {
			in, err := operation.NewInput(d, surface)
			if err != nil {
				t.Fatalf("NewInput: %v", err)
			}
			for _, set := range []struct {
				name   string
				values []string
			}{{"message", []string{"hello there"}}, {"priority", []string{"warning"}}, {"attempts", []string{"2"}}, {"tags", []string{"a", "b"}}} {
				if err := in.Set(set.name, set.values...); err != nil {
					t.Fatalf("%s Set(%s): %v", surface, set.name, err)
				}
			}
			canonical, err := in.Canonical()
			if err != nil {
				t.Fatalf("%s Canonical: %v", surface, err)
			}
			if !bytes.Equal(canonical, tool) || operation.Digest(canonical) != operation.Digest(tool) {
				t.Errorf("%s canonical = %s, want the tool's %s", surface, canonical, tool)
			}
		}
		want := `{"message":"hello there","priority":"warning","attempts":2,"tags":["a","b"]}`
		if string(tool) != want {
			t.Errorf("canonical input = %s, want %s with keys in parameter order", tool, want)
		}
	})

	t.Run("defaults are part of the canonical input", func(t *testing.T) {
		canonical, err := d.Validate(operation.SurfaceAgent, json.RawMessage(`{"message":"hi"}`))
		if err != nil {
			t.Fatalf("Validate: %v", err)
		}
		if want := `{"message":"hi","priority":"info","attempts":3}`; string(canonical) != want {
			t.Errorf("canonical input = %s, want %s", canonical, want)
		}
	})

	t.Run("per-surface enum values, parameters and kinds", func(t *testing.T) {
		cases := []struct {
			name    string
			surface operation.Surface
			input   string
			ok      bool
		}{
			{name: "urgent on the CLI", surface: operation.SurfaceCLI, input: `{"message":"m","priority":"urgent"}`, ok: true},
			{name: "urgent on the tool", surface: operation.SurfaceAgent, input: `{"message":"m","priority":"urgent"}`},
			{name: "urgent in chat", surface: operation.SurfaceChat, input: `{"message":"m","priority":"urgent"}`},
			{name: "a CLI-only parameter on the tool", surface: operation.SurfaceAgent, input: `{"message":"m","probe":true}`},
			{name: "a CLI-only parameter on the CLI", surface: operation.SurfaceCLI, input: `{"message":"m","probe":true}`, ok: true},
			{name: "a path in chat", surface: operation.SurfaceChat, input: `{"message":"m","attachment":"a.txt"}`},
			{name: "a path on the tool", surface: operation.SurfaceAgent, input: `{"message":"m","attachment":"a.txt"}`, ok: true},
			{name: "a missing required parameter", surface: operation.SurfaceAgent, input: `{}`},
			{name: "an unknown parameter", surface: operation.SurfaceAgent, input: `{"message":"m","extra":1}`},
			{name: "a repeated key", surface: operation.SurfaceAgent, input: `{"message":"a","message":"b"}`},
			{name: "a number out of range", surface: operation.SurfaceAgent, input: `{"message":"m","attempts":9}`},
			{name: "a float for an integer", surface: operation.SurfaceAgent, input: `{"message":"m","attempts":2.0}`},
			{name: "a duration without a unit", surface: operation.SurfaceAgent, input: `{"message":"m","grace":"10"}`},
			{name: "a duration with a unit", surface: operation.SurfaceAgent, input: `{"message":"m","grace":"1h30m"}`, ok: true},
			{name: "a fractional duration", surface: operation.SurfaceAgent, input: `{"message":"m","grace":"1.5h"}`, ok: true},
			{name: "a bare zero duration", surface: operation.SurfaceAgent, input: `{"message":"m","grace":"0"}`, ok: true},
			{name: "a negative duration", surface: operation.SurfaceAgent, input: `{"message":"m","grace":"-1h"}`},
			{name: "a string over its length", surface: operation.SurfaceAgent, input: `{"message":"` + strings.Repeat("x", 501) + `"}`},
			{name: "too long a tag", surface: operation.SurfaceAgent, input: `{"message":"m","tags":["` + strings.Repeat("x", 17) + `"]}`},
			{name: "the wrong JSON type for a json parameter", surface: operation.SurfaceCLI, input: `{"message":"m","payload":[1]}`},
			{name: "a json parameter of its type", surface: operation.SurfaceCLI, input: `{"message":"m","payload":{"a":1}}`, ok: true},
			{name: "input that is not an object", surface: operation.SurfaceAgent, input: `[1]`},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				_, err := d.Validate(tc.surface, json.RawMessage(tc.input))
				if tc.ok && err != nil {
					t.Fatalf("Validate = %v, want accepted", err)
				}
				if !tc.ok && !fault.HasCode(err, operation.ErrorCodeInputInvalid) {
					t.Fatalf("Validate = %v, want operation_input_invalid", err)
				}
			})
		}
	})

	t.Run("the input builder refuses what the surface does not have", func(t *testing.T) {
		in, err := operation.NewInput(d, operation.SurfaceAgent)
		if err != nil {
			t.Fatalf("NewInput: %v", err)
		}
		if err := in.Set("probe", "true"); !fault.HasCode(err, operation.ErrorCodeInputInvalid) {
			t.Errorf("Set of a CLI-only parameter on the tool surface = %v, want operation_input_invalid", err)
		}
		if err := in.Set("attempts", "many"); err != nil {
			t.Fatalf("Set: %v", err)
		}
		if _, err := in.Canonical(); !fault.HasCode(err, operation.ErrorCodeInputInvalid) {
			t.Errorf("Canonical with a non-integer = %v, want operation_input_invalid", err)
		}
	})
}

func TestToolSchemaOmitsCLIOnlyValuesAndParameters(t *testing.T) {
	catalog := projectionCatalog()
	schema, err := catalog.Descriptors[0].SchemaFor(operation.SurfaceAgent)
	if err != nil {
		t.Fatalf("SchemaFor: %v", err)
	}
	var decoded struct {
		ID         string `json:"$id"`
		Properties map[string]struct {
			Enum []string `json:"enum"`
		} `json:"properties"`
		Required             []string `json:"required"`
		AdditionalProperties *bool    `json:"additionalProperties"`
	}
	if err := json.Unmarshal(schema, &decoded); err != nil {
		t.Fatalf("decode the schema: %v", err)
	}
	if decoded.ID != "aura://operations/proj.notify/v1" {
		t.Errorf("$id = %q, want aura://operations/proj.notify/v1", decoded.ID)
	}
	if got := decoded.Properties["priority"].Enum; len(got) != 2 || got[0] != "info" || got[1] != "warning" {
		t.Errorf("priority enum = %v, want [info warning]", got)
	}
	for _, name := range []string{"probe", "payload"} {
		if _, present := decoded.Properties[name]; present {
			t.Errorf("the tool schema carries the CLI-only parameter %s", name)
		}
	}
	if len(decoded.Required) != 1 || decoded.Required[0] != "message" || decoded.AdditionalProperties == nil || *decoded.AdditionalProperties {
		t.Errorf("required = %v, additionalProperties = %v, want [message] and false", decoded.Required, decoded.AdditionalProperties)
	}
}

func boundedDescriptor() operation.Descriptor {
	return operation.Descriptor{
		ID: "bound.set", Summary: "set a bounded value", Exposure: operation.ExposureAgent, Risk: operation.RiskChange, Commit: operation.CommitTransactional,
		Params: []operation.Param{{
			Name: "value", Kind: operation.KindString, Summary: "the value", Required: true, MinLength: 1, MaxLength: 50,
			Bounds: []operation.SurfaceLength{{Surface: operation.SurfaceAgent, MaxLength: 10}},
		}},
		Output: operation.Output{Kind: operation.OutputText},
	}
}

func TestASurfaceLengthBoundsTheSchemaAndTheValidationOfThatSurfaceOnly(t *testing.T) {
	d := boundedDescriptor()
	agentSchema, err := d.SchemaFor(operation.SurfaceAgent)
	if err != nil {
		t.Fatalf("SchemaFor agent: %v", err)
	}
	cliSchema, err := d.SchemaFor(operation.SurfaceCLI)
	if err != nil {
		t.Fatalf("SchemaFor cli: %v", err)
	}
	if !strings.Contains(string(agentSchema), `"maxLength":10`) || !strings.Contains(string(cliSchema), `"maxLength":50`) {
		t.Errorf("schemas\nagent: %s\ncli: %s", agentSchema, cliSchema)
	}
	long := json.RawMessage(`{"value":"` + strings.Repeat("v", 11) + `"}`)
	if _, err := d.Validate(operation.SurfaceAgent, long); !fault.HasCode(err, operation.ErrorCodeInputInvalid) {
		t.Errorf("the agent surface accepted 11 characters: %v", err)
	}
	if _, err := d.Validate(operation.SurfaceCLI, long); err != nil {
		t.Errorf("the CLI surface refused 11 characters: %v", err)
	}
}

func TestACatalogRejectsASurfaceLengthOutsideTheParameterRange(t *testing.T) {
	cases := map[string]operation.SurfaceLength{
		"over the range":     {Surface: operation.SurfaceAgent, MaxLength: 51},
		"zero":               {Surface: operation.SurfaceAgent, MaxLength: 0},
		"an unknown surface": {Surface: operation.Surface("nowhere"), MaxLength: 5},
	}
	for name, bound := range cases {
		t.Run(name, func(t *testing.T) {
			d := boundedDescriptor()
			d.Params[0].Bounds = []operation.SurfaceLength{bound}
			err := operation.ValidateCatalogs([]operation.Catalog{{Group: operation.Group{Name: "bound", Summary: "bounds"}, Descriptors: []operation.Descriptor{d}}})
			if err == nil {
				t.Fatal("ValidateCatalogs accepted the surface length")
			}
		})
	}
	t.Run("a repeated surface", func(t *testing.T) {
		d := boundedDescriptor()
		d.Params[0].Bounds = append(d.Params[0].Bounds, d.Params[0].Bounds[0])
		if err := operation.ValidateCatalogs([]operation.Catalog{{Group: operation.Group{Name: "bound", Summary: "bounds"}, Descriptors: []operation.Descriptor{d}}}); err == nil {
			t.Fatal("ValidateCatalogs accepted a repeated surface")
		}
	})
	t.Run("a non string parameter", func(t *testing.T) {
		d := boundedDescriptor()
		d.Params[0] = operation.Param{Name: "count", Kind: operation.KindInteger, Summary: "n", Bounds: []operation.SurfaceLength{{Surface: operation.SurfaceAgent, MaxLength: 5}}}
		if err := operation.ValidateCatalogs([]operation.Catalog{{Group: operation.Group{Name: "bound", Summary: "bounds"}, Descriptors: []operation.Descriptor{d}}}); err == nil {
			t.Fatal("ValidateCatalogs accepted a surface length on an integer")
		}
	})
}
