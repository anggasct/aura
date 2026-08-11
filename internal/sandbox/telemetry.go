package sandbox

import (
	"context"
	"log/slog"
	"time"

	"github.com/anggasct/aura/internal/approval"
)

// record emits one structured line per run with only the identifying and
// accounting fields an operator needs: request id, executable, the configured
// limits, the outcome, and the duration. Arguments, captured output, and
// environment values are deliberately absent so a secret carried by the
// request can never reach the log.
func (r *Registry) record(ctx context.Context, req *SandboxRequest, grant *approval.ApprovalGrant, result Result, runErr error, duration time.Duration) {
	r.logger.LogAttrs(ctx, slog.LevelInfo, "sandbox run",
		slog.String("request_id", req.RequestID),
		slog.String("executable", req.Executable),
		slog.String("tool", req.ToolName),
		slog.String("approval_grant_id", grant.GrantID),
		slog.String("result_code", classifyResult(result, runErr)),
		slog.Int("exit_code", result.ExitCode),
		slog.Bool("terminated", result.Terminated),
		slog.Bool("truncated", result.Truncated),
		slog.Duration("duration", duration),
		slog.Int64("memory_bytes", req.Limits.MemoryBytes),
		slog.Duration("cpu_time", req.Limits.CPUTime),
		slog.Duration("timeout", req.Limits.Timeout),
		slog.Int64("max_output_bytes", req.Limits.MaxOutputBytes),
		slog.Int64("max_open_files", req.Limits.MaxOpenFiles),
		slog.Int64("max_processes", req.Limits.MaxProcesses),
		slog.Int64("file_bytes", req.Limits.FileBytes),
	)
}

// recordDenied logs a grant resolution failure before any child starts, so an
// operator can see replay or tampering attempts without the request's secrets.
func (r *Registry) recordDenied(ctx context.Context, req *SandboxRequest, err error) {
	code := "approval_invalid"
	if c, ok := CodeOf(err); ok {
		code = string(c)
	}
	r.logger.LogAttrs(ctx, slog.LevelInfo, "sandbox run denied",
		slog.String("request_id", req.RequestID),
		slog.String("executable", req.Executable),
		slog.String("tool", req.ToolName),
		slog.String("result_code", code),
	)
}

func classifyResult(result Result, runErr error) string {
	if runErr != nil {
		if code, ok := CodeOf(runErr); ok {
			return string(code)
		}
		return "error"
	}
	if result.Terminated {
		return "terminated"
	}
	return "exited"
}
