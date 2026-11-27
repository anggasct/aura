package operation

import (
	"bytes"
	"encoding/json"
	"slices"

	"github.com/anggasct/aura/internal/platform/tools"
)

func comparedKeywords() []string {
	return []string{"type", "minLength", "maxLength", "minimum", "maximum", "enum"}
}

type shippedSchema struct {
	Properties map[string]map[string]json.RawMessage `json:"properties"`
	Required   []string                              `json:"required"`
}

func moduleToolMismatches(d *Descriptor, definition *tools.Definition) []string {
	if d == nil || definition == nil {
		return []string{"cannot be compared"}
	}
	var shipped shippedSchema
	if len(definition.Schema) > 0 {
		if err := json.Unmarshal(definition.Schema, &shipped); err != nil {
			return []string{"has a schema that cannot be read"}
		}
	}
	var mismatches []string
	if definition.UntrustedOutput != d.UntrustedOutput {
		mismatches = append(mismatches, "does not agree on untrusted output")
	}
	params := d.paramsOn(SurfaceAgent)
	declared := make([]string, 0, len(params))
	var required []string
	for _, param := range params {
		declared = append(declared, param.Name)
		if param.Required {
			required = append(required, param.Name)
		}
		shippedProperty, present := shipped.Properties[param.Name]
		if !present {
			mismatches = append(mismatches, "has no property "+param.Name)
			continue
		}
		mismatches = append(mismatches, propertyMismatches(param, shippedProperty)...)
	}
	for name := range shipped.Properties {
		if !slices.Contains(declared, name) {
			mismatches = append(mismatches, "has a property "+name+" the operation does not declare on the agent surface")
		}
	}
	slices.Sort(required)
	shippedRequired := slices.Clone(shipped.Required)
	slices.Sort(shippedRequired)
	if !slices.Equal(required, shippedRequired) {
		mismatches = append(mismatches, "does not require the same parameters")
	}
	slices.Sort(mismatches)
	return slices.Compact(mismatches)
}

func propertyMismatches(param *Param, shipped map[string]json.RawMessage) []string {
	generated, err := param.propertyJSON(SurfaceAgent)
	if err != nil {
		return []string{"has a property " + param.Name + " that cannot be generated"}
	}
	var expected map[string]json.RawMessage
	if err := json.Unmarshal(generated, &expected); err != nil {
		return []string{"has a property " + param.Name + " that cannot be read"}
	}
	var mismatches []string
	for _, keyword := range comparedKeywords() {
		want, wanted := expected[keyword]
		have, has := shipped[keyword]
		if wanted != has || (wanted && !sameJSON(want, have)) {
			mismatches = append(mismatches, "differs on "+keyword+" of "+param.Name)
		}
	}
	return mismatches
}

func sameJSON(a, b json.RawMessage) bool {
	var left, right bytes.Buffer
	if json.Compact(&left, a) != nil || json.Compact(&right, b) != nil {
		return false
	}
	return bytes.Equal(left.Bytes(), right.Bytes())
}
