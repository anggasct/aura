package app

import (
	"bytes"
	"errors"
	"log/slog"
	"strings"
	"testing"

	appconfig "github.com/anggasct/aura/internal/app/config"
	"github.com/anggasct/aura/internal/platform/capability"
	"github.com/anggasct/aura/internal/platform/fault"
)

func TestChatCapabilityGate(t *testing.T) {
	missing := fault.New(capability.ErrorCodeDependencyMissing, "exec-linux: required dependency process-containment is missing")
	unavailable := fault.New(capability.ErrorCodeCapabilityUnavailable, "exec-linux: capability is absent from this artifact")
	cases := []struct {
		name     string
		state    error
		wantErr  bool
		wantWarn bool
	}{
		{name: "healthy"},
		{name: "missing dependency only warns", state: missing, wantWarn: true},
		{name: "several missing dependencies only warn", state: errors.Join(missing, missing), wantWarn: true},
		{name: "unavailable capability stays fatal", state: unavailable, wantErr: true},
		{name: "missing dependency beside an unavailable capability stays fatal", state: errors.Join(missing, unavailable), wantErr: true},
		{name: "foreign error stays fatal", state: errors.New("boom"), wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var logs bytes.Buffer
			logger := slog.New(slog.NewTextHandler(&logs, nil))
			result := &appconfig.LoadResult{CapabilityStateError: tc.state}
			err := chatCapabilityGate(t.Context(), logger, result)
			if tc.wantErr != (err != nil) {
				t.Fatalf("chatCapabilityGate error = %v, want error %v", err, tc.wantErr)
			}
			if tc.wantErr && !errors.Is(err, tc.state) {
				t.Fatalf("chatCapabilityGate error = %v, want the state error itself", err)
			}
			if warned := strings.Contains(logs.String(), "level=WARN"); warned != tc.wantWarn {
				t.Fatalf("warning logged = %v, want %v (logs: %s)", warned, tc.wantWarn, logs.String())
			}
		})
	}
}
