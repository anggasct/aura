package schedule

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"

	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/operation"
)

type confirmation struct {
	job     Job
	context string
}

func (o *Operations) confirmation(ctx context.Context, call *operation.Call) (*confirmation, error) {
	var in jobInput
	if err := decodeInput(call, &in); err != nil {
		return nil, err
	}
	if strings.TrimSpace(in.ID) == "" {
		return nil, usage("the job id must not be blank")
	}
	job, err := o.service.Get(ctx, in.ID)
	if err != nil {
		return nil, operation.ModuleFailure[ErrorCode](err)
	}
	if job.State == JobDeleted {
		return nil, usage("the job is deleted")
	}
	if in.Context != "" && job.WorkflowID != "" {
		return nil, usage("context applies to a prompt, not to a workflow")
	}
	if in.Context == job.Context {
		in.Context = ""
	}
	return &confirmation{job: job, context: in.Context}, nil
}

func (c *confirmation) runSpec() *JobSpec {
	return &JobSpec{WorkflowID: c.job.WorkflowID, Context: cmp.Or(c.context, c.job.Context)}
}

func (c *confirmation) nothingToDo() bool {
	return !c.job.Tainted && c.context == ""
}

func (c *confirmation) lines(route string) []string {
	job := &c.job
	lines := []string{
		"Job: " + job.ID,
		"Name: " + job.Name,
		"State: " + job.State,
		fmt.Sprintf("Schedule: %s (%s)", job.CronExpression, job.Timezone),
	}
	if job.WorkflowID != "" {
		lines = append(lines, "Starts workflow: "+job.WorkflowID)
		if job.WorkflowInput != "" {
			lines = append(lines, "Workflow input: "+job.WorkflowInput)
		}
	} else {
		lines = append(lines, "Runs prompt: "+job.Prompt)
		lines = append(lines, contextLines(job.Context)...)
	}
	lines = append(lines, "Channel: "+job.OriginChannel, "Destination alias: "+job.OriginDestination)
	if route != "" {
		lines = append(lines, "Delivers to: "+route)
	}
	return append(lines, c.changes()...)
}

func (c *confirmation) changes() []string {
	var lines []string
	if c.job.Tainted {
		lines = append(lines,
			"Marked as created in a conversation with untrusted content: "+c.job.TaintSource,
			"Confirming clears the mark: the next runs are no longer tainted.")
	}
	if c.context != "" {
		lines = append(lines, "Context changes to: "+c.context)
		lines = append(lines, contextLines(c.context)...)
	}
	return lines
}

func (c *confirmation) digest(lines []string) string {
	sum := sha256.Sum256([]byte(strings.Join(lines, "\n") + "\nversion=" + strconv.FormatInt(c.job.Version, 10) + "\ncontext=" + c.context))
	return hex.EncodeToString(sum[:])
}

func (o *Operations) previewConfirm(ctx context.Context, call *operation.Call) (operation.Preview, error) {
	pending, err := o.confirmation(ctx, call)
	if err != nil {
		return operation.Preview{}, err
	}
	if err := o.checkRun(ctx, pending.runSpec()); err != nil {
		return operation.Preview{}, err
	}
	lines := pending.lines(o.routeOf(pending.job.OriginDestination))
	if pending.nothingToDo() {
		return operation.Preview{Lines: []string{"nothing to confirm: the job carries no taint mark and the context is unchanged"}, Empty: true}, nil
	}
	return operation.Preview{Lines: lines, Digest: pending.digest(lines)}, nil
}

func (o *Operations) confirm(ctx context.Context, call *operation.Call) (operation.Result, error) {
	pending, err := o.confirmation(ctx, call)
	if err != nil {
		return operation.Result{}, err
	}
	if err := o.checkRun(ctx, pending.runSpec()); err != nil {
		return operation.Result{}, err
	}
	return call.Commit(ctx, func(ctx context.Context) (operation.Result, error) {
		if pending.nothingToDo() || pending.digest(pending.lines(o.routeOf(pending.job.OriginDestination))) != call.PreviewDigest {
			return operation.Result{}, stalePreview("the job changed after the preview")
		}
		confirmed, err := o.service.Confirm(ctx, pending.job.ID, &JobConfirmation{ExpectedVersion: pending.job.Version, ExpectedState: pending.job.State, Context: pending.context})
		if fault.HasCode(err, ErrorCodeOccurrenceConflict) {
			return operation.Result{}, stalePreview("the job changed after the preview")
		}
		if err != nil {
			return operation.Result{}, operation.ModuleFailure[ErrorCode](err)
		}
		return encodeResult(map[string]any{"id": confirmed.ID, "context": listedContext(&confirmed), "tainted": confirmed.Tainted})
	})
}

func stalePreview(detail string) error {
	return operation.NotPerformed(operation.ErrorCodePreviewStale, operation.NewError(operation.ErrorCodePreviewStale, detail))
}
