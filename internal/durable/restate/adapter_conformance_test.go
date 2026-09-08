package restate

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/anggasct/aura/internal/durable"
	"github.com/anggasct/aura/internal/durable/durabletest"
)

type conformanceHarness struct {
	fake    *durable.Fake
	adapter *Adapter
}

type statusErrorBody struct {
	Message string `json:"message"`
	Code    int    `json:"code"`
}

func newConformanceHarness(t *testing.T) *conformanceHarness {
	t.Helper()
	harness := &conformanceHarness{fake: durable.NewFake()}
	mux := http.NewServeMux()
	mux.HandleFunc("/", harness.handle)
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	adapter, err := NewAdapter(Config{IngressURL: server.URL}, nil)
	if err != nil {
		t.Fatalf("NewAdapter: %v", err)
	}
	harness.adapter = adapter
	return harness
}

func (h *conformanceHarness) syncHandlers() {
	h.adapter.mu.Lock()
	handlers := make(map[string]durable.Handler, len(h.adapter.handlers))
	for name, fn := range h.adapter.handlers {
		handlers[name] = fn
	}
	h.adapter.mu.Unlock()
	for name, fn := range handlers {
		h.fake.RegisterHandler(name, fn)
	}
}

func writeConformanceError(w http.ResponseWriter, code int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	encoded, err := json.Marshal(statusErrorBody{Message: message, Code: code})
	if err != nil {
		return
	}
	_, _ = w.Write(encoded)
}

func (h *conformanceHarness) handle(w http.ResponseWriter, r *http.Request) {
	h.syncHandlers()
	w.Header().Set("Content-Type", "application/json")
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	switch {
	case r.Method == http.MethodPost && len(parts) == 4 && parts[3] == "send":
		key := parts[1]
		handler := parts[2]
		body, _ := io.ReadAll(r.Body)
		if _, err := h.fake.Start(r.Context(), durable.StartRequest{Handler: handler, Key: key, Payload: body}); err != nil {
			writeConformanceError(w, http.StatusBadRequest, err.Error())
			return
		}
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"invocationId":"inv_` + key + `","status":"accepted"}`))
	case r.Method == http.MethodPost && len(parts) == 3 && parts[2] == signalMethod:
		key := parts[1]
		body, _ := io.ReadAll(r.Body)
		var request signalRequest
		if err := json.Unmarshal(body, &request); err != nil {
			writeConformanceError(w, http.StatusBadRequest, err.Error())
			return
		}
		if err := h.fake.Signal(r.Context(), durable.RunRef{Key: key}, request.Name, []byte(request.Payload)); err != nil {
			if errors.Is(err, durable.ErrUnknownRun) {
				writeConformanceError(w, http.StatusNotFound, err.Error())
				return
			}
			writeConformanceError(w, http.StatusInternalServerError, err.Error())
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	case r.Method == http.MethodPost && len(parts) == 3 && parts[2] == statusMethod:
		key := parts[1]
		status, err := h.fake.Status(r.Context(), durable.RunRef{Key: key})
		if err != nil {
			if errors.Is(err, durable.ErrUnknownRun) {
				encoded, merr := json.Marshal(statusResponse{State: unknownRunState})
				if merr != nil {
					writeConformanceError(w, http.StatusInternalServerError, merr.Error())
					return
				}
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write(encoded)
				return
			}
			writeConformanceError(w, http.StatusInternalServerError, err.Error())
			return
		}
		encoded, merr := json.Marshal(statusResponse{State: string(status.State), Detail: status.Detail})
		if merr != nil {
			writeConformanceError(w, http.StatusInternalServerError, merr.Error())
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(encoded)
	case r.Method == http.MethodDelete && len(parts) == 3 && parts[0] == "restate" && parts[1] == "invocation":
		key := strings.TrimPrefix(parts[2], "inv_")
		if err := h.fake.Cancel(r.Context(), durable.RunRef{Key: key}); err != nil {
			if errors.Is(err, durable.ErrUnknownRun) {
				writeConformanceError(w, http.StatusNotFound, err.Error())
				return
			}
			writeConformanceError(w, http.StatusInternalServerError, err.Error())
			return
		}
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{}`))
	default:
		writeConformanceError(w, http.StatusNotFound, "not found")
	}
}

func TestAdapterPortConformance(t *testing.T) {
	harness := newConformanceHarness(t)
	durabletest.ExerciseRuntime(t, harness.adapter)
}

func TestAdapterCancelWithoutHandleReturnsUnknownRun(t *testing.T) {
	stub := newStubIngress(t)
	first, err := NewAdapter(Config{IngressURL: stub.server.URL}, nil)
	if err != nil {
		t.Fatalf("NewAdapter: %v", err)
	}
	first.RegisterHandler("greet", func(_ context.Context, _ *durable.Invocation) error { return nil })
	stub.status["run-fresh"] = statusResponse{State: "running"}
	ref, err := first.Start(t.Context(), durable.StartRequest{Handler: "greet", Key: "run-fresh", Payload: []byte(`{}`)})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	fresh, err := NewAdapter(Config{IngressURL: stub.server.URL}, nil)
	if err != nil {
		t.Fatalf("NewAdapter: %v", err)
	}
	fresh.RegisterHandler("greet", func(_ context.Context, _ *durable.Invocation) error { return nil })
	if err := fresh.Cancel(t.Context(), ref); !errors.Is(err, durable.ErrUnknownRun) {
		t.Fatalf("Cancel without handle err = %v, want %v", err, durable.ErrUnknownRun)
	}
}

func TestAdapterEmptyPayloadAccepted(t *testing.T) {
	stub := newStubIngress(t)
	adapter := newTestAdapter(t, stub)
	ctx := t.Context()
	for _, tc := range []struct {
		name    string
		payload []byte
	}{
		{"nil", nil},
		{"empty", []byte{}},
		{"object", []byte(`{}`)},
	} {
		t.Run("start/"+tc.name, func(t *testing.T) {
			if _, err := adapter.Start(ctx, durable.StartRequest{Handler: "greet", Key: "run-" + tc.name, Payload: tc.payload}); err != nil {
				t.Fatalf("Start with %s payload: %v", tc.name, err)
			}
		})
		t.Run("signal/"+tc.name, func(t *testing.T) {
			if err := adapter.Signal(ctx, durable.RunRef{Key: "run-1"}, "wake", tc.payload); err != nil {
				t.Fatalf("Signal with %s payload: %v", tc.name, err)
			}
		})
	}
}
