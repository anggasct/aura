package codeexec

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestEncodedResultKeepsTheContractKeyOrder(t *testing.T) {
	outcome := Outcome{Class: ClassOK, Stdout: "out", Stderr: "err"}
	encoded, err := encodeBounded(&outcome, &Limits{MaxStdoutBytes: 1 << 10, MaxStderrBytes: 1 << 10}, 1<<16)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"result_class":"ok","exit_code":0,"signal":0,"duration_ms":0,"artifacts":[],"stdout_truncated":false,"stderr_truncated":false,"stdout":"out","stderr":"err"}`
	if string(encoded) != want {
		t.Fatalf("encoded = %s\nwant      %s", encoded, want)
	}
}

func TestEncodedResultStaysValidAndBounded(t *testing.T) {
	const bound = 4 << 10
	cases := []struct {
		name          string
		stdout        string
		stderr        string
		outLimit      int64
		errLimit      int64
		wantStdoutCut bool
		wantStderrCut bool
	}{
		{"fits untouched", "hello", "world", 1 << 10, 1 << 10, false, false},
		{"stdout above its own limit", strings.Repeat("a", 3<<10), "", 1 << 10, 1 << 10, true, false},
		{"stderr above its own limit", "", strings.Repeat("b", 3<<10), 1 << 10, 1 << 10, false, true},
		{"both within their limits but above the result bound", strings.Repeat("a", 3<<10), strings.Repeat("b", 3<<10), 3 << 10, 3 << 10, true, false},
		{"control characters expand when escaped", strings.Repeat("\x01", 3<<10), "", 3 << 10, 1 << 10, true, false},
		{"quotes and backslashes expand when escaped", strings.Repeat(`"\`, 2<<10), strings.Repeat(`\`, 2<<10), 4 << 10, 4 << 10, true, true},
		{"multi-byte runes are never split", strings.Repeat("é你🙂", 2<<10), "", 8 << 10, 1 << 10, true, false},
		{"invalid utf-8 is replaced", "ok\xff\xfe", "\xc3", 1 << 10, 1 << 10, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			outcome := Outcome{Class: ClassExitNonzero, ExitCode: 1, Duration: 1500000, Stdout: tc.stdout, Stderr: tc.stderr}
			encoded, err := encodeBounded(&outcome, &Limits{MaxStdoutBytes: tc.outLimit, MaxStderrBytes: tc.errLimit}, bound)
			if err != nil {
				t.Fatalf("encodeBounded: %v", err)
			}
			if len(encoded) > bound {
				t.Fatalf("encoded %d bytes, bound is %d", len(encoded), bound)
			}
			if !json.Valid(encoded) {
				t.Fatalf("result is not valid JSON: %q", encoded)
			}
			var decoded encodedResult
			if err := json.Unmarshal(encoded, &decoded); err != nil {
				t.Fatal(err)
			}
			if !utf8.ValidString(decoded.Stdout) || !utf8.ValidString(decoded.Stderr) {
				t.Fatalf("decoded text is not valid UTF-8: %q %q", decoded.Stdout, decoded.Stderr)
			}
			if decoded.StdoutTruncated != tc.wantStdoutCut || decoded.StderrTruncated != tc.wantStderrCut {
				t.Fatalf("truncated flags = %v %v, want %v %v", decoded.StdoutTruncated, decoded.StderrTruncated, tc.wantStdoutCut, tc.wantStderrCut)
			}
			if decoded.ResultClass != ClassExitNonzero || decoded.ExitCode != 1 || decoded.DurationMS != 1 {
				t.Fatalf("scalar fields changed: %+v", decoded)
			}
			if utf8.ValidString(tc.stdout) && !tc.wantStdoutCut && decoded.Stdout != tc.stdout {
				t.Fatalf("stdout changed without being flagged: %q", decoded.Stdout)
			}
			if utf8.ValidString(tc.stdout) && tc.wantStdoutCut && !strings.HasPrefix(tc.stdout, decoded.Stdout) {
				t.Fatalf("a cut stdout must stay a prefix of the original: %q", decoded.Stdout)
			}
		})
	}
}

func TestEncodedResultKeepsTheSandboxTruncationFlags(t *testing.T) {
	outcome := Outcome{Class: ClassOK, Stdout: "x", StdoutTruncated: true, StderrTruncated: true}
	encoded, err := encodeBounded(&outcome, &Limits{MaxStdoutBytes: 10, MaxStderrBytes: 10}, 1<<10)
	if err != nil {
		t.Fatal(err)
	}
	var decoded encodedResult
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	if !decoded.StdoutTruncated || !decoded.StderrTruncated {
		t.Fatalf("flags = %v %v, want both kept", decoded.StdoutTruncated, decoded.StderrTruncated)
	}
}

func TestEncodedResultTrimsTheLargerStreamFirst(t *testing.T) {
	outcome := Outcome{Class: ClassOK, Stdout: strings.Repeat("a", 900), Stderr: strings.Repeat("b", 100)}
	encoded, err := encodeBounded(&outcome, &Limits{MaxStdoutBytes: 1 << 10, MaxStderrBytes: 1 << 10}, 600)
	if err != nil {
		t.Fatal(err)
	}
	var decoded encodedResult
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	if len(decoded.Stderr) != 100 || decoded.StderrTruncated || !decoded.StdoutTruncated {
		t.Fatalf("stdout %d bytes (cut %v), stderr %d bytes (cut %v)", len(decoded.Stdout), decoded.StdoutTruncated, len(decoded.Stderr), decoded.StderrTruncated)
	}
}

func TestEncodedResultRejectsAnImpossibleBound(t *testing.T) {
	outcome := Outcome{Class: ClassOK, Stdout: "x"}
	for _, bound := range []int64{0, -1, 10} {
		if _, err := encodeBounded(&outcome, &Limits{MaxStdoutBytes: 10, MaxStderrBytes: 10}, bound); err == nil {
			t.Errorf("a bound of %d produced a result", bound)
		}
	}
}

func TestEncodedResultDoesNotEscapeHTML(t *testing.T) {
	outcome := Outcome{Class: ClassOK, Stdout: "<a href='x'>&</a>"}
	encoded, err := encodeBounded(&outcome, &Limits{MaxStdoutBytes: 1 << 10, MaxStderrBytes: 1 << 10}, 1<<10)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(encoded, []byte("<a href='x'>&</a>")) {
		t.Fatalf("encoded = %s", encoded)
	}
}
