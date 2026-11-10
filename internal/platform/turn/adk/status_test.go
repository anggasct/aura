package runtimeadk

import (
	"strings"
	"testing"
)

func TestStatusProtocolByteStatic(t *testing.T) {
	first := statusProtocolLine()
	second := statusProtocolLine()
	if first == "" {
		t.Fatal("status protocol is empty")
	}
	if first != second {
		t.Fatal("status protocol is not byte-static")
	}
	if len(strings.Fields(first)) > 40 {
		t.Errorf("status protocol has %d words, want a short prompt-cache-friendly line", len(strings.Fields(first)))
	}
	for _, banned := range []string{"read_file", "shell.exec", "web_fetch"} {
		if strings.Contains(strings.ToLower(first), banned) {
			t.Errorf("status protocol names tools: %q", first)
		}
	}
}

func TestAppendStatusProtocol(t *testing.T) {
	built := appendStatusProtocol("Be helpful.")
	if !strings.Contains(built, "Be helpful.") {
		t.Errorf("persona lost: %q", built)
	}
	if !strings.Contains(built, statusProtocolSuffix) {
		t.Errorf("protocol missing: %q", built)
	}
	empty := appendStatusProtocol("   ")
	if empty != statusProtocolSuffix {
		t.Errorf("empty instruction = %q, want suffix alone", empty)
	}
	firstBuilt := appendStatusProtocol("Be helpful.")
	secondBuilt := appendStatusProtocol("Be helpful.")
	if firstBuilt != secondBuilt {
		t.Error("status builder is not deterministic")
	}
}
