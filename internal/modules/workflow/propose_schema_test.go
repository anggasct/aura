package workflow

import (
	"encoding/json"
	"maps"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func proposeSpecNode(t *testing.T) map[string]any {
	t.Helper()
	definitions := ToolDefinitions()
	for i := range definitions {
		if definitions[i].Name != ProposeToolName {
			continue
		}
		var schema map[string]any
		if err := json.Unmarshal(definitions[i].Schema, &schema); err != nil {
			t.Fatalf("propose schema: %v", err)
		}
		return childNode(t, childNode(t, schema, "properties"), "spec")
	}
	t.Fatalf("%s is not defined", ProposeToolName)
	return nil
}

func childNode(t *testing.T, node map[string]any, key string) map[string]any {
	t.Helper()
	child, ok := node[key].(map[string]any)
	if !ok {
		t.Fatalf("schema node lacks object %q: %v", key, node)
	}
	return child
}

func schemaProperties(t *testing.T, node map[string]any) []string {
	t.Helper()
	return slices.Sorted(maps.Keys(childNode(t, node, "properties")))
}

func decoderFields(model reflect.Type) []string {
	var fields []string
	for field := range model.Fields() {
		name, _, _ := strings.Cut(field.Tag.Get("json"), ",")
		fields = append(fields, name)
	}
	slices.Sort(fields)
	return fields
}

func executorBranches(t *testing.T, step map[string]any) []map[string]any {
	t.Helper()
	raw, ok := childNode(t, childNode(t, step, "properties"), "executor")["oneOf"].([]any)
	if !ok {
		t.Fatal("executor schema has no oneOf")
	}
	branches := make([]map[string]any, 0, len(raw))
	for _, branch := range raw {
		node, ok := branch.(map[string]any)
		if !ok {
			t.Fatalf("executor branch is not an object: %v", branch)
		}
		branches = append(branches, node)
	}
	return branches
}

func TestProposeSchemaMatchesTheSpecDecoder(t *testing.T) {
	spec := proposeSpecNode(t)
	step := childNode(t, childNode(t, spec, "properties"), "steps")
	stepItems := childNode(t, step, "items")
	retry := childNode(t, childNode(t, stepItems, "properties"), "retry")
	branches := executorBranches(t, stepItems)

	executorProperties := map[string]bool{}
	var kinds []string
	for _, branch := range branches {
		for _, name := range schemaProperties(t, branch) {
			executorProperties[name] = true
		}
		kind, _ := childNode(t, childNode(t, branch, "properties"), "kind")["const"].(string)
		kinds = append(kinds, kind)
	}

	cases := []struct {
		name        string
		properties  []string
		model       reflect.Type
		notOffered  []string
		closedNodes []map[string]any
	}{
		{name: "spec offers every decoder field except those the normalizer overwrites", properties: schemaProperties(t, spec), model: reflect.TypeFor[Spec](), notOffered: []string{"id", "name", "source", "version"}, closedNodes: []map[string]any{spec}},
		{name: "step offers every decoder field except the timeout default", properties: schemaProperties(t, stepItems), model: reflect.TypeFor[StepSpec](), notOffered: []string{"timeout"}, closedNodes: []map[string]any{stepItems}},
		{name: "retry offers attempts and not backoff", properties: schemaProperties(t, retry), model: reflect.TypeFor[RetryPolicy](), notOffered: []string{"backoff"}, closedNodes: []map[string]any{retry}},
		{name: "executor branches together offer every decoder field", properties: slices.Sorted(maps.Keys(executorProperties)), model: reflect.TypeFor[ExecutorSpec](), closedNodes: branches},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fields := decoderFields(tc.model)
			for _, property := range tc.properties {
				if !slices.Contains(fields, property) {
					t.Errorf("schema property %q is not a field the decoder accepts (%v)", property, fields)
				}
			}
			var missing []string
			for _, field := range fields {
				if !slices.Contains(tc.properties, field) {
					missing = append(missing, field)
				}
			}
			if !slices.Equal(missing, tc.notOffered) {
				t.Errorf("decoder fields absent from the schema = %v, want %v", missing, tc.notOffered)
			}
			for _, node := range tc.closedNodes {
				if node["additionalProperties"] != false {
					t.Errorf("schema node %v does not close additionalProperties", node["type"])
				}
			}
		})
	}

	t.Run("executor branches cover every kind once", func(t *testing.T) {
		want := []string{string(KindAgent), string(KindApproval), string(KindTool), string(KindWait), string(KindWorkflow)}
		slices.Sort(kinds)
		if !slices.Equal(kinds, want) {
			t.Fatalf("executor kinds = %v, want %v", kinds, want)
		}
	})

	t.Run("required fields are the ones the validator insists on", func(t *testing.T) {
		for name, tc := range map[string]struct {
			node map[string]any
			want []any
		}{
			"step": {node: stepItems, want: []any{"id", "executor"}},
			"spec": {node: spec, want: []any{"goal", "steps"}},
		} {
			if got := tc.node["required"]; !reflect.DeepEqual(got, tc.want) {
				t.Errorf("%s required = %v, want %v", name, got, tc.want)
			}
		}
	})
}
