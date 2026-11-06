package connector

import (
	"encoding/base64"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/anggasct/aura/internal/effect"
	"github.com/anggasct/aura/internal/principal"
	"github.com/anggasct/aura/internal/toolbroker"
)

type step struct {
	method string
	path   string
}

type steppingConnector struct {
	mu     sync.Mutex
	steps  []step
	ending string
	index  int
	seen   []map[string]any
}

func (c *steppingConnector) program(api *fakeAPI) program {
	return func(in map[string]any) (any, error) {
		c.mu.Lock()
		defer c.mu.Unlock()
		switch kindOf(in) {
		case "invoke":
		case "http_result":
			c.seen = append(c.seen, in)
		default:
			return nil, fmt.Errorf("unexpected message %q", kindOf(in))
		}
		if c.index < len(c.steps) {
			next := c.steps[c.index]
			c.index++
			return httpReply(callOf(in), next.method, api.url(next.path), nil, []byte("payload")), nil
		}
		if c.ending == "error" {
			return errorReply(callOf(in), "gave_up"), nil
		}
		return resultReply(callOf(in), map[string]any{"done": true}), nil
	}
}

func (c *steppingConnector) received() []map[string]any {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.seen)
}

type statusQueue struct {
	mu       sync.Mutex
	statuses []int
	body     []byte
	served   int
}

func (q *statusQueue) handler(w http.ResponseWriter, _ *http.Request) {
	q.mu.Lock()
	status := http.StatusOK
	if q.served < len(q.statuses) {
		status = q.statuses[q.served]
	}
	q.served++
	body := q.body
	q.mu.Unlock()
	if status >= 300 && status < 400 {
		w.Header().Set("Location", "https://other.example/")
	}
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

func statusesOf(in []map[string]any) []int {
	out := make([]int, 0, len(in))
	for _, result := range in {
		status, _ := result["status"].(float64)
		out = append(out, int(status))
	}
	return out
}

type settlement string

const (
	settledSucceeded settlement = "succeeded"
	settledFailed    settlement = "failed"
	settledUnknown   settlement = "unknown"
)

func post() step { return step{http.MethodPost, "/v1/send"} }
func get() step  { return step{http.MethodGet, "/v1/lookup"} }

func TestMutatingRequestsAreClassifiedFromTheirStatus(t *testing.T) {
	refused := []int{400, 401, 403, 404, 405, 413, 415, 422, 429}
	ambiguous := []int{300, 301, 302, 303, 304, 307, 308, 408, 409, 410, 418, 425, 451, 500, 501, 502, 503, 504, 507}
	cases := []struct {
		name       string
		steps      []step
		statuses   []int
		ending     string
		body       []byte
		want       settlement
		wantCode   string
		apiCalls   int
		connectors []int
	}{
		{name: "a 200 ends the call as a success", steps: []step{post()}, statuses: []int{200}, want: settledSucceeded, apiCalls: 1, connectors: []int{200}},
		{name: "a 204 counts as accepted", steps: []step{post()}, statuses: []int{204}, want: settledSucceeded, apiCalls: 1, connectors: []int{204}},
		{name: "a retry after 429 is allowed", steps: []step{post(), post()}, statuses: []int{429, 200}, want: settledSucceeded, apiCalls: 2, connectors: []int{429, 200}},
		{name: "a retry after 400 is allowed", steps: []step{post(), post()}, statuses: []int{400, 200}, want: settledSucceeded, apiCalls: 2, connectors: []int{400, 200}},
		{name: "two refusals then the connector gives up", steps: []step{post(), post()}, statuses: []int{429, 422}, ending: "error", want: settledFailed, wantCode: "plugin_connector_error", apiCalls: 2, connectors: []int{429, 422}},
		{name: "400 then a result", steps: []step{post()}, statuses: []int{400}, want: settledFailed, wantCode: "plugin_effect_not_performed", apiCalls: 1, connectors: []int{400}},
		{name: "400 then an error", steps: []step{post()}, statuses: []int{400}, ending: "error", want: settledFailed, wantCode: "plugin_connector_error", apiCalls: 1, connectors: []int{400}},
		{name: "429 then a result", steps: []step{post()}, statuses: []int{429}, want: settledFailed, wantCode: "plugin_effect_not_performed", apiCalls: 1, connectors: []int{429}},
		{name: "503 then an attempted retry", steps: []step{post(), post()}, statuses: []int{503, 200}, want: settledUnknown, wantCode: "plugin_request_failed", apiCalls: 1},
		{name: "500 then a result", steps: []step{post()}, statuses: []int{500}, want: settledUnknown, wantCode: "plugin_request_failed", apiCalls: 1},
		{name: "500 then an error", steps: []step{post()}, statuses: []int{500}, ending: "error", want: settledUnknown, wantCode: "plugin_request_failed", apiCalls: 1},
		{name: "200 then 400 is a refusal after a send", steps: []step{post(), post()}, statuses: []int{200, 400}, want: settledUnknown, wantCode: "plugin_request_failed", apiCalls: 2, connectors: []int{200}},
		{name: "200 then 429 is a refusal after a send", steps: []step{post(), post()}, statuses: []int{200, 429}, want: settledUnknown, wantCode: "plugin_request_failed", apiCalls: 2, connectors: []int{200}},
		{name: "400 then 503 is ambiguous at once", steps: []step{post(), post(), post()}, statuses: []int{400, 503, 200}, want: settledUnknown, wantCode: "plugin_request_failed", apiCalls: 2, connectors: []int{400}},
		{name: "200 then an error", steps: []step{post()}, statuses: []int{200}, ending: "error", want: settledUnknown, wantCode: "plugin_connector_error", apiCalls: 1, connectors: []int{200}},
		{name: "a result with no request at all", want: settledFailed, wantCode: "plugin_effect_not_performed"},
		{name: "a result after reads only", steps: []step{get()}, statuses: []int{200}, want: settledFailed, wantCode: "plugin_effect_not_performed", apiCalls: 1, connectors: []int{200}},
		{name: "reads around a successful write", steps: []step{get(), post(), get()}, statuses: []int{200, 201, 200}, want: settledSucceeded, apiCalls: 3, connectors: []int{200, 201, 200}},
		{name: "a failing read after a successful write", steps: []step{post(), get()}, statuses: []int{200, 500}, want: settledSucceeded, apiCalls: 2, connectors: []int{200, 500}},
		{name: "a refused write then a read then a result", steps: []step{post(), get()}, statuses: []int{403, 200}, want: settledFailed, wantCode: "plugin_effect_not_performed", apiCalls: 2, connectors: []int{403, 200}},
		{name: "an oversized 2xx body is dropped but the write counts", steps: []step{post()}, statuses: []int{200}, body: make([]byte, maxBodyBytes+1), want: settledSucceeded, apiCalls: 1, connectors: []int{200}},
		{name: "an oversized 4xx body is dropped and stays a refusal", steps: []step{post()}, statuses: []int{400}, ending: "error", body: make([]byte, maxBodyBytes+1), want: settledFailed, wantCode: "plugin_connector_error", apiCalls: 1, connectors: []int{400}},
	}
	for _, status := range refused {
		cases = append(cases, struct {
			name       string
			steps      []step
			statuses   []int
			ending     string
			body       []byte
			want       settlement
			wantCode   string
			apiCalls   int
			connectors []int
		}{name: fmt.Sprintf("refused status %d fails the effect and returns to the connector", status), steps: []step{post()}, statuses: []int{status}, ending: "error", want: settledFailed, wantCode: "plugin_connector_error", apiCalls: 1, connectors: []int{status}})
	}
	for _, status := range ambiguous {
		cases = append(cases, struct {
			name       string
			steps      []step
			statuses   []int
			ending     string
			body       []byte
			want       settlement
			wantCode   string
			apiCalls   int
			connectors []int
		}{name: fmt.Sprintf("status %d is ambiguous and ends the call at once", status), steps: []step{post(), post()}, statuses: []int{status, 200}, want: settledUnknown, wantCode: "plugin_request_failed", apiCalls: 1})
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			connector := &steppingConnector{steps: tc.steps, ending: tc.ending}
			queue := &statusQueue{statuses: tc.statuses, body: tc.body}
			f := newManagerFixture(t, func(api *fakeAPI) func(int) func(map[string]any) (any, error) {
				return func(int) func(map[string]any) (any, error) { return connector.program(api) }
			}, nil)
			f.api.mu.Lock()
			f.api.handler = queue.handler
			f.api.mu.Unlock()
			f.start()
			request := f.request("plugin_mailer_send", `{}`, principal.ClassOwner)
			_, err := f.execute(request)
			counts := map[settlement]int{
				settledSucceeded: len(f.intents(effect.StateSucceeded)),
				settledFailed:    len(f.intents(effect.StateFailed)),
				settledUnknown:   len(f.intents(effect.StateUnknown)),
			}
			for state, count := range counts {
				if want := map[bool]int{true: 1, false: 0}[state == tc.want]; count != want {
					t.Fatalf("journal %s = %d, want %d (all %v), error %v", state, count, want, counts, err)
				}
			}
			if tc.want == settledSucceeded {
				if err != nil {
					t.Fatalf("Execute: %v", err)
				}
			} else {
				if class, ok := toolbroker.CodeOf(err); !ok || class != toolbroker.ResultExecutionFailed || !strings.Contains(err.Error(), tc.wantCode) {
					t.Fatalf("Execute error = %v, want an execution failure with %s", err, tc.wantCode)
				}
				if unknown := strings.Contains(err.Error(), "outcome is unknown"); unknown != (tc.want == settledUnknown) {
					t.Fatalf("Execute error = %v, unknown outcome text = %v", err, unknown)
				}
				if tc.want == settledFailed {
					if failed := f.intents(effect.StateFailed); failed[0].SafeErrorCode != tc.wantCode {
						t.Fatalf("journal error code = %q, want %q", failed[0].SafeErrorCode, tc.wantCode)
					}
				}
			}
			if f.api.count() != tc.apiCalls {
				t.Fatalf("api saw %d requests, want %d", f.api.count(), tc.apiCalls)
			}
			if got := statusesOf(connector.received()); !slices.Equal(got, tc.connectors) {
				t.Fatalf("connector was told statuses %v, want %v", got, tc.connectors)
			}
			if tc.body != nil {
				for _, result := range connector.received() {
					if encoded, _ := result["body_b64"].(string); encoded != "" {
						t.Fatalf("an oversized mutating body reached the connector: %d bytes", len(encoded))
					}
				}
			}
			if tc.want == settledUnknown && f.invokePayloads() != 1 {
				t.Fatalf("invokes = %d, the effect was resent", f.invokePayloads())
			}
		})
	}
}

func TestAnAmbiguousStatusNeverGivesTheConnectorAChanceToResend(t *testing.T) {
	connector := &steppingConnector{steps: []step{post(), post(), post()}}
	queue := &statusQueue{statuses: []int{503, 200, 200}}
	starter := newStarter(func(int) func(map[string]any) (any, error) { return nil })
	api := newFakeAPI(t)
	api.handler = queue.handler
	starter.handler = func(int) func(map[string]any) (any, error) { return connector.program(api) }
	h := newTestHost(t, api, starter, &bearerAuth{token: testToken})
	_, fail := invokeTool(t, h, effectTool(h))
	mustCode(t, fail, ErrRequestFailed)
	if !fail.RequestSent {
		t.Fatal("an ambiguous status must count as sent")
	}
	if api.count() != 1 || len(connector.received()) != 0 {
		t.Fatalf("api saw %d requests and the connector heard %d replies, want 1 and 0", api.count(), len(connector.received()))
	}
	if starter.openSessions() != 0 {
		t.Fatal("the session of an ended call stayed open")
	}
}

func TestStatusClassificationIgnoresTheBody(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		sent   bool
	}{
		{"a 400 whose body claims success", 400, `{"sent":true}`, false},
		{"a 500 whose body claims nothing happened", 500, `{"error":"not sent"}`, true},
		{"a 200 whose body says failure", 200, `{"error":"failed"}`, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			api := newFakeAPI(t)
			api.handler = func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}
			starter := newStarter(func(int) func(map[string]any) (any, error) {
				return (&steppingConnector{steps: []step{post()}, ending: "error"}).program(api)
			})
			h := newTestHost(t, api, starter, &bearerAuth{})
			_, fail := invokeTool(t, h, effectTool(h))
			if fail == nil || fail.RequestSent != tc.sent {
				t.Fatalf("failure = %v, want sent=%v", fail, tc.sent)
			}
		})
	}
}

func TestAnOversizedReadReturnsAnErrorAndKeepsTheSession(t *testing.T) {
	api := newFakeAPI(t)
	api.handler = func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/big" {
			_, _ = w.Write(make([]byte, maxBodyBytes+1))
			return
		}
		_, _ = w.Write([]byte("small"))
	}
	var mu sync.Mutex
	var heard []map[string]any
	starter := newStarter(func(int) func(map[string]any) (any, error) {
		index := 0
		return func(in map[string]any) (any, error) {
			mu.Lock()
			defer mu.Unlock()
			switch kindOf(in) {
			case "invoke":
				index = 0
			case "http_result":
				heard = append(heard, in)
			}
			index++
			switch index {
			case 1:
				return httpReply(callOf(in), http.MethodGet, api.url("/big"), nil, nil), nil
			case 2:
				return httpReply(callOf(in), http.MethodGet, api.url("/small"), nil, nil), nil
			}
			return resultReply(callOf(in), map[string]any{"ok": true}), nil
		}
	})
	h := newTestHost(t, api, starter, &bearerAuth{})
	for round := range 2 {
		output, fail := invokeTool(t, h, readTool(h))
		if fail != nil || decodeResultOutput(t, output)["ok"] != true {
			t.Fatalf("round %d: %s, %v", round, output, fail)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(heard) != 4 {
		t.Fatalf("the connector heard %d replies, want 4", len(heard))
	}
	for _, round := range []int{0, 2} {
		first := heard[round]
		if first["error"] != "response_too_large" || first["status"] != nil || first["body_b64"] != nil {
			t.Fatalf("first reply = %v, want only the response_too_large error", first)
		}
		encoded, isText := heard[round+1]["body_b64"].(string)
		if !isText {
			t.Fatalf("the follow up read carried %v, want a body", heard[round+1])
		}
		second, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil {
			t.Fatalf("decode: %v", err)
		}
		if string(second) != "small" {
			t.Fatalf("the follow up read got %q", second)
		}
	}
	if starter.started() != 1 || starter.openSessions() != 1 {
		t.Fatalf("starts = %d, open = %d: the oversized read cost the session", starter.started(), starter.openSessions())
	}
}

func TestAnOversizedReadThroughTheBrokerCostsOnlyThatCall(t *testing.T) {
	var big sync.Once
	f := newManagerFixture(t, func(api *fakeAPI) func(int) func(map[string]any) (any, error) {
		return func(int) func(map[string]any) (any, error) {
			return oneRequest(http.MethodGet, api.url("/v1/big"), nil, nil)
		}
	}, nil)
	f.api.mu.Lock()
	f.api.handler = func(w http.ResponseWriter, _ *http.Request) {
		big.Do(func() { _, _ = w.Write(make([]byte, maxBodyBytes+1)) })
		_, _ = w.Write([]byte(`{"ok":true}`))
	}
	f.api.mu.Unlock()
	f.start()
	for round := range 2 {
		result, err := f.execute(f.request("plugin_mailer_search", `{}`, principal.ClassOwner))
		if err != nil {
			t.Fatalf("round %d: %v", round, err)
		}
		if round == 0 && !strings.Contains(string(result.Output), "response_too_large") {
			t.Fatalf("first output = %s", result.Output)
		}
	}
	if f.starter.started() != 1 {
		t.Fatalf("starts = %d, want the session to survive", f.starter.started())
	}
}
