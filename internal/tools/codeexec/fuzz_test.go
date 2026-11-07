package codeexec

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/anggasct/aura/internal/tools"
)

const fuzzSecretLength = 64

func FuzzCodeExecuteRequest(f *testing.F) {
	for _, seed := range []string{
		`{"runtime":"python","source":"print(1)"}`,
		`{"runtime":"go","source":"package main","stdin":"in","output_files":["a.txt","sub/b.csv"]}`,
		`{"runtime":"javascript","source":"x","env":{"A":"b"}}`,
		`{"runtime":"python","source":"x","output_files":["../a","a","a"]}`,
		`{"runtime":"python","source":"x"} {}`,
		`{"runtime":"python","source":1}`,
		`{"runtime":"ruby","source":"x"}`,
		`null`,
		`[]`,
		``,
	} {
		f.Add([]byte(seed))
	}
	limits := rigLimits()
	runner := &Runner{limits: limits}
	definition := tools.DefinitionsByKey()[auditToolKey]
	f.Fuzz(func(t *testing.T, raw []byte) {
		checked := runner.ArgumentCheck(json.RawMessage(raw))
		decoded, decodeErr := decodeArguments(json.RawMessage(raw))
		if checked == nil {
			if decodeErr != nil {
				t.Fatalf("ArgumentCheck accepted arguments that do not decode: %v", decodeErr)
			}
			if int64(len(decoded.Source)) > limits.MaxSourceBytes || int64(len(decoded.Stdin)) > limits.MaxStdinBytes {
				t.Fatalf("ArgumentCheck accepted source %d or stdin %d above the limits", len(decoded.Source), len(decoded.Stdin))
			}
			if err := tools.ValidateCodeOutputFiles(decoded.OutputFiles, int(limits.MaxOutputFiles)); err != nil {
				t.Fatalf("ArgumentCheck accepted an invalid output list: %v", err)
			}
		} else {
			if !hasErrorCode(checked, ErrorCodeInvalidArgument) {
				t.Fatalf("ArgumentCheck error %v is not invalid_argument", checked)
			}
			if len(decoded.Source) > fuzzSecretLength && strings.Contains(checked.Error(), decoded.Source) {
				t.Fatal("the refusal echoes the source")
			}
			if len(decoded.Stdin) > fuzzSecretLength && strings.Contains(checked.Error(), decoded.Stdin) {
				t.Fatal("the refusal echoes the stdin")
			}
		}
		canonical, err := definition.Validate(json.RawMessage(raw))
		if err != nil {
			return
		}
		fits := int64(len(decoded.Source)) <= limits.MaxSourceBytes && int64(len(decoded.Stdin)) <= limits.MaxStdinBytes &&
			int64(len(decoded.OutputFiles)) <= limits.MaxOutputFiles
		if recheck := runner.ArgumentCheck(canonical); fits && recheck != nil {
			t.Fatalf("the catalog accepted %s but the runner refused it: %v", canonical, recheck)
		}
	})
}

func FuzzEncodeBoundedResult(f *testing.F) {
	f.Add("hello", "world", "a.txt", int64(4096), int64(1024), int64(1024), uint8(2), true)
	f.Add(strings.Repeat("\x01", 3000), "", "", int64(2048), int64(4096), int64(4096), uint8(0), false)
	f.Add(strings.Repeat("é你🙂", 800), strings.Repeat(`"\`, 500), "sub/n.txt", int64(1500), int64(8192), int64(8192), uint8(8), true)
	f.Add("ok\xff\xfe", "\xc3", "", int64(300), int64(10), int64(10), uint8(1), false)
	f.Add("x", "y", "", int64(0), int64(1), int64(1), uint8(0), false)
	f.Fuzz(func(t *testing.T, stdout, stderr, name string, bound, stdoutLimit, stderrLimit int64, artifacts uint8, refused bool) {
		outcome := Outcome{Class: ClassExitNonzero, ExitCode: 1, Signal: 9, Stdout: stdout, Stderr: stderr}
		for range artifacts % 16 {
			entry := ArtifactEntry{Name: name, ArtifactID: "art-0123456789abcdef", Digest: strings.Repeat("a", 64), MediaType: "text/plain; charset=utf-8", Size: 12}
			if refused {
				entry = ArtifactEntry{Name: name, Refused: RefusalQuota}
			}
			outcome.Artifacts = append(outcome.Artifacts, entry)
		}
		limits := Limits{MaxStdoutBytes: stdoutLimit, MaxStderrBytes: stderrLimit}
		encoded, err := encodeBounded(&outcome, &limits, bound)
		if err != nil {
			if !hasErrorCode(err, ErrorCodeInvalidArgument) {
				t.Fatalf("error %v is not invalid_argument", err)
			}
			empty := outcome
			empty.Stdout, empty.Stderr = "", ""
			floor, floorErr := encodeBounded(&empty, &Limits{MaxStdoutBytes: 1, MaxStderrBytes: 1}, 1<<30)
			if bound > 0 && floorErr == nil && int64(len(floor)) <= bound {
				t.Fatalf("refused to encode although the empty envelope (%d bytes) fits the bound %d", len(floor), bound)
			}
			return
		}
		if int64(len(encoded)) > bound {
			t.Fatalf("encoded %d bytes above the bound %d", len(encoded), bound)
		}
		if !json.Valid(encoded) {
			t.Fatalf("result is not valid JSON: %q", encoded)
		}
		var decoded encodedResult
		if err := json.Unmarshal(encoded, &decoded); err != nil {
			t.Fatal(err)
		}
		if !utf8.ValidString(decoded.Stdout) || !utf8.ValidString(decoded.Stderr) {
			t.Fatal("the result carries invalid UTF-8")
		}
		if len(decoded.Artifacts) != len(outcome.Artifacts) {
			t.Fatalf("artifacts = %d, want %d: entries are never dropped", len(decoded.Artifacts), len(outcome.Artifacts))
		}
		if len(decoded.Stdout) < len(strings.ToValidUTF8(stdout, "�")) && !decoded.StdoutTruncated {
			t.Fatal("stdout was cut without the truncation flag")
		}
		if len(decoded.Stderr) < len(strings.ToValidUTF8(stderr, "�")) && !decoded.StderrTruncated {
			t.Fatal("stderr was cut without the truncation flag")
		}
	})
}
