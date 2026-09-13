package skills

import (
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"unicode/utf8"

	"gopkg.in/yaml.v3"
)

const (
	maxSkillFileBytes     = 1 << 20
	maxInstructionRunes   = 1 << 16
	maxYAMLDepth          = 32
	maxYAMLNodes          = 4096
	maxYAMLAliases        = 128
	maxUnknownFields      = 32
	maxUnknownFieldBytes  = 4096
	maxMetadataKeys       = 64
	maxMetadataDepth      = 4
	maxMetadataBytes      = 1 << 16
	maxAllowedTools       = 64
	maxAllowedToolsBytes  = 4096
	maxNameRunes          = 64
	maxDescriptionRunes   = 1024
	maxLicenseRunes       = 256
	maxCompatibilityRunes = 500
)

const (
	FindingFileTooLarge          = "file_too_large"
	FindingFileNotUTF8           = "file_not_utf8"
	FindingFrontmatterMissing    = "frontmatter_missing"
	FindingFrontmatterMalformed  = "frontmatter_malformed"
	FindingBodyTooLarge          = "body_too_large"
	FindingNameMissing           = "name_missing"
	FindingNameInvalid           = "name_invalid"
	FindingDescriptionMissing    = "description_missing"
	FindingDescriptionInvalid    = "description_invalid"
	FindingLicenseInvalid        = "license_invalid"
	FindingCompatibilityInvalid  = "compatibility_invalid"
	FindingAllowedToolsInvalid   = "allowed_tools_invalid"
	FindingMetadataInvalid       = "metadata_invalid"
	FindingAuraMetadataInvalid   = "aura_metadata_invalid"
	FindingUnknownFieldsExceeded = "unknown_fields_exceeded"
	FindingYAMLTooDeep           = "yaml_too_deep"
	FindingYAMLTooManyNodes      = "yaml_too_many_nodes"
	FindingYAMLTooManyAliases    = "yaml_too_many_aliases"
)

var validNamePattern = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,62}[a-z0-9])?$`)

type Manifest struct {
	Name          string
	Description   string
	License       string
	Compatibility string
	AllowedTools  []string
	Metadata      map[string]string
	Aura          map[string]string
	UnknownFields map[string]string
	Body          string
}

type yamlBudget struct {
	nodes   int
	aliases int
	active  map[string]bool
}

type yamlLimitError struct {
	finding string
}

func (e *yamlLimitError) Error() string {
	return e.finding
}

type nonStringScalar struct {
	tag  string
	text string
}

func ParseSkillFile(content []byte) (manifest *Manifest, findings []string) {
	if len(content) > maxSkillFileBytes {
		return nil, []string{FindingFileTooLarge}
	}
	if !utf8.Valid(content) {
		return nil, []string{FindingFileNotUTF8}
	}
	front, body, ok := splitFrontmatter(string(content))
	if !ok {
		return nil, []string{FindingFrontmatterMissing}
	}
	if len([]rune(body)) > maxInstructionRunes {
		return nil, []string{FindingBodyTooLarge}
	}
	var root yaml.Node
	if err := yaml.Unmarshal([]byte(front), &root); err != nil {
		return nil, []string{FindingFrontmatterMalformed}
	}
	if root.Kind != yaml.DocumentNode || len(root.Content) != 1 || root.Content[0].Kind != yaml.MappingNode {
		return nil, []string{FindingFrontmatterMalformed}
	}
	walker := &yamlBudget{active: make(map[string]bool)}
	fields := make(map[string]any, 16)
	if err := walker.mapping(root.Content[0], 0, fields); err != nil {
		return nil, []string{walkerFinding(err)}
	}
	built := &Manifest{
		Metadata:      make(map[string]string),
		Aura:          make(map[string]string),
		UnknownFields: make(map[string]string),
		Body:          body,
	}
	if findings := built.bind(fields); len(findings) > 0 {
		return nil, findings
	}
	return built, nil
}

func walkerFinding(err error) string {
	var limit *yamlLimitError
	if errors.As(err, &limit) {
		return limit.finding
	}
	return FindingFrontmatterMalformed
}

func splitFrontmatter(content string) (front, body string, ok bool) {
	lines := strings.SplitAfter(content, "\n")
	if len(lines) == 0 || strings.TrimRight(lines[0], "\r\n") != "---" {
		return "", "", false
	}
	for index := 1; index < len(lines); index++ {
		line := strings.TrimRight(lines[index], "\r\n")
		if line == "---" || line == "..." {
			front := strings.Join(lines[1:index], "")
			body := strings.Join(lines[index+1:], "")
			return front, body, true
		}
	}
	return "", "", false
}

func (w *yamlBudget) mapping(node *yaml.Node, depth int, out map[string]any) error {
	pairs := node.Content
	for index := 0; index+1 < len(pairs); index += 2 {
		keyNode := pairs[index]
		if keyNode.Kind != yaml.ScalarNode {
			return &yamlLimitError{finding: FindingFrontmatterMalformed}
		}
		value, err := w.value(pairs[index+1], depth+1)
		if err != nil {
			return err
		}
		out[keyNode.Value] = value
	}
	return nil
}

func (w *yamlBudget) value(node *yaml.Node, depth int) (any, error) {
	w.nodes++
	if w.nodes > maxYAMLNodes {
		return nil, &yamlLimitError{finding: FindingYAMLTooManyNodes}
	}
	switch node.Kind {
	case yaml.ScalarNode:
		if node.Tag == "!!str" {
			return node.Value, nil
		}
		return nonStringScalar{tag: node.Tag, text: node.Value}, nil
	case yaml.MappingNode:
		if depth > maxYAMLDepth {
			return nil, &yamlLimitError{finding: FindingYAMLTooDeep}
		}
		out := make(map[string]any, len(node.Content)/2)
		if err := w.mapping(node, depth, out); err != nil {
			return nil, err
		}
		return out, nil
	case yaml.SequenceNode:
		if depth > maxYAMLDepth {
			return nil, &yamlLimitError{finding: FindingYAMLTooDeep}
		}
		out := make([]any, 0, len(node.Content))
		for _, item := range node.Content {
			value, err := w.value(item, depth+1)
			if err != nil {
				return nil, err
			}
			out = append(out, value)
		}
		return out, nil
	case yaml.AliasNode:
		w.aliases++
		if w.aliases > maxYAMLAliases {
			return nil, &yamlLimitError{finding: FindingYAMLTooManyAliases}
		}
		if node.Alias == nil {
			return nil, &yamlLimitError{finding: FindingFrontmatterMalformed}
		}
		if w.active[node.Value] {
			return nil, &yamlLimitError{finding: FindingFrontmatterMalformed}
		}
		w.active[node.Value] = true
		defer delete(w.active, node.Value)
		return w.value(node.Alias, depth)
	default:
		return nil, &yamlLimitError{finding: FindingFrontmatterMalformed}
	}
}

func (m *Manifest) bind(fields map[string]any) []string {
	findings := make([]string, 0, 4)
	name, namePresent, nameText := requiredText(fields, "name")
	switch {
	case !namePresent:
		findings = append(findings, FindingNameMissing)
	case !nameText || len([]rune(name)) > maxNameRunes || !validNamePattern.MatchString(name):
		findings = append(findings, FindingNameInvalid)
	default:
		m.Name = name
	}
	description, descPresent, descText := requiredText(fields, "description")
	switch {
	case !descPresent:
		findings = append(findings, FindingDescriptionMissing)
	case !descText || len([]rune(description)) == 0 || len([]rune(description)) > maxDescriptionRunes:
		findings = append(findings, FindingDescriptionInvalid)
	default:
		m.Description = description
	}
	if raw, present := fields["license"]; present {
		license, ok := raw.(string)
		if !ok || len([]rune(license)) == 0 || len([]rune(license)) > maxLicenseRunes {
			findings = append(findings, FindingLicenseInvalid)
		} else {
			m.License = license
		}
	}
	if raw, present := fields["compatibility"]; present {
		compat, ok := raw.(string)
		if !ok || len([]rune(compat)) == 0 || len([]rune(compat)) > maxCompatibilityRunes {
			findings = append(findings, FindingCompatibilityInvalid)
		} else {
			m.Compatibility = compat
		}
	}
	if raw, present := fields["allowed-tools"]; present {
		tools, ok := parseAllowedTools(raw)
		if !ok {
			findings = append(findings, FindingAllowedToolsInvalid)
		} else {
			m.AllowedTools = tools
		}
	}
	if raw, present := fields["metadata"]; present {
		if err := bindMetadata(raw, m.Metadata, m.Aura); err != nil {
			findings = append(findings, err.finding)
		}
	}
	for key, raw := range fields {
		if isStandardField(key) {
			continue
		}
		if len(m.UnknownFields) >= maxUnknownFields {
			findings = append(findings, FindingUnknownFieldsExceeded)
			break
		}
		rendered, ok := renderBounded(raw)
		if !ok {
			findings = append(findings, FindingUnknownFieldsExceeded)
			break
		}
		m.UnknownFields[key] = rendered
	}
	if len(findings) > 0 {
		return findings
	}
	return nil
}

func isStandardField(key string) bool {
	switch key {
	case "name", "description", "license", "compatibility", "metadata", "allowed-tools":
		return true
	default:
		return false
	}
}

func requiredText(fields map[string]any, key string) (text string, present, isText bool) {
	raw, present := fields[key]
	if !present {
		return "", false, false
	}
	value, ok := raw.(string)
	return value, true, ok
}

func parseAllowedTools(raw any) ([]string, bool) {
	text, ok := raw.(string)
	if !ok || len(text) > maxAllowedToolsBytes {
		return nil, false
	}
	fields := strings.Fields(text)
	if len(fields) > maxAllowedTools {
		return nil, false
	}
	return fields, true
}

type metadataError struct {
	finding string
}

func (e *metadataError) Error() string {
	return e.finding
}

func bindMetadata(raw any, flat, aura map[string]string) *metadataError {
	mapping, ok := raw.(map[string]any)
	if !ok {
		return &metadataError{finding: FindingMetadataInvalid}
	}
	if err := flattenMetadata(mapping, "", 0, flat); err != nil {
		return err
	}
	nested, ok := mapping["aura"]
	if !ok {
		return nil
	}
	auraMapping, ok := nested.(map[string]any)
	if !ok {
		return &metadataError{finding: FindingAuraMetadataInvalid}
	}
	version, ok := auraMapping["version"].(string)
	if !ok || version == "" {
		return &metadataError{finding: FindingAuraMetadataInvalid}
	}
	relocated := make(map[string]string, len(flat))
	if err := flattenMetadata(auraMapping, "", 0, relocated); err != nil {
		return err
	}
	for key, value := range relocated {
		aura[key] = value
	}
	return nil
}

func flattenMetadata(mapping map[string]any, prefix string, depth int, out map[string]string) *metadataError {
	if depth > maxMetadataDepth {
		return &metadataError{finding: FindingMetadataInvalid}
	}
	for key, raw := range mapping {
		if len(out) >= maxMetadataKeys {
			return &metadataError{finding: FindingMetadataInvalid}
		}
		path := key
		if prefix != "" {
			path = prefix + "." + key
		}
		switch value := raw.(type) {
		case string:
			if len(value) > maxMetadataBytes {
				return &metadataError{finding: FindingMetadataInvalid}
			}
			out[path] = value
		case map[string]any:
			if err := flattenMetadata(value, path, depth+1, out); err != nil {
				return err
			}
		default:
			return &metadataError{finding: FindingMetadataInvalid}
		}
	}
	total := 0
	for key, value := range out {
		total += len(key) + len(value)
	}
	if total > maxMetadataBytes {
		return &metadataError{finding: FindingMetadataInvalid}
	}
	return nil
}

func renderBounded(raw any) (string, bool) {
	if text, ok := raw.(string); ok {
		if len(text) > maxUnknownFieldBytes {
			return "", false
		}
		return text, true
	}
	if scalar, ok := raw.(nonStringScalar); ok {
		if len(scalar.text) > maxUnknownFieldBytes {
			return "", false
		}
		return scalar.text, true
	}
	encoded, err := json.Marshal(raw)
	if err != nil || len(encoded) > maxUnknownFieldBytes {
		return "", false
	}
	return string(encoded), true
}
