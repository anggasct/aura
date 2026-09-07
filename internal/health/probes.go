package health

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync/atomic"
	"time"
)

type ProbeBody struct {
	Status    string    `json:"status"`
	Code      string    `json:"code"`
	Version   string    `json:"version,omitempty"`
	CheckedAt time.Time `json:"checked_at"`
}

const (
	ProbeStatusAlive    = "alive"
	ProbeStatusReady    = "ready"
	ProbeStatusNotReady = "not_ready"

	ProbeCodeStarting      = "starting"
	ProbeCodeDraining      = "draining"
	ProbeCodeReady         = "ready"
	ProbeCodeReadyDegraded = "degraded"
	ProbeCodeIrrecoverable = "irrecoverable"
)

type Liveness struct {
	alive atomic.Bool
}

func NewLiveness() *Liveness {
	l := &Liveness{}
	l.alive.Store(true)
	return l
}

func (l *Liveness) Alive() bool { return l.alive.Load() }

func (l *Liveness) SetIrrecoverable() { l.alive.Store(false) }

type Readiness struct {
	started  atomic.Bool
	draining atomic.Bool
}

func NewReadiness() *Readiness { return &Readiness{} }

func (r *Readiness) SetStarted()               { r.started.Store(true) }
func (r *Readiness) SetDraining(draining bool) { r.draining.Store(draining) }
func (r *Readiness) Draining() bool            { return r.draining.Load() }

func intakeBlocking(f *Finding) bool {
	switch {
	case f.Component == ComponentMigration || f.Component == ComponentSandbox:
		return f.Status != StatusUp
	case f.Component == ComponentStorage || strings.HasPrefix(f.Component, ComponentStorage+"/"):
		return f.Status == StatusDown || f.Status == StatusUnknown
	case f.Component == ComponentCapability:
		return f.Status == StatusDown
	default:
		return false
	}
}

func (r *Readiness) Probe(findings []Finding) (ready bool, code string) {
	if !r.started.Load() {
		return false, ProbeCodeStarting
	}
	if r.draining.Load() {
		return false, ProbeCodeDraining
	}
	blocking := FindBlocking(findings)
	if len(blocking) > 0 {
		return false, blocking[0].Component + "." + blocking[0].Code
	}
	if WorstStatus(findings) != StatusUp {
		return true, ProbeCodeReadyDegraded
	}
	return true, ProbeCodeReady
}

func FindBlocking(findings []Finding) []Finding {
	var blocking []Finding
	for i := range findings {
		if intakeBlocking(&findings[i]) {
			blocking = append(blocking, findings[i])
		}
	}
	return blocking
}

func LivenessHandler(live *Liveness, version string, now func() time.Time) http.HandlerFunc {
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		body := ProbeBody{Version: version, CheckedAt: now()}
		status := http.StatusOK
		if live.Alive() {
			body.Status = ProbeStatusAlive
			body.Code = ProbeStatusAlive
		} else {
			body.Status = ProbeStatusNotReady
			body.Code = ProbeCodeIrrecoverable
			status = http.StatusServiceUnavailable
		}
		writeProbe(w, r, status, body)
	}
}

func ReadinessHandler(ready *Readiness, evaluate func(ctx context.Context) []Finding, version string, now func() time.Time) http.HandlerFunc {
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		findings := evaluate(r.Context())
		ok, code := ready.Probe(findings)
		body := ProbeBody{Code: code, Version: version, CheckedAt: now()}
		status := http.StatusOK
		if ok {
			body.Status = ProbeStatusReady
		} else {
			body.Status = ProbeStatusNotReady
			status = http.StatusServiceUnavailable
		}
		writeProbe(w, r, status, body)
	}
}

func writeProbe(w http.ResponseWriter, r *http.Request, status int, body ProbeBody) {
	encoded, err := json.Marshal(body)
	if err != nil {
		http.Error(w, "probe encode failed", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	if r.Method == http.MethodHead {
		return
	}
	_, _ = w.Write(encoded)
}

const ComponentStorage = "storage"
