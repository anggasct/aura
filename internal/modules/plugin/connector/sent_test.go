package connector

import (
	"context"
	"log/slog"
	"net/http"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/platform/egress"
)

func hangUntilAbandoned(_ http.ResponseWriter, r *http.Request) {
	<-r.Context().Done()
}

func hijackAndClose(w http.ResponseWriter, _ *http.Request) {
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		return
	}
	if conn, _, err := hijacker.Hijack(); err == nil {
		_ = conn.Close()
	}
}

func TestRequestSentTracksWhetherAStateChangingRequestLeftTheHost(t *testing.T) {
	cases := []struct {
		name     string
		tool     func(h *host) *Tool
		program  func(api *fakeAPI) program
		api      func(w http.ResponseWriter, r *http.Request)
		timeout  time.Duration
		cancel   bool
		wantCode ErrorCode
		wantSent bool
		apiCalls int
	}{
		{
			name: "post completes and the connector succeeds",
			tool: effectTool,
			program: func(api *fakeAPI) program {
				return oneRequest(http.MethodPost, api.url("/send"), nil, []byte("payload"))
			},
			apiCalls: 1,
		},
		{
			name: "post completes then the session dies",
			tool: effectTool,
			program: func(api *fakeAPI) program {
				return func(in map[string]any) (any, error) {
					if kindOf(in) == "invoke" {
						return httpReply(callOf(in), http.MethodPost, api.url("/send"), nil, []byte("payload")), nil
					}
					return nil, errSessionGone
				}
			},
			wantCode: ErrBackendUnavailable, wantSent: true, apiCalls: 1,
		},
		{
			name: "post completes then the connector reports an error",
			tool: effectTool,
			program: func(api *fakeAPI) program {
				return func(in map[string]any) (any, error) {
					if kindOf(in) == "invoke" {
						return httpReply(callOf(in), http.MethodPost, api.url("/send"), nil, []byte("payload")), nil
					}
					return errorReply(callOf(in), "send_rejected"), nil
				}
			},
			wantCode: ErrConnector, wantSent: true, apiCalls: 1,
		},
		{
			name: "post connection drops mid request",
			tool: effectTool,
			program: func(api *fakeAPI) program {
				return oneRequest(http.MethodPost, api.url("/send"), nil, []byte("payload"))
			},
			api:      hijackAndClose,
			wantCode: ErrRequestFailed, wantSent: true, apiCalls: 1,
		},
		{
			name: "post whose server hangs until the call times out",
			tool: effectTool,
			program: func(api *fakeAPI) program {
				return oneRequest(http.MethodPost, api.url("/send"), nil, []byte("payload"))
			},
			api:      hangUntilAbandoned,
			timeout:  300 * time.Millisecond,
			wantCode: ErrRPCLimit, wantSent: true, apiCalls: 1,
		},
		{
			name: "post cancelled by the caller mid request",
			tool: effectTool,
			program: func(api *fakeAPI) program {
				return oneRequest(http.MethodPost, api.url("/send"), nil, []byte("payload"))
			},
			api:      hangUntilAbandoned,
			cancel:   true,
			wantCode: ErrBackendUnavailable, wantSent: true, apiCalls: 1,
		},
		{
			name: "get whose server hangs until the call times out",
			tool: effectTool,
			program: func(api *fakeAPI) program {
				return oneRequest(http.MethodGet, api.url("/lookup"), nil, nil)
			},
			api:      hangUntilAbandoned,
			timeout:  300 * time.Millisecond,
			wantCode: ErrRPCLimit, apiCalls: 1,
		},
		{
			name: "post to a port nobody listens on",
			tool: effectTool,
			program: func(*fakeAPI) program {
				return oneRequest(http.MethodPost, "https://127.0.0.1:1/send", nil, []byte("payload"))
			},
			wantCode: ErrRequestFailed,
		},
		{
			name: "post to a host that is not approved",
			tool: effectTool,
			program: func(*fakeAPI) program {
				return func(in map[string]any) (any, error) {
					if kindOf(in) == "invoke" {
						return httpReply(callOf(in), http.MethodPost, "https://evil.example/send", nil, []byte("payload")), nil
					}
					return errorReply(callOf(in), "denied"), nil
				}
			},
			wantCode: ErrConnector,
		},
		{
			name: "get then the session dies",
			tool: effectTool,
			program: func(api *fakeAPI) program {
				return func(in map[string]any) (any, error) {
					if kindOf(in) == "invoke" {
						return httpReply(callOf(in), http.MethodGet, api.url("/lookup"), nil, nil), nil
					}
					return nil, errSessionGone
				}
			},
			wantCode: ErrBackendUnavailable, apiCalls: 1,
		},
		{
			name: "get connection drops and the connector gives up",
			tool: effectTool,
			program: func(api *fakeAPI) program {
				return func(in map[string]any) (any, error) {
					if kindOf(in) == "invoke" {
						return httpReply(callOf(in), http.MethodGet, api.url("/lookup"), nil, nil), nil
					}
					return errorReply(callOf(in), "lookup_failed"), nil
				}
			},
			api:      hijackAndClose,
			wantCode: ErrConnector, apiCalls: 1,
		},
		{
			name: "session dies on the first exchange",
			tool: effectTool,
			program: func(*fakeAPI) program {
				return func(map[string]any) (any, error) { return nil, errSessionGone }
			},
			wantCode: ErrBackendUnavailable,
		},
		{
			name: "connector reports an error before any request",
			tool: effectTool,
			program: func(*fakeAPI) program {
				return func(in map[string]any) (any, error) { return errorReply(callOf(in), "bad_input"), nil }
			},
			wantCode: ErrConnector,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			api := newFakeAPI(t)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if tc.api != nil {
				api.handler = tc.api
			}
			if tc.cancel {
				handler := api.handler
				api.handler = func(w http.ResponseWriter, r *http.Request) {
					cancel()
					handler(w, r)
				}
			}
			starter := newStarter(func(int) func(map[string]any) (any, error) { return tc.program(api) })
			h := newTestHost(t, api, starter, &bearerAuth{token: testToken})
			h.allowed["127.0.0.1:1"] = true
			if tc.timeout > 0 {
				h.cfg.RequestTimeout = tc.timeout
			}
			tool := tc.tool(h)
			_, fail := h.invoke(ctx, &call{tool: tool, arguments: []byte(`{"q":"hello"}`), idempotencyKey: "key-1"})
			if tc.wantCode == "" {
				if fail != nil {
					t.Fatalf("invoke: %v", fail)
				}
			} else {
				mustCode(t, fail, tc.wantCode)
				if fail.RequestSent != tc.wantSent {
					t.Fatalf("RequestSent = %v, want %v (%v)", fail.RequestSent, tc.wantSent, fail)
				}
			}
			if api.count() != tc.apiCalls {
				t.Fatalf("api saw %d requests, want %d", api.count(), tc.apiCalls)
			}
		})
	}
}

func TestEgressClientGuardStillAppliesToAnApprovedHost(t *testing.T) {
	api := newFakeAPI(t)
	var told any
	starter := newStarter(func(int) func(map[string]any) (any, error) {
		return func(in map[string]any) (any, error) {
			if kindOf(in) == "invoke" {
				return httpReply(callOf(in), http.MethodPost, api.url("/send?draft=1"), nil, []byte("payload")), nil
			}
			told = in["error"]
			return errorReply(callOf(in), "denied"), nil
		}
	})
	cfg := testConfig(api, &bearerAuth{token: testToken})
	h := newHost(&cfg, starter, egress.NewAPIClient(nil), slog.New(slog.DiscardHandler))
	_, fail := invokeTool(t, h, effectTool(h))
	mustCode(t, fail, ErrConnector)
	if fail.RequestSent || told != "egress_denied" {
		t.Fatalf("sent = %v, connector was told %v, want egress_denied from the egress client", fail.RequestSent, told)
	}
	if api.count() != 0 {
		t.Fatalf("the api saw %d requests through a guarded client", api.count())
	}
}
