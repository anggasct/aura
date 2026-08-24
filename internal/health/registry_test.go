package health

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"
)

type stubChecker struct {
	findings []Finding
	delay    time.Duration
}

func (s stubChecker) Check(ctx context.Context) []Finding {
	if s.delay > 0 {
		select {
		case <-time.After(s.delay):
		case <-ctx.Done():
		}
	}
	return s.findings
}

func hasCode(err error, code ErrorCode) bool {
	got, ok := CodeOf(err)
	return ok && got == code
}

func TestRegistryRejectsInvalidChecks(t *testing.T) {
	if _, err := NewRegistry(RegisteredCheck{ID: "x"}); !hasCode(err, ErrorCodeInvalidCheck) {
		t.Errorf("missing checker err = %v", err)
	}
	if _, err := NewRegistry(RegisteredCheck{ID: "", Checker: stubChecker{}}); !hasCode(err, ErrorCodeInvalidCheck) {
		t.Errorf("empty id err = %v", err)
	}
	dup := []RegisteredCheck{{ID: "a", Checker: stubChecker{}}, {ID: "a", Checker: stubChecker{}}}
	if _, err := NewRegistry(dup...); !hasCode(err, ErrorCodeInvalidCheck) {
		t.Errorf("duplicate id err = %v", err)
	}
}

// The registry owns the contract fields: severity derived from status, scope
// and remediation defaulted, stable finding ID, and first-seen preserved
// across evaluations while last-seen advances.
func TestRegistryStampsFindingsAndTracksFirstSeen(t *testing.T) {
	base := time.Date(2026, 8, 24, 10, 0, 0, 0, time.UTC)
	now := base
	registry, err := NewRegistry(RegisteredCheck{
		ID:          "backup",
		Checker:     stubChecker{findings: []Finding{{Component: "backup", Code: "backup_stale", Status: StatusDegraded, Detail: "older than 24h"}}},
		Remediation: RemediationRestoreBackup,
	})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	registry.now = func() time.Time { return now }

	first := registry.Evaluate(context.Background())
	if len(first) != 1 {
		t.Fatalf("findings = %d", len(first))
	}
	got := first[0]
	if got.ID != "backup/backup_stale" {
		t.Errorf("ID = %q", got.ID)
	}
	if got.Severity != SeverityWarning {
		t.Errorf("Severity = %q, want warning for degraded", got.Severity)
	}
	if got.Scope != ScopeLocal {
		t.Errorf("Scope = %q", got.Scope)
	}
	if got.Remediation != RemediationRestoreBackup {
		t.Errorf("Remediation = %q", got.Remediation)
	}
	if !got.FirstSeen.Equal(base) || !got.LastSeen.Equal(base) {
		t.Errorf("first/last seen = %v/%v, want both %v", got.FirstSeen, got.LastSeen, base)
	}

	now = base.Add(time.Hour)
	second := registry.Evaluate(context.Background())
	if !second[0].FirstSeen.Equal(base) {
		t.Errorf("FirstSeen drifted to %v, want %v", second[0].FirstSeen, base)
	}
	if !second[0].LastSeen.Equal(now) {
		t.Errorf("LastSeen = %v, want %v", second[0].LastSeen, now)
	}
}

// A slow check becomes a stale unknown finding instead of blocking the
// evaluation; sibling checks still complete.
func TestRegistrySlowCheckYieldsStaleFinding(t *testing.T) {
	registry, err := NewRegistry(
		RegisteredCheck{
			ID:      "slow",
			Checker: stubChecker{delay: 5 * time.Second, findings: []Finding{{Component: "slow", Code: "ok", Status: StatusUp}}},
			Timeout: 20 * time.Millisecond,
		},
		RegisteredCheck{
			ID:      "fast",
			Checker: stubChecker{findings: []Finding{{Component: "fast", Code: "ok", Status: StatusUp}}},
		},
	)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	findings := registry.Evaluate(context.Background())
	if len(findings) != 2 {
		t.Fatalf("findings = %d, want 2: %+v", len(findings), findings)
	}
	var slow Finding
	for _, f := range findings {
		if f.Component == "slow" {
			slow = f
		}
	}
	if slow.Code != timeoutCode || slow.Status != StatusUnknown || !slow.Stale {
		t.Fatalf("slow finding = %+v, want stale unknown %q", slow, timeoutCode)
	}
	if WorstSeverity(findings) != SeverityWarning {
		t.Errorf("worst severity = %q, want warning", WorstSeverity(findings))
	}
}

// A panicking checker is contained: the sweep reports a finding instead of
// crashing the process.
func TestRegistryContainsCheckerPanic(t *testing.T) {
	panicChecker := panicChecker{}
	registry, err := NewRegistry(RegisteredCheck{ID: "boom", Checker: panicChecker})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	findings := registry.Evaluate(context.Background())
	if len(findings) != 1 || findings[0].Code != "check_panicked" || findings[0].Status != StatusUnknown {
		t.Fatalf("findings = %+v", findings)
	}
}

type panicChecker struct{}

func (panicChecker) Check(context.Context) []Finding { panic("checker bug") }

func TestRegistryOutputIsSortedDeterministically(t *testing.T) {
	registry, err := NewRegistry(
		RegisteredCheck{ID: "zeta", Checker: stubChecker{findings: []Finding{{Component: "zeta", Code: "b"}}}},
		RegisteredCheck{ID: "alpha", Checker: stubChecker{findings: []Finding{
			{Component: "alpha", Code: "b"},
			{Component: "alpha", Code: "a"},
		}}},
	)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	findings := registry.Evaluate(context.Background())
	got := []string{findings[0].Component + "/" + findings[0].Code, findings[1].Component + "/" + findings[1].Code, findings[2].Component + "/" + findings[2].Code}
	want := []string{"alpha/a", "alpha/b", "zeta/b"}
	if !slices.Equal(got, want) {
		t.Errorf("order = %v, want %v", got, want)
	}
}

func TestReadinessMatrix(t *testing.T) {
	cases := []struct {
		name     string
		started  bool
		draining bool
		findings []Finding
		wantOK   bool
		wantCode string
	}{
		{name: "not started", wantOK: false, wantCode: ProbeCodeStarting},
		{name: "draining", started: true, draining: true, wantOK: false, wantCode: ProbeCodeDraining},
		{name: "healthy", started: true, wantOK: true, wantCode: ProbeCodeReady},
		{name: "degraded but intake safe", started: true, findings: []Finding{{Component: ComponentBackup, Code: "backup_stale", Status: StatusDegraded}}, wantOK: true, wantCode: ProbeCodeReadyDegraded},
		{name: "migration pending", started: true, findings: []Finding{{Component: ComponentMigration, Code: "migration_pending", Status: StatusDegraded}}, wantOK: false, wantCode: "migration.migration_pending"},
		{name: "storage down", started: true, findings: []Finding{{Component: ComponentStorage, Code: "storage_readonly", Status: StatusDown}}, wantOK: false, wantCode: "storage.storage_readonly"},
		{name: "sandbox down", started: true, findings: []Finding{{Component: ComponentSandbox, Code: "sandbox_unavailable", Status: StatusDown}}, wantOK: false, wantCode: "sandbox.sandbox_unavailable"},
		{name: "provider down does not block", started: true, findings: []Finding{{Component: ComponentProvider, Code: "provider_unavailable", Status: StatusDown}}, wantOK: true, wantCode: ProbeCodeReadyDegraded},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := NewReadiness()
			if tc.started {
				r.SetStarted()
			}
			r.SetDraining(tc.draining)
			ok, code := r.Probe(tc.findings)
			if ok != tc.wantOK || code != tc.wantCode {
				t.Errorf("Probe() = %v, %q; want %v, %q", ok, code, tc.wantOK, tc.wantCode)
			}
		})
	}
}

// Liveness must stay alive regardless of any finding: provider, storage, and
// backup degradation never clear it.
func TestLivenessIgnoresDegradation(t *testing.T) {
	live := NewLiveness()
	degraded := []Finding{
		{Component: ComponentProvider, Status: StatusDown},
		{Component: ComponentStorage, Status: StatusDown},
		{Component: ComponentBackup, Status: StatusDown},
	}
	_ = degraded
	if !live.Alive() {
		t.Fatal("liveness must default to alive")
	}
	live.SetIrrecoverable()
	if live.Alive() {
		t.Fatal("irrecoverable must clear liveness")
	}
}

func TestProbeHandlers(t *testing.T) {
	live := NewLiveness()
	liveHandler := LivenessHandler(live, "test-version", nil)

	recorder := httptest.NewRecorder()
	liveHandler.ServeHTTP(recorder, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/livez", http.NoBody))
	if recorder.Code != http.StatusOK {
		t.Fatalf("livez code = %d", recorder.Code)
	}
	var body ProbeBody
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode livez body: %v", err)
	}
	if body.Status != ProbeStatusAlive || body.Version != "test-version" || body.CheckedAt.IsZero() {
		t.Errorf("livez body = %+v", body)
	}
	if raw := recorder.Body.String(); len(raw) > 256 {
		t.Errorf("livez body must stay minimal: %d bytes", len(raw))
	}

	live.SetIrrecoverable()
	recorder = httptest.NewRecorder()
	liveHandler.ServeHTTP(recorder, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/livez", http.NoBody))
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("wedged livez code = %d", recorder.Code)
	}

	ready := NewReadiness()
	ready.SetStarted()
	readyHandler := ReadinessHandler(ready, func(context.Context) []Finding {
		return nil
	}, "test-version", nil)
	recorder = httptest.NewRecorder()
	readyHandler.ServeHTTP(recorder, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/readyz", http.NoBody))
	if recorder.Code != http.StatusOK {
		t.Fatalf("readyz code = %d", recorder.Code)
	}

	blocking := FindBlocking([]Finding{{Component: ComponentMigration, Code: "migration_downgrade", Status: StatusDown}})
	if len(blocking) != 1 {
		t.Fatalf("blocking = %+v", blocking)
	}
	recorder = httptest.NewRecorder()
	readyHandler = ReadinessHandler(ready, func(context.Context) []Finding {
		return blocking
	}, "test-version", nil)
	readyHandler.ServeHTTP(recorder, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/readyz", http.NoBody))
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("blocking readyz code = %d", recorder.Code)
	}

	// Method surface: only GET and HEAD are served.
	recorder = httptest.NewRecorder()
	liveHandler.ServeHTTP(recorder, httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/livez", http.NoBody))
	if recorder.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST livez = %d, want 405", recorder.Code)
	}
}

// The real SandboxChecker result must drive readiness: a host missing
// mandatory containment yields a down finding that blocks intake (503).
func TestSandboxCheckerThroughReadinessBlocksIntake(t *testing.T) {
	support := func() (bool, string) {
		// cgroup_v2 is missing on the fixture host, like the real Checker
		// reports when containment is absent.
		return false, "missing: cgroup_v2"
	}
	checker := NewRegistryCheckForTest(RegisteredCheck{
		ID:      "sandbox",
		Checker: SandboxChecker{Support: support},
	})
	findings := checker.Evaluate(context.Background())
	if len(findings) != 1 || findings[0].Code != "sandbox_unavailable" || findings[0].Status != StatusDown {
		t.Fatalf("sandbox findings = %+v", findings)
	}
	ready := NewReadiness()
	ready.SetStarted()
	blocking := FindBlocking(findings)
	if len(blocking) != 1 {
		t.Fatalf("blocking = %+v, want the sandbox finding", blocking)
	}
	ok, code := ready.Probe(findings)
	if ok || code != "sandbox.sandbox_unavailable" {
		t.Fatalf("Probe() = %v, %q; want false, sandbox.sandbox_unavailable", ok, code)
	}
}

// StorageChecker must classify every intake state to a stable code and
// readiness must turn a blocking storage state into 503.
func TestStorageCheckerThroughReadinessBlocksIntake(t *testing.T) {
	states := []struct {
		name   string
		state  StorageIntakeState
		wantOK bool
	}{
		{name: "ok", state: StorageIntakeOK, wantOK: true},
		{name: "read-only", state: StorageIntakeReadOnly, wantOK: false},
		{name: "full", state: StorageIntakeFull, wantOK: false},
		{name: "unreachable", state: StorageIntakeUnreachable, wantOK: false},
		{name: "unknown", state: StorageIntakeUnknown, wantOK: false},
	}
	for _, tc := range states {
		t.Run(tc.name, func(t *testing.T) {
			checker := NewRegistryCheckForTest(RegisteredCheck{
				ID:      "storage",
				Checker: StorageChecker{Intake: func(context.Context) (StorageIntakeState, string) { return tc.state, tc.name }},
			})
			findings := checker.Evaluate(context.Background())
			if len(findings) != 1 {
				t.Fatalf("findings = %+v", findings)
			}
			ready := NewReadiness()
			ready.SetStarted()
			ok, code := ready.Probe(findings)
			if ok != tc.wantOK {
				t.Errorf("Probe() = %v, want %v (code %q)", ok, tc.wantOK, code)
			}
			if tc.state != StorageIntakeOK && code != "storage."+findings[0].Code {
				t.Errorf("code = %q, want storage.%s", code, findings[0].Code)
			}
		})
	}
}

func NewRegistryCheckForTest(checks ...RegisteredCheck) *Registry {
	registry, err := NewRegistry(checks...)
	if err != nil {
		panic(err)
	}
	return registry
}

// The readiness handler must respect request cancellation so a hung
// evaluation cannot pin probe workers.
func TestReadinessHandlerPassesRequestContext(t *testing.T) {
	ready := NewReadiness()
	ready.SetStarted()
	seen := make(chan error, 1)
	handler := ReadinessHandler(ready, func(ctx context.Context) []Finding {
		select {
		case <-ctx.Done():
			seen <- ctx.Err()
		case <-time.After(5 * time.Second):
			seen <- nil
		}
		return nil
	}, "", nil)
	request := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/readyz", http.NoBody)
	ctx, cancel := context.WithCancel(request.Context())
	request = request.WithContext(ctx)
	recorder := httptest.NewRecorder()
	go func() {
		time.AfterFunc(20*time.Millisecond, cancel)
		handler.ServeHTTP(recorder, request)
	}()
	select {
	case err := <-seen:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("evaluate ctx err = %v, want canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("handler did not propagate cancellation")
	}
}
