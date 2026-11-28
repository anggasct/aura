package schedule

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/operation"
)

func webToolsRig(t *testing.T, webTools WebToolsOffered) *scheduleRig {
	t.Helper()
	return newConfiguredScheduleRig(t, &rigOptions{owner: ownerRoute{alias: "me", route: "telegram:owner"}, webTools: webTools})
}

func offered(value bool, err error) WebToolsOffered {
	return func(context.Context) (bool, error) { return value, err }
}

func TestWebOnlyJobNeedsAPersonaThatOffersAWebTool(t *testing.T) {
	webOnly := `{"name":"nightly","schedule":"0 21 * * *","timezone":"UTC","prompt":"summarize","context":"web_only",` + destination + `}`
	full := ownerAddInput
	cases := map[string]struct {
		webTools WebToolsOffered
		input    string
		refused  bool
	}{
		"a persona that offers a web tool":      {offered(true, nil), webOnly, false},
		"a persona that offers none":            {offered(false, nil), webOnly, true},
		"a persona that cannot be resolved":     {offered(false, errors.New("registry unavailable")), webOnly, false},
		"no persona check wired":                {nil, webOnly, false},
		"a full job whatever the persona lists": {offered(false, nil), full, false},
	}
	for name, tc := range cases {
		for _, surface := range []operation.Surface{operation.SurfaceCLI, operation.SurfaceChat} {
			t.Run(name+" on "+string(surface), func(t *testing.T) {
				rig := webToolsRig(t, tc.webTools)
				_, err := rig.Registry.Preview(t.Context(), "cron.add", &operation.Call{Caller: ownerCaller(surface), Input: json.RawMessage(tc.input)})
				if tc.refused {
					if !fault.HasCode(err, operation.ErrorCodeInputInvalid) || !strings.Contains(err.Error(), "web_search or web_fetch") {
						t.Errorf("preview = %v, want %s naming the web tools", err, operation.ErrorCodeInputInvalid)
					}
					return
				}
				if err != nil {
					t.Errorf("preview = %v, want it accepted", err)
				}
			})
		}
	}
	t.Run("the apply refuses too, when the persona changed after the preview", func(t *testing.T) {
		offer := true
		rig := webToolsRig(t, func(context.Context) (bool, error) { return offer, nil })
		call := &operation.Call{Caller: ownerCaller(operation.SurfaceCLI), Input: json.RawMessage(webOnly), IdempotencyKey: "web-only-add", RequestDigest: operation.Digest(json.RawMessage(webOnly))}
		preview, err := rig.Registry.Preview(t.Context(), "cron.add", call)
		if err != nil {
			t.Fatalf("Preview: %v", err)
		}
		offer = false
		call.PreviewDigest, call.Confirmation = preview.Digest, preview.Code
		if _, err := rig.Registry.Invoke(t.Context(), "cron.add", call); !fault.HasCode(err, operation.ErrorCodeInputInvalid) {
			t.Errorf("apply = %v, want %s", err, operation.ErrorCodeInputInvalid)
		}
		if listed := rig.jobCount(t); listed != 0 {
			t.Errorf("a refused apply stored %d jobs", listed)
		}
	})
}

func TestConfirmToWebOnlyNeedsAPersonaThatOffersAWebTool(t *testing.T) {
	for name, tc := range map[string]struct {
		webTools WebToolsOffered
		refused  bool
	}{
		"a persona that offers a web tool":  {offered(true, nil), false},
		"a persona that offers none":        {offered(false, nil), true},
		"a persona that cannot be resolved": {offered(false, errors.New("registry unavailable")), false},
	} {
		t.Run(name, func(t *testing.T) {
			rig := webToolsRig(t, tc.webTools)
			job := rig.createTainted(t)
			input := `{"id":"` + job.ID + `","context":"web_only"}`
			_, err := rig.Registry.Preview(t.Context(), "cron.confirm", &operation.Call{Caller: ownerCaller(operation.SurfaceChat), Input: json.RawMessage(input)})
			if tc.refused != fault.HasCode(err, operation.ErrorCodeInputInvalid) || (!tc.refused && err != nil) {
				t.Errorf("preview = %v, want refused=%t", err, tc.refused)
			}
			if got := rig.job(t, job.ID); !got.Tainted || got.Context != ContextFull {
				t.Errorf("the preview changed the job: %+v", got)
			}
		})
	}
}

func (r *scheduleRig) jobCount(t *testing.T) int {
	t.Helper()
	var count int
	if err := r.db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM scheduled_job`).Scan(&count); err != nil {
		t.Fatalf("count jobs: %v", err)
	}
	return count
}

func TestListShowsADashAsTheContextOfAWorkflowJob(t *testing.T) {
	rig := webToolsRig(t, nil)
	prompt, workflow := defaultRunSpec(), defaultRunSpec()
	workflow.Prompt, workflow.Context, workflow.WorkflowID = "", "", "wf1"
	for _, spec := range []*JobSpec{prompt, workflow} {
		if _, err := rig.service.Create(t.Context(), spec); err != nil {
			t.Fatalf("Create: %v", err)
		}
	}
	result, err := rig.Registry.Invoke(t.Context(), "cron.list", &operation.Call{Caller: ownerCaller(operation.SurfaceCLI), Input: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatalf("cron.list: %v", err)
	}
	var listed struct {
		Rows []struct {
			Context string `json:"context"`
		} `json:"rows"`
	}
	if err := json.Unmarshal(result.Data, &listed); err != nil {
		t.Fatalf("result %s: %v", result.Data, err)
	}
	var contexts []string
	for _, row := range listed.Rows {
		contexts = append(contexts, row.Context)
	}
	slices.Sort(contexts)
	if !slices.Equal(contexts, []string{"-", ContextFull}) {
		t.Errorf("contexts = %q, want the prompt job's context and a dash for the workflow job (result %s)", contexts, result.Data)
	}
}
