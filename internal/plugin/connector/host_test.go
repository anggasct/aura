package connector

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

func TestInvokeRunsARequestThroughTheHost(t *testing.T) {
	api := newFakeAPI(t)
	starter := newStarter(func(int) func(map[string]any) (any, error) {
		return oneRequest(http.MethodGet, api.url("/v1/messages?q=a&page=2"), map[string]string{"Accept": "application/json"}, nil)
	})
	h := newTestHost(t, api, starter, &bearerAuth{token: testToken})
	output, fail := invokeTool(t, h, readTool(h))
	if fail != nil {
		t.Fatalf("invoke: %v", fail)
	}
	decoded := decodeResultOutput(t, output)
	if decoded["status"] != float64(200) {
		t.Fatalf("output = %s", output)
	}
	seen := api.requests()
	if len(seen) != 1 || seen[0].Method != http.MethodGet || seen[0].Path != "/v1/messages" || seen[0].Query != "q=a&page=2" {
		t.Fatalf("api saw %+v", seen)
	}
	if got := seen[0].Header.Get("Authorization"); got != "Bearer "+testToken {
		t.Fatalf("authorization = %q", got)
	}
	if got := seen[0].Header.Get("Accept"); got != "application/json" {
		t.Fatalf("accept = %q", got)
	}
	var invoke map[string]any
	if err := decodeStrict(starter.session(0).payloads()[0], &invoke); err != nil {
		t.Fatalf("decode invoke: %v", err)
	}
	if invoke["v"] != Version || invoke["type"] != "invoke" || invoke["tool"] != "search" || invoke["idempotency_key"] != "key-1" || invoke["call"] == "" {
		t.Fatalf("invoke = %v", invoke)
	}
	if args, _ := invoke["arguments"].(map[string]any); args["q"] != "hello" {
		t.Fatalf("arguments = %v", invoke["arguments"])
	}
}

func TestConnectorNeverReceivesACredentialAndItsAuthorizationIsDropped(t *testing.T) {
	cases := []struct {
		name  string
		token string
		want  string
	}{
		{"oauth2 style bearer", testToken, "Bearer " + testToken},
		{"no auth", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			api := newFakeAPI(t)
			api.handler = func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/plain")
				w.Header().Set("ETag", `"v1"`)
				w.Header().Set("Retry-After", "7")
				w.Header().Set("Set-Cookie", "session=leak")
				w.Header().Set("X-Credential", r.Header.Get("Authorization"))
				w.Header().Set("Www-Authenticate", "Bearer realm=leak")
				_, _ = w.Write([]byte("body"))
			}
			supplied := map[string]string{
				"Authorization": "Bearer evil", "Cookie": "a=b", "Host": "evil.example",
				"Proxy-Authorization": "Basic x", "Proxy-Connection": "keep-alive", "Connection": "close", "Transfer-Encoding": "chunked",
				"Upgrade": "websocket", "X-Custom": "kept",
			}
			starter := newStarter(func(int) func(map[string]any) (any, error) {
				return oneRequest(http.MethodGet, api.url("/x"), supplied, nil)
			})
			h := newTestHost(t, api, starter, &bearerAuth{token: tc.token})
			output, fail := invokeTool(t, h, readTool(h))
			if fail != nil {
				t.Fatalf("invoke: %v", fail)
			}
			seen := api.requests()
			if len(seen) != 1 {
				t.Fatalf("api saw %d requests", len(seen))
			}
			header := seen[0].Header
			if got := header.Get("Authorization"); got != tc.want {
				t.Fatalf("authorization at the api = %q, want %q", got, tc.want)
			}
			for _, name := range []string{"Cookie", "Proxy-Authorization", "Proxy-Connection", "Upgrade"} {
				if header.Get(name) != "" {
					t.Errorf("header %s reached the api", name)
				}
			}
			if header.Get("X-Custom") != "kept" {
				t.Errorf("a plain header was dropped: %v", header)
			}
			var returned struct {
				Headers map[string]string `json:"headers"`
				Status  int               `json:"status"`
			}
			if err := decodeStrict(output, &returned); err != nil {
				t.Fatalf("decode output: %v (%s)", err, output)
			}
			if len(returned.Headers) != 3 || returned.Headers["Content-Type"] != "text/plain" || returned.Headers["ETag"] != `"v1"` || returned.Headers["Retry-After"] != "7" {
				t.Fatalf("headers returned to the connector = %v", returned.Headers)
			}
			for _, payload := range starter.allPayloads() {
				if tc.token != "" && bytes.Contains(payload, []byte(tc.token)) {
					t.Fatalf("a message to the connector carries the credential: %s", payload)
				}
				for _, leak := range []string{"leak", "Set-Cookie", "X-Credential"} {
					if bytes.Contains(payload, []byte(leak)) {
						t.Fatalf("a message to the connector carries %q: %s", leak, payload)
					}
				}
			}
		})
	}
}

func TestSessionRequestCarriesNoEnvironmentOrCredential(t *testing.T) {
	api := newFakeAPI(t)
	starter := newStarter(func(int) func(map[string]any) (any, error) {
		return oneRequest(http.MethodGet, api.url("/x"), nil, nil)
	})
	h := newTestHost(t, api, starter, &bearerAuth{token: testToken})
	if _, fail := invokeTool(t, h, readTool(h)); fail != nil {
		t.Fatalf("invoke: %v", fail)
	}
	starter.mu.Lock()
	defer starter.mu.Unlock()
	if len(starter.requests) != 1 {
		t.Fatalf("starts = %d", len(starter.requests))
	}
	request := starter.requests[0]
	if request.Executable != "/bundle/bin/connector" || request.WorkingDir != "/bundle" || request.MaxOutputBytes != 1<<20 || request.Timeout != h.cfg.RequestTimeout || request.SetupTimeout != h.cfg.StartupTimeout {
		t.Fatalf("session request = %+v", request)
	}
	if strings.Contains(fmt.Sprintf("%+v", request), testToken) {
		t.Fatalf("session request carries the credential: %+v", request)
	}
}

func TestHostRefusesRequestsTheContractForbids(t *testing.T) {
	api := newFakeAPI(t)
	hostName := api.host()
	denied := []struct {
		name string
		url  string
	}{
		{"unapproved host", "https://evil.example/x"},
		{"plain http", "http://" + hostName + "/x"},
		{"userinfo", "https://user:pw@" + hostName + "/x"},
		{"fragment", api.url("/x#frag")},
		{"approved host on another port", "https://api.mail.example:8443/x"},
		{"no host", "https:///x"},
		{"another scheme", "ftp://" + hostName + "/x"},
		{"relative", "/x"},
	}
	for _, tc := range denied {
		t.Run("denied "+tc.name, func(t *testing.T) {
			starter := newStarter(func(int) func(map[string]any) (any, error) {
				return oneRequest(http.MethodGet, tc.url, nil, nil)
			})
			h := newTestHost(t, api, starter, &bearerAuth{token: testToken})
			output, fail := invokeTool(t, h, readTool(h))
			if fail != nil {
				t.Fatalf("invoke: %v", fail)
			}
			if decoded := decodeResultOutput(t, output); decoded["error"] != "egress_denied" || decoded["status"] != nil {
				t.Fatalf("connector was told %v, want egress_denied", decoded)
			}
		})
	}
	if api.count() != 0 {
		t.Fatalf("the api saw %d refused requests", api.count())
	}
}

func TestHostEndsTheCallForRequestsThatBreakTheContract(t *testing.T) {
	api := newFakeAPI(t)
	tooLong := api.url("/" + strings.Repeat("a", maxURLBytes))
	manyHeaders := map[string]string{}
	for i := range maxHeaders + 1 {
		manyHeaders[fmt.Sprintf("X-H%d", i)] = "v"
	}
	cases := []struct {
		name    string
		tool    func(h *host) *Tool
		method  string
		url     string
		headers map[string]string
		body    string
		want    ErrorCode
	}{
		{"read tool posts", readTool, "POST", api.url("/x"), nil, "", ErrTierViolation},
		{"read tool puts", readTool, "PUT", api.url("/x"), nil, "", ErrTierViolation},
		{"read tool patches", readTool, "PATCH", api.url("/x"), nil, "", ErrTierViolation},
		{"read tool deletes", readTool, "DELETE", api.url("/x"), nil, "", ErrTierViolation},
		{"unknown method", effectTool, "TRACE", api.url("/x"), nil, "", ErrRPCInvalid},
		{"lowercase method", effectTool, "get", api.url("/x"), nil, "", ErrRPCInvalid},
		{"empty url", effectTool, "GET", "", nil, "", ErrRPCInvalid},
		{"url too long", effectTool, "GET", tooLong, nil, "", ErrRPCInvalid},
		{"unparsable url", effectTool, "GET", "https://[::1", nil, "", ErrRPCInvalid},
		{"header name with a space", effectTool, "GET", api.url("/x"), map[string]string{"Bad Name": "v"}, "", ErrRPCInvalid},
		{"header value with a newline", effectTool, "GET", api.url("/x"), map[string]string{"X-A": "a\r\nX-B: b"}, "", ErrRPCInvalid},
		{"too many headers", effectTool, "GET", api.url("/x"), manyHeaders, "", ErrRPCInvalid},
		{"header names repeat by case", effectTool, "GET", api.url("/x"), map[string]string{"X-A": "1", "x-a": "2"}, "", ErrRPCInvalid},
		{"body is not base64", effectTool, "POST", api.url("/x"), nil, "***", ErrRPCInvalid},
		{"body is unpadded base64", effectTool, "POST", api.url("/x"), nil, "QQ", ErrRPCInvalid},
		{"body over the bound", effectTool, "POST", api.url("/x"), nil, base64.StdEncoding.EncodeToString(make([]byte, maxBodyBytes+1)), ErrRPCLimit},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			starter := newStarter(func(int) func(map[string]any) (any, error) {
				return func(in map[string]any) (any, error) {
					message := httpReply(callOf(in), tc.method, tc.url, tc.headers, nil)
					message["body_b64"] = tc.body
					return message, nil
				}
			})
			h := newTestHost(t, api, starter, &bearerAuth{token: testToken})
			_, fail := invokeTool(t, h, tc.tool(h))
			mustCode(t, fail, tc.want)
			if fail.RequestSent {
				t.Fatal("a refused request is marked as sent")
			}
			if !starter.session(0).closed.Load() {
				t.Fatal("the session stayed open after the call was refused")
			}
		})
	}
	if api.count() != 0 {
		t.Fatalf("the api saw %d refused requests", api.count())
	}
}

func TestApprovedHostMatchingIsExact(t *testing.T) {
	h := newHost(&Config{ID: "mailer", Egress: []string{"api.mail.example", "Uploads.Mail.Example:8443"}}, nil, http.DefaultClient, nil)
	cases := map[string]bool{
		"https://api.mail.example/x":          true,
		"https://API.MAIL.EXAMPLE/x":          true,
		"https://api.mail.example:443/x":      true,
		"https://api.mail.example:8443/x":     false,
		"https://uploads.mail.example:8443/x": true,
		"https://uploads.mail.example/x":      false,
		"https://mail.example/x":              false,
		"https://evil.api.mail.example/x":     false,
		"https://api.mail.example.evil/x":     false,
	}
	for raw, want := range cases {
		target, err := url.Parse(raw)
		if err != nil {
			t.Fatalf("parse %q: %v", raw, err)
		}
		if got := h.approvedHost(target); got != want {
			t.Errorf("approvedHost(%q) = %v, want %v", raw, got, want)
		}
	}
}

func TestRedirectIsReturnedToTheConnectorUnfollowed(t *testing.T) {
	api := newFakeAPI(t)
	api.handler = func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/elsewhere" {
			_, _ = w.Write([]byte("followed"))
			return
		}
		w.Header().Set("Location", "/elsewhere")
		w.WriteHeader(http.StatusFound)
	}
	starter := newStarter(func(int) func(map[string]any) (any, error) {
		return oneRequest(http.MethodGet, api.url("/start"), nil, nil)
	})
	h := newTestHost(t, api, starter, &bearerAuth{token: testToken})
	output, fail := invokeTool(t, h, readTool(h))
	if fail != nil {
		t.Fatalf("invoke: %v", fail)
	}
	if decoded := decodeResultOutput(t, output); decoded["status"] != float64(302) {
		t.Fatalf("connector saw %s, want the 302 as is", output)
	}
	if seen := api.requests(); len(seen) != 1 || seen[0].Path != "/start" {
		t.Fatalf("api saw %+v, want the redirect target untouched", seen)
	}
}

func TestResponseBodyBound(t *testing.T) {
	cases := []struct {
		name string
		size int
		want ErrorCode
	}{
		{"exactly the bound", maxBodyBytes, ""},
		{"one byte over", maxBodyBytes + 1, ErrRPCLimit},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			api := newFakeAPI(t)
			api.handler = func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write(make([]byte, tc.size))
			}
			starter := newStarter(func(int) func(map[string]any) (any, error) {
				return func(in map[string]any) (any, error) {
					if kindOf(in) == "invoke" {
						return httpReply(callOf(in), "GET", api.url("/x"), nil, nil), nil
					}
					encoded, _ := in["body_b64"].(string)
					return resultReply(callOf(in), map[string]any{"size": len(encoded)}), nil
				}
			})
			h := newTestHost(t, api, starter, &bearerAuth{})
			h.cfg.MaxMessageBytes = 1 << 10
			_, fail := invokeTool(t, h, readTool(h))
			if tc.want == "" {
				if fail != nil {
					t.Fatalf("invoke: %v", fail)
				}
				return
			}
			mustCode(t, fail, tc.want)
		})
	}
}

func TestRequestStepBound(t *testing.T) {
	cases := []struct {
		name  string
		steps int
		want  ErrorCode
	}{
		{"at the bound", maxHTTPSteps, ""},
		{"over the bound", maxHTTPSteps + 1, ErrRPCLimit},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			api := newFakeAPI(t)
			starter := newStarter(func(int) func(map[string]any) (any, error) {
				done := 0
				return func(in map[string]any) (any, error) {
					if done == tc.steps {
						return resultReply(callOf(in), map[string]any{"steps": done}), nil
					}
					done++
					return httpReply(callOf(in), "GET", api.url("/x"), nil, nil), nil
				}
			})
			h := newTestHost(t, api, starter, &bearerAuth{})
			output, fail := invokeTool(t, h, readTool(h))
			if tc.want == "" {
				if fail != nil || decodeResultOutput(t, output)["steps"] != float64(tc.steps) {
					t.Fatalf("invoke = %s, %v", output, fail)
				}
				return
			}
			mustCode(t, fail, tc.want)
			if api.count() != maxHTTPSteps {
				t.Fatalf("api saw %d requests, want exactly the bound", api.count())
			}
		})
	}
}

func TestOutputBound(t *testing.T) {
	cases := []struct {
		name   string
		output string
		want   ErrorCode
	}{
		{"within the bound", `{"v":"` + strings.Repeat("a", 100) + `"}`, ""},
		{"over the bound", `{"v":"` + strings.Repeat("a", 2000) + `"}`, ErrRPCLimit},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			starter := newStarter(func(int) func(map[string]any) (any, error) {
				return func(in map[string]any) (any, error) {
					return []byte(`{"v":"` + Version + `","type":"result","call":"` + callOf(in) + `","output":` + tc.output + `}`), nil
				}
			})
			h := newTestHost(t, nil, starter, &bearerAuth{})
			h.cfg.MaxMessageBytes = 1024
			_, fail := invokeTool(t, h, readTool(h))
			if tc.want == "" {
				if fail != nil {
					t.Fatalf("invoke: %v", fail)
				}
				return
			}
			mustCode(t, fail, tc.want)
			if !starter.session(0).closed.Load() {
				t.Fatal("session left open")
			}
		})
	}
}

func TestCredentialGoesOnlyToTheHostsThePluginDeclaredForIt(t *testing.T) {
	primary := newFakeAPI(t)
	secondary := newFakeAPI(t)
	cases := []struct {
		name     string
		target   func() string
		wantAuth string
		wantHit  *fakeAPI
	}{
		{"auth host", func() string { return primary.url("/v1/messages") }, "Bearer " + testToken, primary},
		{"second approved host", func() string { return secondary.url("/v1/files") }, "", secondary},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			before := tc.wantHit.count()
			starter := newStarter(func(int) func(map[string]any) (any, error) {
				return oneRequest(http.MethodGet, tc.target(), map[string]string{"Authorization": "Bearer evil"}, nil)
			})
			strategy := &bearerAuth{token: testToken}
			cfg := testConfig(primary, strategy)
			cfg.Egress = append(cfg.Egress, secondary.host())
			cfg.AuthHosts = []string{primary.host()}
			h := newHost(&cfg, starter, primary.client(), slog.New(slog.DiscardHandler))
			if _, fail := invokeTool(t, h, readTool(h)); fail != nil {
				t.Fatalf("invoke: %v", fail)
			}
			seen := tc.wantHit.requests()
			if len(seen) != before+1 {
				t.Fatalf("the target saw %d requests, want %d", len(seen), before+1)
			}
			if got := seen[before].Header.Get("Authorization"); got != tc.wantAuth {
				t.Fatalf("authorization = %q, want %q", got, tc.wantAuth)
			}
			wantCalls := int64(0)
			if tc.wantAuth != "" {
				wantCalls = 1
			}
			if strategy.calls.Load() != wantCalls {
				t.Fatalf("the strategy was asked %d times, want %d", strategy.calls.Load(), wantCalls)
			}
		})
	}
}

func TestAuthHostsMatchLikeEgressEntries(t *testing.T) {
	cfg := Config{AuthHosts: []string{"API.Mail.Example", "files.mail.example:8443"}}
	h := &host{credited: egressSet(cfg.AuthHosts)}
	cases := map[string]bool{
		"https://api.mail.example/x":           true,
		"https://api.mail.example:443/x":       true,
		"https://API.MAIL.EXAMPLE/x":           true,
		"https://api.mail.example:8443/x":      false,
		"https://files.mail.example:8443/x":    true,
		"https://files.mail.example/x":         false,
		"https://other.mail.example/x":         false,
		"https://api.mail.example.evil.test/x": false,
	}
	for raw, want := range cases {
		target, err := url.Parse(raw)
		if err != nil {
			t.Fatalf("Parse %s: %v", raw, err)
		}
		if got := h.credentialHost(target); got != want {
			t.Errorf("credentialHost(%s) = %v, want %v", raw, got, want)
		}
	}
}
