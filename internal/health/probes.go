package health

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync/atomic"
	"time"
)

// ProbeBody is the complete surface of an HTTP probe response: status, one
// stable code, the build version, and the check time. Findings and component
// detail never appear here — they are local CLI surfaces only.
type ProbeBody struct {
	Status    string    `json:"status"`
	Code      string    `json:"code"`
	Version   string    `json:"version,omitempty"`
	CheckedAt time.Time `json:"checked_at"`
}

// Probe status and code values. Stable for monitors.
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

// Liveness answers only whether the process event loop is alive. It is true
// from process start and stays true through graceful drain; only a detected
// irrecoverable wedge clears it. Dependency degradation never touches it.
type Liveness struct {
	alive atomic.Bool
}

func NewLiveness() *Liveness {
	l := &Liveness{}
	l.alive.Store(true)
	return l
}

func (l *Liveness) Alive() bool { return l.alive.Load() }

// SetIrrecoverable clears liveness for a condition the process cannot
// recover from. There is no setter back to alive by design.
func (l *Liveness) SetIrrecoverable() { l.alive.Store(false) }

// Readiness reports whether new ingress can be durably accepted. The
// startup and draining states are lifecycle facts owned by the process;
// intake-blocking findings are classified from the evaluation result.
type Readiness struct {
	started  atomic.Bool
	draining atomic.Bool
}

func NewReadiness() *Readiness { return &Readiness{} }

func (r *Readiness) SetStarted()               { r.started.Store(true) }
func (r *Readiness) SetDraining(draining bool) { r.draining.Store(draining) }
func (r *Readiness) Draining() bool            { return r.draining.Load() }

// intakeBlocking reports whether a finding must keep new work out. Provider
// and backup degradation do not: they degrade without blocking intake. Any
// non-healthy migration state blocks (writing events against a schema this
// binary did not fully migrate is never safe intake), as does a missing
// mandatory sandbox: the sandbox check reports that as degraded, and intake
// must still stay closed until containment is restored.
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

// Probe evaluates readiness from the lifecycle state plus the current
// findings. The returned code names the first blocking condition in a stable
// order so a 503 is always explainable.
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

// FindBlocking returns the intake-blocking findings in stable order.
func FindBlocking(findings []Finding) []Finding {
	var blocking []Finding
	for i := range findings {
		if intakeBlocking(&findings[i]) {
			blocking = append(blocking, findings[i])
		}
	}
	return blocking
}

// LivenessHandler serves GET/HEAD /livez. The response never depends on an
// external system, so a provider or storage outage cannot cause restart
// loops through this endpoint.
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

// ReadinessHandler serves GET/HEAD /readyz. Evaluation runs under the
// caller-provided context; findings are recomputed per request so the probe
// reflects the current state, never a long-lived cache.
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

// ComponentStorage names the storage component; the migration, backup, and
// artifact checks are its sub-surfaces but storage-level failures (read-only,
// full, busy) report under this component.
const ComponentStorage = "storage"
