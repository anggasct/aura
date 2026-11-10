package codeexec

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/platform/approval"
	"github.com/anggasct/aura/internal/platform/sandbox"
	"github.com/anggasct/aura/internal/platform/toolbroker"
	"github.com/anggasct/aura/internal/platform/tools"
)

func codeRequest(arguments string) *toolbroker.ToolRequest {
	return &toolbroker.ToolRequest{ToolName: tools.CodeExecuteToolName, ToolVersion: "v1", Arguments: json.RawMessage(arguments)}
}

func brokerConstraints() approval.Constraints {
	return approval.Constraints{MaxOutputBytes: 64 * 1024, Timeout: 30 * time.Second}
}

func notPerformedCode(err error) (string, bool) {
	var target *toolbroker.NotPerformedError
	if !errors.As(err, &target) {
		return "", false
	}
	return target.Code, true
}

func TestDecodeArgumentsIsStrict(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		ok   bool
	}{
		{"minimal", `{"runtime":"python","source":"x"}`, true},
		{"with stdin", `{"runtime":"python","source":"x","stdin":"y"}`, true},
		{"unknown field", `{"runtime":"python","source":"x","env":{"A":"b"}}`, false},
		{"with output files", `{"runtime":"python","source":"x","output_files":["a"]}`, true},
		{"output files of the wrong type", `{"runtime":"python","source":"x","output_files":"a"}`, false},
		{"arguments are not accepted", `{"runtime":"python","source":"x","args":["a"]}`, false},
		{"wrong type", `{"runtime":"python","source":1}`, false},
		{"trailing content", `{"runtime":"python","source":"x"} {}`, false},
		{"not an object", `["python"]`, false},
		{"empty", ``, false},
		{"null", `null`, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := decodeArguments(json.RawMessage(tc.raw))
			if (err == nil) != tc.ok {
				t.Fatalf("decodeArguments(%s) = %v, want ok = %v", tc.raw, err, tc.ok)
			}
			if err != nil && !hasErrorCode(err, ErrorCodeInvalidArgument) {
				t.Fatalf("error = %v, want invalid_argument", err)
			}
		})
	}
}

func TestArgumentCheckEnforcesTheConfiguredLimits(t *testing.T) {
	rig := newRig(t, nil)
	cases := []struct {
		name string
		raw  string
		ok   bool
	}{
		{"within limits", `{"runtime":"python","source":"` + strings.Repeat("x", 1<<10) + `","stdin":"` + strings.Repeat("y", 1<<10) + `"}`, true},
		{"source above its limit", `{"runtime":"python","source":"` + strings.Repeat("x", 1<<10+1) + `"}`, false},
		{"stdin above its limit", `{"runtime":"python","source":"x","stdin":"` + strings.Repeat("y", 1<<10+1) + `"}`, false},
		{"malformed", `{"runtime":`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := rig.runner.ArgumentCheck(json.RawMessage(tc.raw))
			if (err == nil) != tc.ok {
				t.Fatalf("ArgumentCheck = %v, want ok = %v", err, tc.ok)
			}
			if err != nil && strings.Contains(err.Error(), "xxxx") {
				t.Fatalf("the error echoes the program: %v", err)
			}
		})
	}
}

func TestAdapterRunsAProgramAndEncodesTheResult(t *testing.T) {
	rig := newRig(t, nil)
	rig.stub.result = sandbox.Result{Stdout: "hello\n", Stderr: "warn"}
	result, err := rig.runner.Adapter()(t.Context(), codeRequest(`{"runtime":"python","source":"print('hello')"}`), brokerConstraints())
	if err != nil {
		t.Fatal(err)
	}
	if result.Class != toolbroker.ResultOK {
		t.Fatalf("class = %s", result.Class)
	}
	var decoded map[string]any
	if err := json.Unmarshal(result.Output, &decoded); err != nil {
		t.Fatalf("output is not JSON: %v", err)
	}
	if decoded["result_class"] != "ok" || decoded["stdout"] != "hello\n" || decoded["stderr"] != "warn" || decoded["exit_code"] != float64(0) {
		t.Fatalf("output = %s", result.Output)
	}
	if !strings.HasPrefix(string(result.Output), `{"result_class":"ok","exit_code":0,"signal":0,"duration_ms":`) {
		t.Fatalf("key order changed: %s", result.Output)
	}
}

func TestAdapterMapsResultClassesToBrokerClasses(t *testing.T) {
	cases := []struct {
		name   string
		result sandbox.Result
		want   toolbroker.ResultClass
		class  string
	}{
		{"failure exit", sandbox.Result{ExitCode: 1}, toolbroker.ResultExecutionFailed, "exit_nonzero"},
		{"timeout", sandbox.Result{Terminated: true, TimedOut: true}, toolbroker.ResultDeadlineExceeded, "timeout"},
		{"policy kill", sandbox.Result{ExitCode: -1, Signal: int(syscall.SIGSYS)}, toolbroker.ResultPolicyDenied, "policy_denied"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rig := newRig(t, nil)
			rig.stub.result = tc.result
			result, err := rig.runner.Adapter()(t.Context(), codeRequest(`{"runtime":"python","source":"x"}`), brokerConstraints())
			if err != nil {
				t.Fatal(err)
			}
			var decoded encodedResult
			if err := json.Unmarshal(result.Output, &decoded); err != nil {
				t.Fatal(err)
			}
			if result.Class != tc.want || string(decoded.ResultClass) != tc.class {
				t.Fatalf("class = %s / %s, want %s / %s", result.Class, decoded.ResultClass, tc.want, tc.class)
			}
		})
	}
}

func TestAdapterKeepsHugeOutputInsideTheBrokerBound(t *testing.T) {
	rig := newRig(t, func(l *Limits) { l.MaxStdoutBytes, l.MaxStderrBytes = 1<<20, 1<<20 })
	rig.stub.result = sandbox.Result{Stdout: strings.Repeat("\x01é\"", 200000), Stderr: strings.Repeat("\\", 200000)}
	for _, bound := range []int64{64 * 1024, 4 * 1024, 0} {
		constraints := brokerConstraints()
		constraints.MaxOutputBytes = bound
		result, err := rig.runner.Adapter()(t.Context(), codeRequest(`{"runtime":"python","source":"x"}`), constraints)
		if err != nil {
			t.Fatal(err)
		}
		limit := bound
		if limit <= 0 {
			limit = defaultMaxResult
		}
		if int64(len(result.Output)) > limit || !json.Valid(result.Output) {
			t.Fatalf("bound %d: %d bytes, valid = %v", bound, len(result.Output), json.Valid(result.Output))
		}
		var decoded encodedResult
		if err := json.Unmarshal(result.Output, &decoded); err != nil {
			t.Fatal(err)
		}
		if !decoded.StdoutTruncated || !decoded.StderrTruncated {
			t.Fatalf("bound %d: both streams exceed it and must be flagged: %v %v", bound, decoded.StdoutTruncated, decoded.StderrTruncated)
		}
	}
}

func TestAdapterRefusesBeforeAnyEffect(t *testing.T) {
	cases := []struct {
		name     string
		request  *toolbroker.ToolRequest
		wantCode string
		wantType toolbroker.ResultClass
	}{
		{"unknown runtime", codeRequest(`{"runtime":"ruby","source":"x"}`), "runtime_unavailable", toolbroker.ResultCapabilityUnavailable},
		{"unknown field", codeRequest(`{"runtime":"python","source":"x","env":{}}`), "invalid_argument", toolbroker.ResultInvalidArgument},
		{"oversized source", codeRequest(`{"runtime":"python","source":"` + strings.Repeat("x", 1<<10+1) + `"}`), "invalid_argument", toolbroker.ResultInvalidArgument},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rig := newRig(t, nil)
			_, err := rig.runner.Adapter()(t.Context(), tc.request, brokerConstraints())
			code, notPerformed := notPerformedCode(err)
			class, _ := toolbroker.CodeOf(err)
			if !notPerformed || code != tc.wantCode || class != tc.wantType {
				t.Fatalf("error = %v (code %q, class %q), want %q and %q", err, code, class, tc.wantCode, tc.wantType)
			}
			if rig.stub.count() != 0 {
				t.Fatalf("a refused call started %d sandbox runs", rig.stub.count())
			}
		})
	}
}

func TestAdapterRefusesAnUnverifiedRuntime(t *testing.T) {
	rig := newRigWith(t, nil, []rigRuntime{
		{id: "python", version: rigPythonVersion},
		{id: "javascript", version: "26.13.0"},
	})
	for _, runtime := range []string{"python", "javascript"} {
		request := codeRequest(`{"runtime":"` + runtime + `","source":"x"}`)
		_, err := rig.runner.Adapter()(t.Context(), request, brokerConstraints())
		class, _ := toolbroker.CodeOf(err)
		if code, ok := notPerformedCode(err); !ok || code != "runtime_unavailable" || class != toolbroker.ResultCapabilityUnavailable {
			t.Fatalf("%s: error = %v", runtime, err)
		}
	}
	if rig.stub.count() != 0 || len(rig.runDirs(t)) != 0 {
		t.Fatalf("an unverified runtime started %d runs and left %v", rig.stub.count(), rig.runDirs(t))
	}
}

func TestAdapterRejectsForeignRequests(t *testing.T) {
	rig := newRig(t, nil)
	cases := []struct {
		name    string
		request *toolbroker.ToolRequest
	}{
		{"nil", nil},
		{"another tool", &toolbroker.ToolRequest{ToolName: "exec", ToolVersion: "v1", Arguments: json.RawMessage(`{}`)}},
		{"another version", &toolbroker.ToolRequest{ToolName: tools.CodeExecuteToolName, ToolVersion: "v2", Arguments: json.RawMessage(`{}`)}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := rig.runner.Adapter()(t.Context(), tc.request, brokerConstraints())
			if class, _ := toolbroker.CodeOf(err); class != toolbroker.ResultInvalidArgument {
				t.Fatalf("error = %v, want invalid_argument", err)
			}
		})
	}
	if rig.stub.count() != 0 {
		t.Fatalf("a foreign request started %d runs", rig.stub.count())
	}
}

func TestAdapterAnswersACancelledCallWithoutLeakingProgramData(t *testing.T) {
	rig := newRig(t, nil)
	ctx, cancel := context.WithCancel(t.Context())
	rig.stub.before = func(context.Context, *sandbox.Spec) { cancel() }
	rig.stub.err = sandbox.Errorf(sandbox.ErrorCodeSandboxInitFailed, "init failed")
	_, err := rig.runner.Adapter()(ctx, codeRequest(`{"runtime":"python","source":"SECRET"}`), brokerConstraints())
	if code, ok := notPerformedCode(err); !ok || code != "cancelled" || !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want a cancelled call", err)
	}
}

func TestAdapterHidesSandboxFailureDetail(t *testing.T) {
	cases := []struct {
		name     string
		err      error
		wantCode string
		wantType toolbroker.ResultClass
	}{
		{"init failure", sandbox.Errorf(sandbox.ErrorCodeSandboxInitFailed, "mount /home/ubuntu/secret failed"), "run_failed", toolbroker.ResultExecutionFailed},
		{"uncoded failure", errors.New("open /home/ubuntu/secret"), "run_failed", toolbroker.ResultExecutionFailed},
		{"sandbox unavailable", sandbox.Errorf(sandbox.ErrorCodeSandboxUnavailable, "landlock is missing"), "runtime_unavailable", toolbroker.ResultCapabilityUnavailable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rig := newRig(t, nil)
			rig.stub.err = tc.err
			_, err := rig.runner.Adapter()(t.Context(), codeRequest(`{"runtime":"python","source":"SECRET"}`), brokerConstraints())
			code, ok := notPerformedCode(err)
			class, _ := toolbroker.CodeOf(err)
			if !ok || code != tc.wantCode || class != tc.wantType {
				t.Fatalf("error = %v (code %q, class %q), want %q and %q", err, code, class, tc.wantCode, tc.wantType)
			}
			if strings.Contains(err.Error(), "/home/") || strings.Contains(err.Error(), "SECRET") || strings.Contains(err.Error(), "landlock") {
				t.Fatalf("the error leaks detail: %v", err)
			}
		})
	}
}

func TestAdapterAppliesTheBrokerDeadlineAndTimeout(t *testing.T) {
	rig := newRig(t, nil)
	var deadline time.Time
	rig.stub.before = func(ctx context.Context, _ *sandbox.Spec) { deadline, _ = ctx.Deadline() }
	request := codeRequest(`{"runtime":"python","source":"x"}`)
	request.Deadline = time.Now().Add(time.Hour)
	constraints := brokerConstraints()
	constraints.Timeout = 3 * time.Second
	if _, err := rig.runner.Adapter()(t.Context(), request, constraints); err != nil {
		t.Fatal(err)
	}
	if !deadline.Equal(request.Deadline) {
		t.Fatalf("the sandbox context deadline = %v, want the broker deadline %v", deadline, request.Deadline)
	}
	if got := rig.stub.last(t).spec.Limits.CPUTime; got != 3*time.Second {
		t.Fatalf("cpu budget = %v, want the approval timeout of 3s", got)
	}
}
