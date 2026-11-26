package codeexec

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/anggasct/aura/internal/platform/toolbroker"
	"github.com/anggasct/aura/internal/platform/tools"
)

func TestCodeExecuteToolDefinitionMatchesBuiltinCatalog(t *testing.T) {
	wantPtr, ok := tools.DefinitionsByKey()[ToolName+"@"+tools.Version]
	if !ok {
		t.Fatal("code_execute@v1 missing from builtin catalog")
	}
	got := ToolDefinition()
	if got.Validator == nil {
		t.Fatal("ToolDefinition().Validator must not be nil")
	}

	gotCopy := got
	gotCopy.Validator = nil
	wantCopy := *wantPtr
	wantCopy.Validator = nil
	if !reflect.DeepEqual(gotCopy, wantCopy) {
		t.Fatalf("ToolDefinition() non-func fields differ from catalog:\n got: %+v\nwant: %+v", gotCopy, wantCopy)
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
			wantOut, wantErr := wantPtr.Validate(json.RawMessage(tc.raw))
			if (gotErr == nil) != tc.ok {
				t.Fatalf("got.Validate(%s) err = %v, want ok=%t", tc.raw, gotErr, tc.ok)
			}
			if (gotErr == nil) != (wantErr == nil) {
				t.Fatalf("parity mismatch for %s: gotErr=%v wantErr=%v", tc.raw, gotErr, wantErr)
			}
			if tc.ok && !bytes.Equal(gotOut, wantOut) {
				t.Fatalf("validated output mismatch: got %s, want %s", gotOut, wantOut)
			}
		})
	}
}

func TestCodeExecuteToolRuleMatchesDefaultPolicy(t *testing.T) {
	want, ok := toolbroker.DefaultPolicy().Rules[ToolName]
	if !ok {
		t.Fatal("code_execute missing from DefaultPolicy")
	}
	got := ToolRule()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ToolRule() = %+v, want %+v", got, want)
	}
}
