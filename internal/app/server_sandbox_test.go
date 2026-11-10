package app

import (
	"bytes"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"testing"

	"github.com/anggasct/aura/internal/config"
	"github.com/anggasct/aura/internal/platform/sandbox"
)

func TestLogSandboxHostLogsOutcome(t *testing.T) {
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
			prepared, err := tc.prepare()
			logSandboxHost(t.Context(), logger, prepared, err)
			for _, want := range tc.want {
				if !strings.Contains(out.String(), want) {
					t.Errorf("log %q lacks %q", out.String(), want)
				}
			}
		})
	}
}

func TestServerPreparesTheHostBeforeResolvingCapabilities(t *testing.T) {
	var order []string
	prepare := func() (sandbox.HostPreparation, error) {
		order = append(order, "prepare")
		return sandbox.HostPreparation{}, nil
	}
	load := func(string) (config.LoadResult, error) {
		order = append(order, "load")
		return config.LoadResult{}, nil
	}
	if _, err := prepareHostThenLoad("aura.yaml", prepare, load); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(order, []string{"prepare", "load"}) {
		t.Fatalf("order = %v, want prepare then load", order)
	}
}

func TestServerKeepsThePreparationFailureForLogging(t *testing.T) {
	failure := sandbox.Errorf(sandbox.ErrorCodeSandboxUnavailable, "no delegated cgroup")
	startup, err := prepareHostThenLoad("aura.yaml",
		func() (sandbox.HostPreparation, error) { return sandbox.HostPreparation{}, failure },
		func(string) (config.LoadResult, error) { return config.LoadResult{}, nil },
	)
	if err != nil {
		t.Fatal(err)
	}
	if !errors.Is(startup.hostErr, failure) {
		t.Fatalf("hostErr = %v, want %v", startup.hostErr, failure)
	}
}
