package cli

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"github.com/anggasct/aura/internal/sandbox"
)

func TestPrepareSandboxHostLogsOutcome(t *testing.T) {
	cases := []struct {
		name    string
		prepare func() (sandbox.HostPreparation, error)
		want    []string
	}{
		{
			name: "prepared",
			prepare: func() (sandbox.HostPreparation, error) {
				return sandbox.HostPreparation{CgroupRoot: "aura.service", Mode: "self_leaf"}, nil
			},
			want: []string{"level=INFO", "sandbox host prepared", "cgroup_root=aura.service", "mode=self_leaf"},
		},
		{
			name: "not prepared",
			prepare: func() (sandbox.HostPreparation, error) {
				return sandbox.HostPreparation{}, sandbox.Errorf(sandbox.ErrorCodeSandboxUnavailable, "no delegated cgroup")
			},
			want: []string{"level=WARN", "sandbox host not prepared", "no delegated cgroup"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			logger := slog.New(slog.NewTextHandler(&out, nil))
			prepareSandboxHost(t.Context(), logger, tc.prepare)
			for _, want := range tc.want {
				if !strings.Contains(out.String(), want) {
					t.Errorf("log %q lacks %q", out.String(), want)
				}
			}
		})
	}
}
