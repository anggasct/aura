package logging

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
)

func TestParseLevel(t *testing.T) {
	cases := map[string]slog.Level{
		"debug": slog.LevelDebug,
		"INFO":  slog.LevelInfo,
		"warn":  slog.LevelWarn,
		"error": slog.LevelError,
	}
	for in, want := range cases {
		got, err := ParseLevel(in)
		if err != nil {
			t.Errorf("ParseLevel(%q): %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("ParseLevel(%q) = %v, want %v", in, got, want)
		}
	}
	for _, in := range []string{"", "verbose", "trace"} {
		if _, err := ParseLevel(in); err == nil {
			t.Errorf("ParseLevel(%q) expected error, got nil", in)
		}
	}
}

func TestParseFormat(t *testing.T) {
	for _, in := range []string{"text", "json", "TEXT", "Json"} {
		if _, err := ParseFormat(in); err != nil {
			t.Errorf("ParseFormat(%q): %v", in, err)
		}
	}
	for _, in := range []string{"", "xml", "yaml"} {
		if _, err := ParseFormat(in); err == nil {
			t.Errorf("ParseFormat(%q) expected error, got nil", in)
		}
	}
}

func TestNew_TextOutput(t *testing.T) {
	var buf bytes.Buffer
	l, err := New("info", "text", &buf)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	l.Info("hello world", "component", "test")
	out := buf.String()
	if !strings.Contains(out, "level=INFO") || !strings.Contains(out, "hello world") {
		t.Errorf("text output missing level/msg: %q", out)
	}
	if !strings.Contains(out, "component=test") {
		t.Errorf("text output missing component attr: %q", out)
	}
}

func TestNew_JSONOutput(t *testing.T) {
	var buf bytes.Buffer
	l, err := New("info", "json", &buf)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	l.Info("hello json", "component", "test")
	var m map[string]any
	if err := json.Unmarshal(buf.Bytes(), &m); err != nil {
		t.Fatalf("output is not valid JSON: %v\n%s", err, buf.String())
	}
	if m["msg"] != "hello json" || m["level"] != "INFO" || m["component"] != "test" {
		t.Errorf("unexpected JSON fields: %v", m)
	}
}

func TestNew_LevelFiltering(t *testing.T) {
	var buf bytes.Buffer
	l, err := New("warn", "text", &buf)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	l.Info("suppressed")
	if buf.Len() != 0 {
		t.Errorf("info record emitted at warn level: %q", buf.String())
	}
	l.Warn("shown")
	if !strings.Contains(buf.String(), "shown") {
		t.Errorf("warn record not emitted: %q", buf.String())
	}
}

func TestNew_InvalidLevel(t *testing.T) {
	if _, err := New("nope", "text", &bytes.Buffer{}); err == nil {
		t.Fatal("expected error for invalid level, got nil")
	}
}

func TestNew_InvalidFormat(t *testing.T) {
	if _, err := New("info", "xml", &bytes.Buffer{}); err == nil {
		t.Fatal("expected error for invalid format, got nil")
	}
}

func TestSetup_SetsDefault(t *testing.T) {
	var buf bytes.Buffer
	if err := Setup("info", "text", &buf); err != nil {
		t.Fatalf("Setup: %v", err)
	}
	slog.Info("via default")
	if !strings.Contains(buf.String(), "via default") {
		t.Errorf("default logger did not route to configured writer: %q", buf.String())
	}
}
