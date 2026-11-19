package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"

	"gopkg.in/yaml.v3"
)

func Marshal(c *Config) ([]byte, error) {
	encoded, err := json.Marshal(c)
	if err != nil {
		return nil, fmt.Errorf("encode config: %w", err)
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(encoded, &doc); err != nil {
		return nil, fmt.Errorf("read encoded config: %w", err)
	}
	if len(doc.Content) == 1 {
		if err := restyle(doc.Content[0], reflect.TypeFor[Config]()); err != nil {
			return nil, err
		}
	}
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(&doc); err != nil {
		return nil, fmt.Errorf("write config: %w", err)
	}
	if err := enc.Close(); err != nil {
		return nil, fmt.Errorf("write config: %w", err)
	}
	return buf.Bytes(), nil
}

func restyle(node *yaml.Node, t reflect.Type) error {
	t = indirect(t)
	if node.Kind == yaml.ScalarNode {
		return restyleScalar(node)
	}
	node.Style = 0
	if node.Kind == yaml.MappingNode {
		return restyleMapping(node, t)
	}
	for _, item := range node.Content {
		if err := restyleValue(item, elementType(t)); err != nil {
			return err
		}
	}
	return nil
}

func restyleMapping(node *yaml.Node, t reflect.Type) error {
	content := make([]*yaml.Node, 0, len(node.Content))
	for i := 0; i+1 < len(node.Content); i += 2 {
		key, value := node.Content[i], node.Content[i+1]
		valueType := memberType(t, key.Value)
		if isNull(value) && valueType != nil && valueType.Kind() == reflect.Pointer {
			continue
		}
		if err := restyleScalar(key); err != nil {
			return err
		}
		if err := restyleValue(value, valueType); err != nil {
			return err
		}
		content = append(content, key, value)
	}
	node.Content = content
	return nil
}

func restyleValue(node *yaml.Node, t reflect.Type) error {
	if isNull(node) {
		renderNull(node, t)
		return nil
	}
	return restyle(node, t)
}

func renderNull(node *yaml.Node, t reflect.Type) {
	if t == nil {
		return
	}
	if kind := indirect(t).Kind(); kind == reflect.Slice {
		*node = yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
	} else if kind == reflect.Map {
		*node = yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	}
}

func restyleScalar(node *yaml.Node) error {
	if node.Tag != "!!str" {
		node.Style = 0
		return nil
	}
	var styled yaml.Node
	if err := styled.Encode(node.Value); err != nil {
		return fmt.Errorf("style config value: %w", err)
	}
	node.Style = styled.Style
	return nil
}

func isNull(node *yaml.Node) bool {
	return node.Kind == yaml.ScalarNode && node.Tag == "!!null"
}

func indirect(t reflect.Type) reflect.Type {
	for t != nil && t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	return t
}

func elementType(t reflect.Type) reflect.Type {
	if t == nil || (t.Kind() != reflect.Slice && t.Kind() != reflect.Array) {
		return nil
	}
	return t.Elem()
}

func memberType(t reflect.Type, name string) reflect.Type {
	if t == nil {
		return nil
	}
	if t.Kind() == reflect.Map {
		return t.Elem()
	}
	if t.Kind() != reflect.Struct {
		return nil
	}
	for field := range t.Fields() {
		if strings.Split(field.Tag.Get("json"), ",")[0] == name {
			return field.Type
		}
	}
	return nil
}
