package cli

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/anggasct/aura/internal/sandbox"
)

func TestFormatSandboxStatusAvailable(t *testing.T) {
	all := sandbox.Primitives{UserNamespace: true, Seccomp: true, CgroupV2: true, Landlock: true, ProcessGroups: true}
	text, available := formatSandboxStatus(all, nil)
	if !available || text != "sandbox: available" {
		t.Fatalf("got %q available=%v", text, available)
	}
}

// The surface must name every absent primitive so an operator knows exactly
// what the host lacks, matching the Require gate's vocabulary.
func TestFormatSandboxStatusNamesMissing(t *testing.T) {
	partial := sandbox.Primitives{ProcessGroups: true}
	text, available := formatSandboxStatus(partial, nil)
	if available {
		t.Fatal("want unavailable")
	}
	for _, want := range []string{"cgroup_v2", "landlock", "seccomp", "user_namespace"} {
		if !strings.Contains(text, want) {
			t.Errorf("status %q missing primitive %q", text, want)
		}
	}
	if strings.Contains(text, "process_groups") {
		t.Errorf("status should not list a present primitive:\n%s", text)
	}
}

func TestFormatSandboxStatusReasonOnProbeError(t *testing.T) {
	text, available := formatSandboxStatus(sandbox.Primitives{}, errors.New("sandbox requires Linux"))
	if available || !strings.Contains(text, "sandbox requires Linux") {
		t.Fatalf("got %q available=%v", text, available)
	}
}

func TestStatusCmdReportsMissingPrimitive(t *testing.T) {
	orig := sandboxNegotiate
	t.Cleanup(func() { sandboxNegotiate = orig })
	sandboxNegotiate = func() (sandbox.Primitives, error) {
		return sandbox.Primitives{ProcessGroups: true}, nil
	}
	cmd := newStatusCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	if err := cmd.RunE(cmd, nil); err == nil {
		t.Fatal("want error when sandbox unavailable")
	}
	if !strings.Contains(out.String(), "user_namespace") {
		t.Errorf("output missing primitive name:\n%s", out.String())
	}
}

func TestStatusCmdAvailableExitsClean(t *testing.T) {
	orig := sandboxNegotiate
	t.Cleanup(func() { sandboxNegotiate = orig })
	sandboxNegotiate = func() (sandbox.Primitives, error) {
		return sandbox.Primitives{UserNamespace: true, Seccomp: true, CgroupV2: true, Landlock: true, ProcessGroups: true}, nil
	}
	cmd := newStatusCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	if err := cmd.RunE(cmd, nil); err != nil {
		t.Fatalf("RunE: %v", err)
	}
	if !strings.Contains(out.String(), "sandbox: available") {
		t.Errorf("output=%q", out.String())
	}
}
