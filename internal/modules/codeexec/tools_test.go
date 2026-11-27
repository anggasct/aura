package codeexec

import (
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/anggasct/aura/internal/platform/toolbroker"
	"github.com/anggasct/aura/internal/platform/tools"
)

func TestCodeExecuteDefinition(t *testing.T) {
	definition := ToolDefinition()
	if definition.Name != ToolName || definition.Version != tools.Version || definition.Validator == nil {
		t.Fatalf("ToolDefinition() = %+v", definition)
	}
	if !definition.Effectful || !definition.RequiresApproval {
		t.Errorf("code_execute must be effectful and need approval: %+v", definition)
	}
	if !slices.Equal(definition.RequiredCapabilities, []string{CapabilityName}) {
		t.Errorf("required capabilities = %v", definition.RequiredCapabilities)
	}
	if definition.ExternalSink {
		t.Error("code_execute has no network and is not an external sink")
	}
}

func TestCodeExecuteSchemaListsEveryRuntimeAndNothingElse(t *testing.T) {
	var schema struct {
		Properties map[string]struct {
			Enum      []string `json:"enum"`
			MinLength int      `json:"minLength"`
			Type      string   `json:"type"`
		} `json:"properties"`
		Required             []string `json:"required"`
		AdditionalProperties bool     `json:"additionalProperties"`
	}
	if err := json.Unmarshal(codeExecuteSchema(), &schema); err != nil {
		t.Fatalf("schema: %v", err)
	}
	if !slices.Equal(schema.Properties["runtime"].Enum, CodeRuntimes()) || !slices.Equal(CodeRuntimes(), []string{"python", "javascript", "go"}) {
		t.Errorf("runtime enum = %v, runtimes = %v", schema.Properties["runtime"].Enum, CodeRuntimes())
	}
	if len(schema.Properties) != 4 || schema.Properties["output_files"].Type != "array" || schema.Properties["source"].MinLength != 1 || schema.Properties["stdin"].Type != "string" {
		t.Errorf("properties = %+v", schema.Properties)
	}
	if !slices.Equal(schema.Required, []string{"runtime", "source"}) || schema.AdditionalProperties {
		t.Errorf("required = %v, additionalProperties = %v", schema.Required, schema.AdditionalProperties)
	}
	for _, forbidden := range []string{"args", "env", "path"} {
		if _, present := schema.Properties[forbidden]; present {
			t.Errorf("the schema accepts %q", forbidden)
		}
	}
}

func TestCodeExecuteToolDefinitionMatchesBuiltinCatalog(t *testing.T) {
	got := ToolDefinition()
	if got.Validator == nil {
		t.Fatal("ToolDefinition().Validator must not be nil")
	}

	maxFiles := make([]string, 0, MaxCodeOutputFiles+1)
	for i := range MaxCodeOutputFiles {
		maxFiles = append(maxFiles, fmt.Sprintf("f%03d.txt", i))
	}
	maxFilesJSON, err := json.Marshal(maxFiles)
	if err != nil {
		t.Fatal(err)
	}
	tooManyFilesJSON, err := json.Marshal(append(maxFiles, "extra.txt"))
	if err != nil {
		t.Fatal(err)
	}

	payloads := []struct {
		name string
		raw  string
		ok   bool
	}{
		{name: "minimal python", raw: `{"runtime":"python","source":"print(1)"}`, ok: true},
		{name: "minimal javascript", raw: `{"runtime":"javascript","source":"console.log(1)"}`, ok: true},
		{name: "minimal go", raw: `{"runtime":"go","source":"package main"}`, ok: true},
		{name: "with empty stdin", raw: `{"runtime":"python","source":"x","stdin":""}`, ok: true},
		{name: "with stdin and outputs", raw: `{"runtime":"python","source":"x","stdin":"hi","output_files":["a.txt","sub/b.csv","a/b/c/d.bin"]}`, ok: true},
		{name: "empty output files", raw: `{"runtime":"python","source":"x","output_files":[]}`, ok: true},
		{name: "max output files", raw: `{"runtime":"python","source":"x","output_files":` + string(maxFilesJSON) + `}`, ok: true},
		{name: "max source bytes", raw: fmt.Sprintf(`{"runtime":"python","source":%q}`, strings.Repeat("a", MaxCodeSourceBytes)), ok: true},
		{name: "over max source bytes", raw: fmt.Sprintf(`{"runtime":"python","source":%q}`, strings.Repeat("a", MaxCodeSourceBytes+1)), ok: false},
		{name: "over max stdin bytes", raw: fmt.Sprintf(`{"runtime":"python","source":"x","stdin":%q}`, strings.Repeat("b", MaxCodeStdinBytes+1)), ok: false},
		{name: "non-string stdin", raw: `{"runtime":"python","source":"x","stdin":123}`, ok: false},
		{name: "unknown runtime", raw: `{"runtime":"ruby","source":"puts 1"}`, ok: false},
		{name: "empty runtime", raw: `{"runtime":"","source":"x"}`, ok: false},
		{name: "missing runtime", raw: `{"source":"x"}`, ok: false},
		{name: "empty source", raw: `{"runtime":"python","source":""}`, ok: false},
		{name: "missing source", raw: `{"runtime":"python"}`, ok: false},
		{name: "unknown field", raw: `{"runtime":"python","source":"x","env":{}}`, ok: false},
		{name: "output files non-array", raw: `{"runtime":"python","source":"x","output_files":"a.txt"}`, ok: false},
		{name: "output files non-string item", raw: `{"runtime":"python","source":"x","output_files":[1]}`, ok: false},
		{name: "output files too many", raw: `{"runtime":"python","source":"x","output_files":` + string(tooManyFilesJSON) + `}`, ok: false},
		{name: "output files duplicate", raw: `{"runtime":"python","source":"x","output_files":["a.txt","a.txt"]}`, ok: false},
		{name: "output files dotdot", raw: `{"runtime":"python","source":"x","output_files":["../a.txt"]}`, ok: false},
		{name: "output files absolute", raw: `{"runtime":"python","source":"x","output_files":["/a.txt"]}`, ok: false},
		{name: "output files too deep", raw: `{"runtime":"python","source":"x","output_files":["a/b/c/d/e.txt"]}`, ok: false},
		{name: "output files bad char", raw: `{"runtime":"python","source":"x","output_files":["a b.txt"]}`, ok: false},
		{name: "output files over max name bytes", raw: fmt.Sprintf(`{"runtime":"python","source":"x","output_files":[%q]}`, strings.Repeat("a", MaxCodeOutputNameBytes+1)), ok: false},
		{name: "trailing json", raw: `{"runtime":"python","source":"x"} {}`, ok: false},
		{name: "not an object", raw: `["python"]`, ok: false},
		{name: "invalid json", raw: `{`, ok: false},
	}
	for _, tc := range payloads {
		t.Run(tc.name, func(t *testing.T) {
			gotOut, gotErr := got.Validate(json.RawMessage(tc.raw))
			if (gotErr == nil) != tc.ok {
				t.Fatalf("got.Validate(%s) err = %v, want ok=%t", tc.raw, gotErr, tc.ok)
			}
			if tc.ok && !json.Valid(gotOut) {
				t.Fatalf("validated output is not valid JSON: %s", gotOut)
			}
		})
	}
}

func TestCodeExecuteValidation(t *testing.T) {
	definition := ToolDefinition()
	cases := []struct {
		name string
		raw  string
		ok   bool
	}{
		{"python", `{"runtime":"python","source":"print(1)"}`, true},
		{"javascript with stdin", `{"runtime":"javascript","source":"1","stdin":"in"}`, true},
		{"go is accepted", `{"runtime":"go","source":"package main"}`, true},
		{"empty stdin", `{"runtime":"python","source":"x","stdin":""}`, true},
		{"unknown runtime", `{"runtime":"ruby","source":"x"}`, false},
		{"runtime case", `{"runtime":"Python","source":"x"}`, false},
		{"missing runtime", `{"source":"x"}`, false},
		{"missing source", `{"runtime":"python"}`, false},
		{"empty source", `{"runtime":"python","source":""}`, false},
		{"numeric source", `{"runtime":"python","source":1}`, false},
		{"numeric stdin", `{"runtime":"python","source":"x","stdin":1}`, false},
		{"null stdin", `{"runtime":"python","source":"x","stdin":null}`, false},
		{"environment", `{"runtime":"python","source":"x","env":{"A":"b"}}`, false},
		{"arguments", `{"runtime":"python","source":"x","args":["a"]}`, false},
		{"output files", `{"runtime":"python","source":"x","output_files":["a.txt","plots/b.png"]}`, true},
		{"no output files", `{"runtime":"python","source":"x","output_files":[]}`, true},
		{"null output files", `{"runtime":"python","source":"x","output_files":null}`, false},
		{"output files as a string", `{"runtime":"python","source":"x","output_files":"a.txt"}`, false},
		{"output file number", `{"runtime":"python","source":"x","output_files":[1]}`, false},
		{"output file parent", `{"runtime":"python","source":"x","output_files":["../a.txt"]}`, false},
		{"output file absolute", `{"runtime":"python","source":"x","output_files":["/etc/passwd"]}`, false},
		{"output file repeated", `{"runtime":"python","source":"x","output_files":["a","a"]}`, false},
		{"too many output files", `{"runtime":"python","source":"x","output_files":[` + strings.TrimSuffix(strings.Repeat(`"a",`, MaxCodeOutputFiles+1), ",") + `]}`, false},
		{"source above its bound", `{"runtime":"python","source":"` + strings.Repeat("x", MaxCodeSourceBytes+1) + `"}`, false},
		{"source at its bound", `{"runtime":"python","source":"` + strings.Repeat("x", MaxCodeSourceBytes) + `"}`, true},
		{"stdin above its bound", `{"runtime":"python","source":"x","stdin":"` + strings.Repeat("x", MaxCodeStdinBytes+1) + `"}`, false},
		{"not an object", `"python"`, false},
		{"trailing content", `{"runtime":"python","source":"x"}{}`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := definition.Validate(json.RawMessage(tc.raw))
			if (err == nil) != tc.ok {
				t.Fatalf("Validate = %v, want ok = %v", err, tc.ok)
			}
			if err != nil && strings.Contains(err.Error(), "xxxx") {
				t.Fatalf("the error echoes the program: %v", err)
			}
			if err == nil && !json.Valid(got) {
				t.Fatalf("canonical arguments are not JSON: %s", got)
			}
		})
	}
}

func TestValidateCodeOutputFile(t *testing.T) {
	cases := []struct {
		name string
		file string
		ok   bool
	}{
		{"plain", "result.csv", true},
		{"nested", "plots/2026/a.png", true},
		{"dashes and underscores", "a-b_c.txt", true},
		{"hidden", ".hidden", true},
		{"at the depth bound", "a/b/c/d", true},
		{"above the depth bound", "a/b/c/d/e", false},
		{"at the length bound", strings.Repeat("a", MaxCodeOutputNameBytes), true},
		{"above the length bound", strings.Repeat("a", MaxCodeOutputNameBytes+1), false},
		{"empty", "", false},
		{"current directory", ".", false},
		{"parent", "..", false},
		{"parent segment", "a/../b", false},
		{"leading parent", "../b", false},
		{"absolute", "/a", false},
		{"trailing slash", "a/", false},
		{"empty segment", "a//b", false},
		{"backslash", `a\b`, false},
		{"space", "a b", false},
		{"newline", "a\nb", false},
		{"null byte", "a\x00b", false},
		{"non ascii", "é.txt", false},
		{"glob", "*.txt", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateCodeOutputFile(tc.file)
			if (err == nil) != tc.ok {
				t.Fatalf("ValidateCodeOutputFile(%q) = %v, want ok = %v", tc.file, err, tc.ok)
			}
			if err != nil && tc.file != "" && strings.Contains(err.Error(), tc.file) {
				t.Fatalf("the error echoes the name: %v", err)
			}
		})
	}
}

func TestValidateCodeOutputFilesBoundsTheListAndRejectsRepeats(t *testing.T) {
	cases := []struct {
		name  string
		files []string
		limit int
		ok    bool
	}{
		{"none", nil, 2, true},
		{"at the limit", []string{"a", "b"}, 2, true},
		{"above the limit", []string{"a", "b", "c"}, 2, false},
		{"repeat", []string{"a", "b", "a"}, 3, false},
		{"invalid entry", []string{"a", "../b"}, 3, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := ValidateCodeOutputFiles(tc.files, tc.limit); (err == nil) != tc.ok {
				t.Fatalf("ValidateCodeOutputFiles = %v, want ok = %v", err, tc.ok)
			}
		})
	}
}

func FuzzCodeExecuteOutputFiles(f *testing.F) {
	definition := ToolDefinition()
	for _, seed := range []string{
		`{"runtime":"python","source":"x","output_files":["a.txt"]}`,
		`{"runtime":"python","source":"x","output_files":["../a","/b","a//b",".",""]}`,
		`{"runtime":"python","source":"x","output_files":["a","a"]}`,
		`{"runtime":"python","source":"x","output_files":[1,null,{}]}`,
		`{"runtime":"python","source":"x","output_files":"a"}`,
	} {
		f.Add([]byte(seed))
	}
	f.Fuzz(func(t *testing.T, raw []byte) {
		canonical, err := definition.Validate(json.RawMessage(raw))
		if err != nil {
			return
		}
		var decoded struct {
			OutputFiles []string `json:"output_files"`
		}
		if err := json.Unmarshal(canonical, &decoded); err != nil {
			t.Fatalf("accepted arguments do not decode: %v", err)
		}
		if err := ValidateCodeOutputFiles(decoded.OutputFiles, MaxCodeOutputFiles); err != nil {
			t.Fatalf("accepted arguments carry an invalid output list: %v", err)
		}
		for _, name := range decoded.OutputFiles {
			if strings.HasPrefix(name, "/") || slices.Contains(strings.Split(name, "/"), "..") {
				t.Fatalf("accepted an escaping name %q", name)
			}
		}
	})
}

func TestCodeExecuteToolRuleMatchesDefaultPolicy(t *testing.T) {
	def := ToolDefinition()
	want := toolbroker.DefaultRule(&def)
	got := ToolRule()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ToolRule() = %+v, want %+v", got, want)
	}
}
