package cli

import (
	"bytes"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/spf13/cobra"
)

func processProbeOutput(t *testing.T, listen, probe string) (string, error) {
	t.Helper()
	cmd := &cobra.Command{Use: "status"}
	cmd.SetContext(t.Context())
	var out bytes.Buffer
	cmd.SetOut(&out)
	err := runProcessProbe(cmd, listen, probe)
	return out.String(), err
}

func TestStatusProbeFollowsTheRunningProcess(t *testing.T) {
	var ready atomic.Int32
	ready.Store(http.StatusOK)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/livez":
			w.WriteHeader(http.StatusOK)
		case "/readyz":
			w.WriteHeader(int(ready.Load()))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)
	listen := strings.TrimPrefix(server.URL, "http://")

	if out, err := processProbeOutput(t, listen, "ready"); err != nil || !strings.Contains(out, "ready: yes") {
		t.Fatalf("ready probe = %q, %v", out, err)
	}
	ready.Store(http.StatusServiceUnavailable)
	var coded *exitCodeError
	if out, err := processProbeOutput(t, listen, "ready"); !errors.As(err, &coded) || coded.code != exitCritical || !strings.Contains(out, "ready: no") {
		t.Fatalf("not-ready probe = %q, %v", out, err)
	}
	if out, err := processProbeOutput(t, listen, "live"); err != nil || !strings.Contains(out, "live: yes") {
		t.Fatalf("live probe while not ready = %q, %v", out, err)
	}
	server.Close()
	if out, err := processProbeOutput(t, listen, "live"); !errors.As(err, &coded) || coded.code != exitCritical || !strings.Contains(out, "live: unreachable") {
		t.Fatalf("stopped probe = %q, %v", out, err)
	}
}

func TestStatusProbeRejectsAnUnknownProbe(t *testing.T) {
	_, err := processProbeOutput(t, "127.0.0.1:1", "startup")
	var coded *exitCodeError
	if !errors.As(err, &coded) || coded.code != exitCommand {
		t.Fatalf("unknown probe = %v", err)
	}
}
