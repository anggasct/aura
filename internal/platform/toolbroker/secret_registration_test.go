package toolbroker

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/anggasct/aura/internal/platform/approval"
	"github.com/anggasct/aura/internal/platform/capability/capabilitytest"
)

func TestRegisteredSecretsAreRedactedFromOutputsAndErrors(t *testing.T) {
	cases := []struct {
		name    string
		adapter Adapter
		check   func(t *testing.T, result ToolResult, err error)
	}{
		{
			name: "output",
			adapter: func(context.Context, *ToolRequest, approval.Constraints) (ToolResult, error) {
				return ToolResult{Output: json.RawMessage(`{"echo":"rotated-token-value"}`)}, nil
			},
			check: func(t *testing.T, result ToolResult, err error) {
				if err != nil || string(result.Output) != `{"echo":"[REDACTED]"}` {
					t.Fatalf("output = %s, err = %v", result.Output, err)
				}
			},
		},
		{
			name: "error",
			adapter: func(context.Context, *ToolRequest, approval.Constraints) (ToolResult, error) {
				return ToolResult{}, errors.New("upstream said rotated-token-value")
			},
			check: func(t *testing.T, _ ToolResult, err error) {
				if err == nil || strings.Contains(err.Error(), "rotated-token-value") || !strings.Contains(err.Error(), "[REDACTED]") {
					t.Fatalf("error = %v", err)
				}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			broker, err := New(&Options{
				HostCapabilities: capabilitytest.Host(t), Effects: newBrokerEffectExecutor(t),
				Adapters: map[string]Adapter{"read_file@v1": tc.adapter},
			})
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			request := brokerRequest("read_file", `{"path":"note.txt"}`, "workspace-read")
			before, err := broker.Execute(context.Background(), request)
			if tc.name == "output" && (err != nil || !strings.Contains(string(before.Output), "rotated-token-value")) {
				t.Fatalf("unregistered output = %s, err = %v", before.Output, err)
			}
			broker.RegisterSecret("")
			broker.RegisterSecret("rotated-token-value")
			broker.RegisterSecret("rotated-token-value")
			result, err := broker.Execute(context.Background(), request)
			tc.check(t, result, err)
			if got := broker.secretValues(); len(got) != 1 {
				t.Fatalf("secrets = %v, want one deduplicated value", got)
			}
		})
	}
}

func TestSecretRegistrationIsSafeUnderConcurrentExecution(t *testing.T) {
	broker, err := New(&Options{
		HostCapabilities: capabilitytest.Host(t), Effects: newBrokerEffectExecutor(t),
		Adapters: map[string]Adapter{"read_file@v1": func(context.Context, *ToolRequest, approval.Constraints) (ToolResult, error) {
			return ToolResult{Output: json.RawMessage(`{"ok":true}`)}, nil
		}},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(2)
		go func() {
			defer wg.Done()
			broker.RegisterSecret("value-" + string(rune('a'+i)))
		}()
		go func() {
			defer wg.Done()
			request := brokerRequest("read_file", `{"path":"note.txt"}`, "workspace-read")
			if _, err := broker.Execute(context.Background(), request); err != nil {
				t.Errorf("Execute: %v", err)
			}
		}()
	}
	wg.Wait()
}
