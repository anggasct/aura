package cli

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/anggasct/aura/internal/capability"
	"github.com/anggasct/aura/internal/config"
	"github.com/anggasct/aura/internal/health"
	"github.com/anggasct/aura/internal/sandbox"
)

func cliWithoutDelegation() (sandbox.Primitives, error) {
	return sandbox.Primitives{
		UserNamespace: true, Seccomp: true, Landlock: true, ProcessGroups: true,
		CgroupDetail: "no delegated cgroup for sandbox bounds in paseo.service: run aura server under a systemd unit with Delegate=yes",
	}, nil
}

func daemonProbe(t *testing.T, body health.ProbeBody) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/readyz" {
			http.NotFound(w, r)
			return
		}
		if err := json.NewEncoder(w).Encode(body); err != nil {
			t.Errorf("encode probe body: %v", err)
		}
	}))
	t.Cleanup(srv.Close)
	return strings.TrimPrefix(srv.URL, "http://")
}

func runStatusAgainst(t *testing.T, listen string, offline bool) (string, error) {
	t.Helper()
	dataRoot := t.TempDir()
	seedHealthyStorage(t, dataRoot)
	cfg := config.Default()
	cfg.Storage.Path = dataRoot
	cfg.Health.Listen = listen
	cfg.Health.DiskWarningPercent = 1
	cfg.Health.DiskCriticalPercent = 1
	cfg.Health.DiskCriticalFloorBytes = 1
	cfg.Models.Definitions = map[string]config.ModelDefinition{
		"primary": {Protocol: "anthropic", Model: "claude-sonnet-4"},
	}
	cmd := &cobra.Command{Use: "status"}
	cmd.SetContext(t.Context())
	var out bytes.Buffer
	cmd.SetOut(&out)
	err := runStatus(cmd, &cfg, capability.Report{}, cliWithoutDelegation, offline, false, pinnedProcessProbe)
	return out.String(), err
}

func TestStatusSandboxFollowsDaemon(t *testing.T) {
	cases := []struct {
		name       string
		body       *health.ProbeBody
		offline    bool
		want       []string
		absent     []string
		wantFailed bool
	}{
		{
			name:   "daemon ready",
			body:   &health.ProbeBody{Status: health.ProbeStatusReady, Code: health.ProbeCodeReady},
			want:   []string{"sandbox: available", "sandbox ok: verified by running daemon"},
			absent: []string{"missing: cgroup_v2", "scope=cli"},
		},
		{
			name:       "daemon reports sandbox unavailable",
			body:       &health.ProbeBody{Status: health.ProbeStatusNotReady, Code: "sandbox.sandbox_unavailable"},
			want:       []string{"critical sandbox sandbox_unavailable: reported by running daemon"},
			absent:     []string{"scope=cli"},
			wantFailed: true,
		},
		{
			name:       "daemon not ready for another reason",
			body:       &health.ProbeBody{Status: health.ProbeStatusNotReady, Code: "storage.storage_unreachable"},
			want:       []string{"sandbox sandbox_unavailable: scope=cli: missing: cgroup_v2", "paseo.service"},
			wantFailed: true,
		},
		{
			name:       "offline",
			offline:    true,
			want:       []string{"live: unreachable", "sandbox: unavailable", "scope=cli: missing: cgroup_v2"},
			wantFailed: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			listen := ""
			if tc.body != nil {
				listen = daemonProbe(t, *tc.body)
			}
			text, err := runStatusAgainst(t, listen, tc.offline)
			if tc.wantFailed != (err != nil) {
				t.Fatalf("runStatus err = %v, want failure %v\n%s", err, tc.wantFailed, text)
			}
			for _, want := range tc.want {
				if !strings.Contains(text, want) {
					t.Errorf("output lacks %q:\n%s", want, text)
				}
			}
			for _, absent := range tc.absent {
				if strings.Contains(text, absent) {
					t.Errorf("output carries %q:\n%s", absent, text)
				}
			}
		})
	}
}

func TestSandboxSupportNamesCgroupRoot(t *testing.T) {
	cases := []struct {
		name      string
		have      sandbox.Primitives
		supported bool
		want      string
	}{
		{
			name: "delegated",
			have: sandbox.Primitives{
				UserNamespace: true, Seccomp: true, CgroupV2: true, Landlock: true, ProcessGroups: true,
				CgroupDetail: "cgroup_root=aura.service mode=self_leaf",
			},
			supported: true,
			want:      "all containment primitives available (cgroup_root=aura.service mode=self_leaf)",
		},
		{
			name:      "not delegated",
			have:      func() sandbox.Primitives { p, _ := cliWithoutDelegation(); return p }(),
			supported: false,
			want:      "missing: cgroup_v2 (no delegated cgroup for sandbox bounds in paseo.service",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			supported, detail := sandboxSupport(func() (sandbox.Primitives, error) { return tc.have, nil })()
			if supported != tc.supported || !strings.Contains(detail, tc.want) {
				t.Fatalf("sandboxSupport = %v %q, want %v containing %q", supported, detail, tc.supported, tc.want)
			}
			if strings.Contains(detail, "scope=cli") {
				t.Fatalf("daemon-side detail %q carries the cli scope label", detail)
			}
		})
	}
}

func TestDoctorSandboxDetailCarriesCliScope(t *testing.T) {
	supported, detail := localSandboxSupport(cliWithoutDelegation)()
	if supported || !strings.HasPrefix(detail, "scope=cli: missing: cgroup_v2") {
		t.Fatalf("localSandboxSupport = %v %q, want unsupported with scope=cli prefix", supported, detail)
	}
}
