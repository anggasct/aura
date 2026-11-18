package config

import (
	"fmt"
	"strconv"
	"strings"

	yamlv3 "gopkg.in/yaml.v3"

	"github.com/anggasct/aura/internal/platform/configkit"
	"github.com/anggasct/aura/internal/platform/fault"
)

type document struct {
	data []byte
	root *yamlv3.Node
}

func parseDocument(data []byte) (*document, error) {
	var tree yamlv3.Node
	if err := yamlv3.Unmarshal(data, &tree); err != nil {
		return nil, malformedDocument(fault.Wrap(configkit.ErrorCodeMalformed, "invalid YAML", err))
	}
	if tree.Kind != yamlv3.DocumentNode || len(tree.Content) == 0 {
		return nil, malformedDocument(fault.New(configkit.ErrorCodeMalformed, "invalid YAML: empty document"))
	}
	root := tree.Content[0]
	if root.Kind != yamlv3.MappingNode {
		return nil, malformedDocument(fault.New(configkit.ErrorCodeMalformed, "invalid YAML: top level must be a mapping"))
	}
	return &document{data: data, root: root}, nil
}

func malformedDocument(err error) error {
	return configkit.Field("", err)
}

func duplicateKeys(node *yamlv3.Node, path string) []error {
	var problems []error
	switch node.Kind {
	case yamlv3.MappingNode:
		seen := make(map[string]bool, len(node.Content)/2)
		for i := 0; i+1 < len(node.Content); i += 2 {
			key := node.Content[i]
			child := joinPath(path, key.Value)
			if seen[key.Value] {
				problems = append(problems, &configkit.FieldError{
					Path:     child,
					Position: nodePosition(key),
					Err:      fault.New(configkit.ErrorCodeMalformed, "duplicate key"),
				})
			}
			seen[key.Value] = true
			problems = append(problems, duplicateKeys(node.Content[i+1], child)...)
		}
	case yamlv3.SequenceNode:
		for i, item := range node.Content {
			problems = append(problems, duplicateKeys(item, path+"["+strconv.Itoa(i)+"]")...)
		}
	default:
	}
	return problems
}

func documentVersion(doc *yamlv3.Node) (int, error) {
	version := 0
	if value := mappingValue(doc, "version"); value != nil {
		if err := value.Decode(&version); err != nil {
			return 0, fmt.Errorf("invalid version at line %d: %w", value.Line, err)
		}
	}
	return version, nil
}

func (d *document) has(path string) bool {
	_, _, found := d.find(path)
	return found
}

func (d *document) positionOf(path string) string {
	if name, set := environmentVariableFor(dottedPath(path)); set {
		return "env " + name
	}
	if _, value, found := d.find(path); found {
		return nodePosition(value)
	}
	return ""
}

func (d *document) keyPositionOf(path string) string {
	if key, _, found := d.find(path); found {
		return nodePosition(key)
	}
	return ""
}

func (d *document) find(path string) (key, value *yamlv3.Node, found bool) {
	if path == "" {
		return nil, nil, false
	}
	return descend(d.root, strings.Split(dottedPath(path), "."))
}

func descend(node *yamlv3.Node, segments []string) (key, value *yamlv3.Node, found bool) {
	if node.Kind == yamlv3.AliasNode {
		node = node.Alias
	}
	switch node.Kind {
	case yamlv3.MappingNode:
		for n := 1; n <= len(segments); n++ {
			name := strings.Join(segments[:n], ".")
			for i := 0; i+1 < len(node.Content); i += 2 {
				if node.Content[i].Value != name {
					continue
				}
				if n == len(segments) {
					return node.Content[i], node.Content[i+1], true
				}
				if key, value, found = descend(node.Content[i+1], segments[n:]); found {
					return key, value, true
				}
			}
		}
	case yamlv3.SequenceNode:
		index, err := strconv.Atoi(segments[0])
		if err != nil || index < 0 || index >= len(node.Content) {
			return nil, nil, false
		}
		if len(segments) == 1 {
			return node.Content[index], node.Content[index], true
		}
		return descend(node.Content[index], segments[1:])
	default:
	}
	return nil, nil, false
}

func dottedPath(path string) string {
	return strings.NewReplacer("[", ".", "]", "").Replace(path)
}

func joinPath(prefix, name string) string {
	if prefix == "" {
		return name
	}
	return prefix + "." + name
}

func nodePosition(node *yamlv3.Node) string {
	return fmt.Sprintf("line %d, column %d", node.Line, node.Column)
}
