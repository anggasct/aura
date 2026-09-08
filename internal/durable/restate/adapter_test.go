package restate

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/anggasct/aura/internal/durable"
)

type stubIngress struct {
	t       *testing.T
	mux     *http.ServeMux
	server  *httptest.Server
	last    stubRequest
	status  map[string]statusResponse
	signals []signalRequest
}

type stubRequest struct {
	method string
	path   string
	header http.Header
	body   []byte
}

func newStubIngress(t *testing.T) *stubIngress {
	t.Helper()
	stub := &stubIngress{t: t, mux: http.NewServeMux(), status: map[string]statusResponse{}}
	stub.mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		body := make([]byte, r.ContentLength)
		if r.ContentLength > 0 {
			_, _ = r.Body.Read(body)
		}
		stub.last = stubRequest{method: r.Method, path: r.URL.Path, header: r.Header.Clone(), body: body}
		parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && len(parts) == 4 && parts[2] == runMethod && parts[3] == "send":
			var envelope runEnvelope
			if err := json.Unmarshal(body, &envelope); err != nil {
				t.Errorf("decode run envelope: %v", err)
			}
			if envelope.Handler == "" {
				t.Error("run envelope carries no handler")
			}
			w.WriteHeader(http.StatusAccepted)
			_, _ = w.Write([]byte(`{"invocationId":"inv_` + parts[1] + `","status":"accepted"}`))
		case r.Method == http.MethodPost && len(parts) == 3 && parts[2] == signalMethod:
			var request signalRequest
			if err := json.Unmarshal(body, &request); err != nil {
				t.Errorf("decode signal request: %v", err)
			}
			stub.signals = append(stub.signals, request)
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{}`))
		case r.Method == http.MethodPost && len(parts) == 3 && parts[2] == statusMethod:
			response, ok := stub.status[parts[1]]
			if !ok {
				response = statusResponse{State: unknownRunState}
			}
			encoded, _ := json.Marshal(response)
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(encoded)
		case r.Method == http.MethodDelete && len(parts) == 3 && parts[0] == "restate" && parts[1] == "invocation":
			w.WriteHeader(http.StatusAccepted)
			_, _ = w.Write([]byte(`{}`))
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"message":"not found","code":404}`))
		}
	})
	stub.server = httptest.NewServer(stub.mux)
	t.Cleanup(stub.server.Close)
	return stub
}

func newTestAdapter(t *testing.T, stub *stubIngress) *Adapter {
	t.Helper()
	adapter, err := NewAdapter(Config{IngressURL: stub.server.URL}, nil)
	if err != nil {
		t.Fatalf("NewAdapter: %v", err)
	}
	adapter.RegisterHandler("greet", func(_ context.Context, _ durable.Invocation) error { return nil })
	return adapter
}

func TestAdapterStartSendsIdempotentInvocation(t *testing.T) {
	stub := newStubIngress(t)
	adapter := newTestAdapter(t, stub)

	ref, err := adapter.Start(t.Context(), durable.StartRequest{Handler: "greet", Key: "run-1", Payload: []byte(`{"in":1}`)})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if ref.Key != "run-1" {
		t.Errorf("ref = %+v, want key run-1", ref)
	}
	if stub.last.method != http.MethodPost || stub.last.path != "/WorkflowRun/run-1/run/send" {
		t.Errorf("request = %s %s, want POST /WorkflowRun/run-1/run/send", stub.last.method, stub.last.path)
	}
	if stub.last.header.Get("idempotency-key") != "" {
		t.Errorf("idempotency-key = %q, want unset: workflow runs are idempotent by key", stub.last.header.Get("idempotency-key"))
	}
	var envelope runEnvelope
	if err := json.Unmarshal(stub.last.body, &envelope); err != nil {
		t.Fatalf("decode run envelope: %v", err)
	}
	if envelope.Handler != "greet" || string(envelope.Payload) != `{"in":1}` {
		t.Errorf("envelope = %+v, want handler greet with the payload bytes", envelope)
	}

	again, err := adapter.Start(t.Context(), durable.StartRequest{Handler: "greet", Key: "run-1", Payload: []byte(`{"in":1}`)})
	if err != nil {
		t.Fatalf("duplicate Start: %v", err)
	}
	if again != ref {
		t.Errorf("duplicate Start ref = %+v, want %+v", again, ref)
	}
}

func TestAdapterStartValidation(t *testing.T) {
	stub := newStubIngress(t)
	adapter := newTestAdapter(t, stub)
	ctx := t.Context()

	cases := []struct {
		name    string
		request durable.StartRequest
	}{
		{"empty key", durable.StartRequest{Handler: "greet", Key: ""}},
		{"empty handler", durable.StartRequest{Handler: "", Key: "run-1"}},
		{"unknown handler", durable.StartRequest{Handler: "missing", Key: "run-1"}},
		{"invalid payload", durable.StartRequest{Handler: "greet", Key: "run-1", Payload: []byte(`{oops`)}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := adapter.Start(ctx, tc.request); err == nil {
				t.Errorf("expected %s to fail, got nil", tc.name)
			}
		})
	}
}

func TestAdapterSignalRoundTrip(t *testing.T) {
	stub := newStubIngress(t)
	adapter := newTestAdapter(t, stub)
	stub.status["run-1"] = statusResponse{State: "running"}

	if err := adapter.Signal(t.Context(), durable.RunRef{Key: "run-1"}, "wake", []byte(`{"up":true}`)); err != nil {
		t.Fatalf("Signal: %v", err)
	}
	if stub.last.path != "/WorkflowRun/run-1/signal" {
		t.Errorf("request path = %s, want /WorkflowRun/run-1/signal", stub.last.path)
	}
	if len(stub.signals) != 1 || stub.signals[0].Name != "wake" || string(stub.signals[0].Payload) != `{"up":true}` {
		t.Errorf("signals = %+v, want the named payload", stub.signals)
	}
}

func TestAdapterStatusMapping(t *testing.T) {
	stub := newStubIngress(t)
	adapter := newTestAdapter(t, stub)
	ctx := t.Context()

	stub.status["run-1"] = statusResponse{State: "suspended", Detail: "waiting"}
	status, err := adapter.Status(ctx, durable.RunRef{Key: "run-1"})
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if status.State != durable.RunSuspended || status.Detail != "waiting" {
		t.Errorf("status = %+v, want suspended/waiting", status)
	}

	if _, err := adapter.Status(ctx, durable.RunRef{Key: "run-absent"}); !errors.Is(err, durable.ErrUnknownRun) {
		t.Errorf("unknown run status err = %v, want %v", err, durable.ErrUnknownRun)
	}
	if _, err := adapter.Status(ctx, durable.RunRef{}); err == nil {
		t.Error("expected empty key status to fail, got nil")
	}

	stub.status["run-broken"] = statusResponse{State: "napping"}
	if _, err := adapter.Status(ctx, durable.RunRef{Key: "run-broken"}); err == nil {
		t.Error("expected unknown state string to fail, got nil")
	}
}

func TestAdapterCancelConverges(t *testing.T) {
	stub := newStubIngress(t)
	adapter := newTestAdapter(t, stub)
	ctx := t.Context()

	stub.status["run-1"] = statusResponse{State: "running"}
	ref, err := adapter.Start(ctx, durable.StartRequest{Handler: "greet", Key: "run-1", Payload: []byte(`{}`)})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := adapter.Cancel(ctx, ref); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	if stub.last.method != http.MethodDelete || stub.last.path != "/restate/invocation/inv_run-1" {
		t.Errorf("request = %s %s, want DELETE /restate/invocation/inv_run-1", stub.last.method, stub.last.path)
	}

	stub.status["run-done"] = statusResponse{State: "succeeded"}
	if err := adapter.Cancel(ctx, durable.RunRef{Key: "run-done"}); err != nil {
		t.Errorf("cancel terminal run: %v", err)
	}
	if err := adapter.Cancel(ctx, durable.RunRef{Key: "run-absent"}); !errors.Is(err, durable.ErrUnknownRun) {
		t.Errorf("cancel unknown run err = %v, want %v", err, durable.ErrUnknownRun)
	}
}

func TestAdapterNotFoundMapping(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"message":"invocation not found","code":404}`))
	}))
	t.Cleanup(server.Close)
	adapter, err := NewAdapter(Config{IngressURL: server.URL}, nil)
	if err != nil {
		t.Fatalf("NewAdapter: %v", err)
	}
	adapter.RegisterHandler("greet", func(_ context.Context, _ durable.Invocation) error { return nil })

	if _, err := adapter.Status(t.Context(), durable.RunRef{Key: "run-1"}); !errors.Is(err, durable.ErrUnknownRun) {
		t.Errorf("status err = %v, want %v", err, durable.ErrUnknownRun)
	}
}

func TestAdapterConfigValidation(t *testing.T) {
	if _, err := NewAdapter(Config{}, nil); err == nil {
		t.Error("expected empty ingress URL to fail, got nil")
	}
	adapter, err := NewAdapter(Config{ServiceName: "Custom", IngressURL: "http://127.0.0.1:8080"}, nil)
	if err != nil {
		t.Fatalf("NewAdapter: %v", err)
	}
	if adapter.service != "Custom" {
		t.Errorf("service = %q, want Custom", adapter.service)
	}
}

func TestAdapterRegisterHandlerPanics(t *testing.T) {
	adapter, err := NewAdapter(Config{IngressURL: "http://127.0.0.1:8080"}, nil)
	if err != nil {
		t.Fatalf("NewAdapter: %v", err)
	}
	fn := func(_ context.Context, _ durable.Invocation) error { return nil }
	for _, tc := range []struct {
		name string
		call func()
	}{
		{"empty name", func() { adapter.RegisterHandler("", fn) }},
		{"nil function", func() { adapter.RegisterHandler("greet", nil) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Errorf("expected %s to panic", tc.name)
				}
			}()
			tc.call()
		})
	}
	adapter.RegisterHandler("greet", fn)
	defer func() {
		if recover() == nil {
			t.Error("expected duplicate registration to panic")
		}
	}()
	adapter.RegisterHandler("greet", fn)
}
