package sandbox

import (
	"testing"
	"time"
)

func testSessionRequest(t *testing.T) *SessionRequest {
	t.Helper()
	return &SessionRequest{
		RequestID:   "session-req-1",
		SessionID:   "session-1",
		Executable:  "helper",
		WorkingDir:  t.TempDir(),
		Environment: map[string]string{"PATH": "/usr/bin"},
		Limits:      Limits{Timeout: 5 * time.Second},
	}
}

func TestStartValidatesRequest(t *testing.T) {
	cases := []struct {
		name string
		mut  func(*SessionRequest)
	}{
		{"empty workdir", func(r *SessionRequest) { r.WorkingDir = " " }},
		{"empty executable", func(r *SessionRequest) { r.Executable = " " }},
		{"zero timeout", func(r *SessionRequest) { r.Limits.Timeout = 0 }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := testSessionRequest(t)
			tc.mut(req)
			_, err := Start(t.Context(), req)
			if code, ok := CodeOf(err); !ok || code != ErrorCodeInvalidArgument {
				t.Fatalf("Start = %v, want invalid_argument", err)
			}
		})
	}
}

// TestStartNilRequest proves the pointer contract: a nil request is a typed
// invalid_argument, never a panic.
func TestStartNilRequest(t *testing.T) {
	_, err := Start(t.Context(), nil)
	if code, ok := CodeOf(err); !ok || code != ErrorCodeInvalidArgument {
		t.Fatalf("Start(nil) = %v, want invalid_argument", err)
	}
}
