package skills

import (
	"strings"
	"testing"
)

func TestParseSkillFileValid(t *testing.T) {
	cases := map[string]struct {
		document string
		check    func(t *testing.T, manifest *Manifest)
	}{
		"minimal": {
			document: "---\nname: pdf-tools\ndescription: Handle PDF files.\n---\n# Instructions\nDo things.\n",
			check: func(t *testing.T, manifest *Manifest) {
				t.Helper()
				if manifest.Name != "pdf-tools" || manifest.Description != "Handle PDF files." {
					t.Errorf("identity = %q/%q", manifest.Name, manifest.Description)
				}
				if manifest.Body != "# Instructions\nDo things.\n" {
					t.Errorf("body = %q", manifest.Body)
				}
			},
		},
		"full": {
			document: "---\nname: pdf-tools\ndescription: Handle PDF files.\nlicense: Apache-2.0\ncompatibility: Requires python3\nallowed-tools: read write\nmetadata:\n  author: example\n  aura:\n    version: \"1\"\n    profile: strict\n---\nBody.\n",
			check: func(t *testing.T, manifest *Manifest) {
				t.Helper()
				if manifest.License != "Apache-2.0" || manifest.Compatibility != "Requires python3" {
					t.Errorf("license/compat = %q/%q", manifest.License, manifest.Compatibility)
				}
				if len(manifest.AllowedTools) != 2 || manifest.AllowedTools[0] != "read" {
					t.Errorf("allowed-tools = %v", manifest.AllowedTools)
				}
				if manifest.Metadata["author"] != "example" || manifest.Aura["version"] != "1" {
					t.Errorf("metadata = %v aura = %v", manifest.Metadata, manifest.Aura)
				}
			},
		},
		"invented fields stay unknown": {
			document: "---\nname: pdf-tools\ndescription: Handle PDF files.\ntriggers:\n  - pdf\ntools:\n  - exec\n---\nBody.\n",
			check: func(t *testing.T, manifest *Manifest) {
				t.Helper()
				if manifest.Name != "pdf-tools" {
					t.Errorf("name = %q", manifest.Name)
				}
				if _, ok := manifest.UnknownFields["triggers"]; !ok {
					t.Errorf("triggers not preserved as unknown: %v", manifest.UnknownFields)
				}
				if _, ok := manifest.UnknownFields["tools"]; !ok {
					t.Errorf("tools not preserved as unknown: %v", manifest.UnknownFields)
				}
			},
		},
		"crlf and dot fence": {
			document: "---\r\nname: pdf-tools\r\ndescription: Handle PDF files.\r\n...\r\nBody.\r\n",
			check: func(t *testing.T, manifest *Manifest) {
				t.Helper()
				if manifest.Name != "pdf-tools" || manifest.Body != "Body.\r\n" {
					t.Errorf("name/body = %q/%q", manifest.Name, manifest.Body)
				}
			},
		},
		"empty body": {
			document: "---\nname: pdf-tools\ndescription: Handle PDF files.\n---\n",
			check: func(t *testing.T, manifest *Manifest) {
				t.Helper()
				if manifest.Body != "" {
					t.Errorf("body = %q", manifest.Body)
				}
			},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			manifest, findings := ParseSkillFile([]byte(tc.document))
			if len(findings) != 0 {
				t.Fatalf("findings = %v", findings)
			}
			if manifest == nil {
				t.Fatal("manifest is nil")
			}
			tc.check(t, manifest)
		})
	}
}

func TestParseSkillFileInvalid(t *testing.T) {
	var bomb strings.Builder
	bomb.WriteString("---\nname: x\ndescription: y\nanchor: &a [v]\n")
	for index := range maxYAMLAliases + 2 {
		bomb.WriteString("ref" + strings.Repeat("r", index) + ": *a\n")
	}
	var wide strings.Builder
	wide.WriteString("---\nname: x\ndescription: y\ndata: [")
	for index := range maxYAMLNodes + 1 {
		if index > 0 {
			wide.WriteString(",")
		}
		wide.WriteString("1")
	}
	wide.WriteString("]\n---\n")
	var deep strings.Builder
	deep.WriteString("---\nname: x\ndescription: y\n")
	for index := range maxYAMLDepth + 2 {
		deep.WriteString(strings.Repeat("  ", index) + "k:\n")
	}
	deep.WriteString(strings.Repeat("  ", maxYAMLDepth+2) + "v: 1\n---\n")
	cases := map[string]struct {
		document string
		raw      []byte
		finding  string
	}{
		"missing frontmatter":     {document: "# Just markdown\n", finding: FindingFrontmatterMissing},
		"unclosed frontmatter":    {document: "---\nname: x\n", finding: FindingFrontmatterMissing},
		"malformed yaml":          {document: "---\nname: [unclosed\ndescription: y\n---\n", finding: FindingFrontmatterMalformed},
		"non mapping root":        {document: "---\n- a\n- b\n---\n", finding: FindingFrontmatterMalformed},
		"missing name":            {document: "---\ndescription: y\n---\n", finding: FindingNameMissing},
		"missing description":     {document: "---\nname: x\n---\n", finding: FindingDescriptionMissing},
		"uppercase name":          {document: "---\nname: Bad-Name\ndescription: y\n---\n", finding: FindingNameInvalid},
		"spaced name":             {document: "---\nname: bad name\ndescription: y\n---\n", finding: FindingNameInvalid},
		"long name":               {document: "---\nname: " + strings.Repeat("a", 65) + "\ndescription: y\n---\n", finding: FindingNameInvalid},
		"empty description":       {document: "---\nname: x\ndescription: ''\n---\n", finding: FindingDescriptionInvalid},
		"long description":        {document: "---\nname: x\ndescription: " + strings.Repeat("a", 1025) + "\n---\n", finding: FindingDescriptionInvalid},
		"numeric description":     {document: "---\nname: x\ndescription: 42\n---\n", finding: FindingDescriptionInvalid},
		"empty license":           {document: "---\nname: x\ndescription: y\nlicense: ''\n---\n", finding: FindingLicenseInvalid},
		"empty compatibility":     {document: "---\nname: x\ndescription: y\ncompatibility: ''\n---\n", finding: FindingCompatibilityInvalid},
		"mapping allowed tools":   {document: "---\nname: x\ndescription: y\nallowed-tools:\n  a: b\n---\n", finding: FindingAllowedToolsInvalid},
		"mapping metadata":        {document: "---\nname: x\ndescription: y\nmetadata: [a]\n---\n", finding: FindingMetadataInvalid},
		"sequence metadata value": {document: "---\nname: x\ndescription: y\nmetadata:\n  k: [1]\n---\n", finding: FindingMetadataInvalid},
		"aura not mapping":        {document: "---\nname: x\ndescription: y\nmetadata:\n  aura: nope\n---\n", finding: FindingAuraMetadataInvalid},
		"aura missing version":    {document: "---\nname: x\ndescription: y\nmetadata:\n  aura:\n    profile: strict\n---\n", finding: FindingAuraMetadataInvalid},
		"yaml bomb":               {document: bomb.String() + "---\n", finding: FindingYAMLTooManyAliases},
		"wide yaml":               {document: wide.String(), finding: FindingYAMLTooManyNodes},
		"deep yaml":               {document: deep.String(), finding: FindingYAMLTooDeep},
		"alias cycle":             {document: "---\nname: x\ndescription: y\na: &a\n  b: *a\n---\n", finding: FindingFrontmatterMalformed},
		"oversized body":          {document: "---\nname: x\ndescription: y\n---\n" + strings.Repeat("a", maxInstructionRunes+1), finding: FindingBodyTooLarge},
		"oversized file":          {raw: make([]byte, maxSkillFileBytes+1), finding: FindingFileTooLarge},
		"non utf8":                {raw: []byte("---\nname: \xff\n---\n"), finding: FindingFileNotUTF8},
		"too many unknown fields": {document: "---\nname: x\ndescription: y\n" + manyUnknownFields() + "---\n", finding: FindingUnknownFieldsExceeded},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			raw := tc.raw
			if raw == nil {
				raw = []byte(tc.document)
			}
			manifest, findings := ParseSkillFile(raw)
			if manifest != nil {
				t.Errorf("manifest should be nil for invalid input")
			}
			if !containsFinding(findings, tc.finding) {
				t.Errorf("findings = %v, want %s", findings, tc.finding)
			}
		})
	}
}

func manyUnknownFields() string {
	var builder strings.Builder
	for index := range maxUnknownFields + 1 {
		builder.WriteString("extra_field_" + strings.Repeat("x", index) + ": 1\n")
	}
	return builder.String()
}

func containsFinding(findings []string, want string) bool {
	for _, finding := range findings {
		if finding == want {
			return true
		}
	}
	return false
}
