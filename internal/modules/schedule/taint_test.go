package schedule

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/operation"
	"github.com/anggasct/aura/internal/platform/operation/operationtest"
	"github.com/anggasct/aura/internal/platform/principal"
	"github.com/anggasct/aura/internal/platform/sqlite/sqlitetest"
	"github.com/anggasct/aura/internal/platform/turn"
	turnstore "github.com/anggasct/aura/internal/platform/turn/store"
)

const (
	hostileSource = "a webhook"
	destination   = `"channel":"telegram","destination":"me"`
	ownerAddInput      = `{"name":"nightly","schedule":"0 21 * * *","timezone":"UTC","prompt":"summarize","context":"full",` + destination + `}`
)

func taintedCaller(source string) operation.Caller {
	caller := ownerCaller(operation.SurfaceChat)
	caller.Tainted, caller.TaintSource = true, source
	return caller
}

func ownerCaller(surface operation.Surface) operation.Caller {
	return operation.Caller{Surface: surface, PrincipalID: principal.OwnerID, Class: principal.ClassOwner, Direct: true}
}

func (r *scheduleRig) createTainted(t *testing.T) Job {
	t.Helper()
	spec := defaultRunSpec()
	spec.TaintSource = hostileSource
	job, err := r.service.Create(t.Context(), spec)
	if err != nil {
		t.Fatalf("Create(tainted): %v", err)
	}
	return job
}

func (r *scheduleRig) job(t *testing.T, id string) Job {
	t.Helper()
	job, err := r.service.Get(t.Context(), id)
	if err != nil {
		t.Fatalf("Get(%s): %v", id, err)
	}
	return job
}

func (r *scheduleRig) preview(t *testing.T, id, input string, surface operation.Surface) operation.Preview {
	t.Helper()
	preview, err := r.Registry.Preview(t.Context(), id, &operation.Call{Caller: ownerCaller(surface), Input: json.RawMessage(input)})
	if err != nil {
		t.Fatalf("Preview(%s): %v", id, err)
	}
	return preview
}

func TestScheduleCreatedOnATaintedConversationRunsTainted(t *testing.T) {
	t.Run("the operation stores the label of the conversation it ran in", func(t *testing.T) {
		rig := newScheduleRig(t, ownerRoute{alias: "me", route: "telegram:owner"})
		caller := taintedCaller(hostileSource)
		result, err := rig.InvokePreviewed(t, "cron.add", &operation.Call{Caller: caller, Input: json.RawMessage(ownerAddInput), IdempotencyKey: "tainted-add", RequestDigest: operation.Digest(json.RawMessage(ownerAddInput))})
		if err != nil {
			t.Fatalf("cron.add: %v", err)
		}
		var added struct{ ID string }
		if err := json.Unmarshal(result.Data, &added); err != nil {
			t.Fatalf("result %s: %v", result.Data, err)
		}
		if job := rig.job(t, added.ID); !job.Tainted || job.TaintSource != hostileSource {
			t.Errorf("job = %+v, want tainted by %q", job, hostileSource)
		}
	})
	t.Run("a conversation with no label still marks the job", func(t *testing.T) {
		rig := newScheduleRig(t, ownerRoute{alias: "me", route: "telegram:owner"})
		caller := taintedCaller("")
		result, err := rig.InvokePreviewed(t, "cron.add", &operation.Call{Caller: caller, Input: json.RawMessage(ownerAddInput), IdempotencyKey: "unlabelled-add", RequestDigest: operation.Digest(json.RawMessage(ownerAddInput))})
		if err != nil {
			t.Fatalf("cron.add: %v", err)
		}
		var added struct{ ID string }
		if err := json.Unmarshal(result.Data, &added); err != nil {
			t.Fatal(err)
		}
		if job := rig.job(t, added.ID); !job.Tainted || job.TaintSource != unknownTaintLabel {
			t.Errorf("job = %+v, want tainted by %q", job, unknownTaintLabel)
		}
	})
	t.Run("an owner surface leaves the job clean", func(t *testing.T) {
		rig := newScheduleRig(t, ownerRoute{alias: "me", route: "telegram:owner"})
		result, err := rig.InvokePreviewed(t, "cron.add", &operation.Call{Caller: ownerCaller(operation.SurfaceChat), Input: json.RawMessage(ownerAddInput), IdempotencyKey: "clean-add", RequestDigest: operation.Digest(json.RawMessage(ownerAddInput))})
		if err != nil {
			t.Fatalf("cron.add: %v", err)
		}
		var added struct{ ID string }
		if err := json.Unmarshal(result.Data, &added); err != nil {
			t.Fatal(err)
		}
		if job := rig.job(t, added.ID); job.Tainted || job.TaintSource != "" {
			t.Errorf("job = %+v, want clean", job)
		}
	})
	t.Run("every run carries the label", func(t *testing.T) {
		inBubble(t, func(spec *JobSpec) { spec.TaintSource = hostileSource }, func(t *testing.T, f *runFixture) {
			f.succeed(t, f.scheduled(t, f.nextFire(t)))
			requests := f.turns.requests()
			if len(requests) != 1 || requests[0].TaintSource != hostileSource || requests[0].JobID != f.job.ID {
				t.Fatalf("turn requests = %+v, want one carrying %q", requests, hostileSource)
			}
		})
	})
}

func TestEachScheduledRunGetsItsOwnSession(t *testing.T) {
	inBubble(t, nil, func(t *testing.T, f *runFixture) {
		fire := f.nextFire(t)
		keys := []string{f.scheduled(t, fire)}
		for range 2 {
			occurrence, err := f.service.RunNow(t.Context(), f.job.ID, time.Now())
			if err != nil {
				t.Fatalf("RunNow(): %v", err)
			}
			keys = append(keys, f.recorded(t, occurrence.ScheduledForUTC))
		}
		for _, key := range keys {
			f.succeed(t, key)
		}
		seen := map[string]bool{}
		for _, request := range f.turns.requests() {
			if !strings.HasPrefix(request.SessionID, "cron:"+f.job.ID+":") || seen[request.SessionID] {
				t.Errorf("session %q is shared or not per run", request.SessionID)
			}
			seen[request.SessionID] = true
		}
		if len(seen) != 3 {
			t.Errorf("sessions = %v, want one for the fire and one for each run-now", seen)
		}
	})
	t.Run("a run-now in the same second as a fire has another session", func(t *testing.T) {
		fire := time.Date(2026, 9, 12, 21, 0, 0, 0, time.UTC)
		if sessionKey("job-1", fire) == sessionKey("job-1", fire.Add(5*time.Nanosecond)) {
			t.Error("a run-now shares the session of the fire it follows")
		}
	})
}

func TestConfirmedJobRunsClean(t *testing.T) {
	rig := newScheduleRig(t, ownerRoute{alias: "me", route: "telegram:owner"})
	job := rig.createTainted(t)
	input := `{"id":"` + job.ID + `"}`
	preview := rig.preview(t, "cron.confirm", input, operation.SurfaceChat)
	if preview.Code == "" || !strings.Contains(strings.Join(preview.Lines, "\n"), hostileSource) {
		t.Fatalf("preview = %+v, want the label and a code", preview)
	}
	if _, err := rig.Registry.Invoke(t.Context(), "cron.confirm", &operation.Call{
		Caller: ownerCaller(operation.SurfaceChat), Input: json.RawMessage(input), IdempotencyKey: "confirm-1",
		RequestDigest: operation.Digest(json.RawMessage(input)), PreviewDigest: preview.Digest, Confirmation: preview.Code,
	}); err != nil {
		t.Fatalf("cron.confirm: %v", err)
	}
	confirmed := rig.job(t, job.ID)
	if confirmed.Tainted || confirmed.TaintSource != "" || confirmed.Version != job.Version+1 {
		t.Errorf("confirmed job = %+v, want clean at version %d", confirmed, job.Version+1)
	}
	inBubble(t, func(spec *JobSpec) { spec.TaintSource = hostileSource }, func(t *testing.T, f *runFixture) {
		f.succeed(t, f.scheduled(t, f.nextFire(t)))
		if _, err := f.store.ConfirmJob(t.Context(), f.job.ID, &JobConfirmation{ExpectedVersion: f.job.Version}, time.Now()); err != nil {
			t.Fatalf("ConfirmJob: %v", err)
		}
		next := f.nextFire(t).Add(time.Minute)
		f.succeed(t, f.scheduled(t, next))
		requests := f.turns.requests()
		if len(requests) != 2 || requests[0].TaintSource == "" || requests[1].TaintSource != "" {
			t.Errorf("turn requests = %+v, want the first tainted and the next clean", requests)
		}
	})
}

func TestOneRunsTaintDoesNotReachTheNextRun(t *testing.T) {
	inBubble(t, nil, func(t *testing.T, f *runFixture) {
		f.succeed(t, f.scheduled(t, f.nextFire(t)))
		first := f.turns.requests()[0]
		tainted := f.store.jobs[f.job.ID]
		tainted.Tainted, tainted.TaintSource = true, hostileSource
		f.store.jobs[f.job.ID] = tainted
		later := f.nextFire(t).Add(time.Minute)
		f.succeed(t, f.scheduled(t, later))
		second := f.turns.requests()[1]
		if first.SessionID == second.SessionID || first.TaintSource != "" || second.TaintSource != hostileSource {
			t.Errorf("first %+v, second %+v, want separate sessions and a taint only on the second", first, second)
		}
	})
}

func TestOccurrenceRetentionKeepsRunSessions(t *testing.T) {
	db := sqlitetest.OpenMigrated(t, turnstore.Schema(), operation.Schema(), Schema())
	store, err := NewSQLiteStore(db)
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	if err := store.InsertJob(ctx, testScheduledJob("job-1")); err != nil {
		t.Fatalf("InsertJob: %v", err)
	}
	fire := time.Date(2026, 9, 12, 21, 0, 0, 0, time.UTC)
	now := fire.Add(time.Minute)
	if err := store.RecordFire(ctx, &Occurrence{ID: "cron-1", JobID: "job-1", JobVersion: 1, ScheduledForUTC: fire, State: OccurrenceFired, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("RecordFire: %v", err)
	}
	if err := store.SettleOccurrence(ctx, "cron-1", OccurrenceCompleted, "", "", "", now); err != nil {
		t.Fatalf("SettleOccurrence: %v", err)
	}
	sessions := turnstore.NewSessionService(db)
	runSession := sessionKey("job-1", fire)
	created := time.Now().UTC()
	if err := sessions.Create(ctx, &turn.Session{ID: runSession, OwnerID: principal.OwnerID, Metadata: json.RawMessage(`{}`), CreatedAt: created, UpdatedAt: created}); err != nil {
		t.Fatalf("create the run session: %v", err)
	}
	pruned, err := store.PruneOccurrences(ctx, "job-1", now.Add(time.Hour), 100)
	if err != nil || pruned != 1 {
		t.Fatalf("PruneOccurrences = %d, %v, want one occurrence", pruned, err)
	}
	if _, err := sessions.Get(ctx, runSession); err != nil {
		t.Errorf("the run session is gone after the occurrence was pruned: %v", err)
	}
}

func TestCronAddRequiresContextForPrompts(t *testing.T) {
	rig := newScheduleRig(t, ownerRoute{alias: "me", route: "telegram:owner"})
	const base = `"name":"nightly","schedule":"0 21 * * *","timezone":"UTC"`
	cases := map[string]struct {
		input string
		valid bool
	}{
		"a prompt without context":       {`{` + base + `,"prompt":"summarize"}`, false},
		"a prompt with an unknown value": {`{` + base + `,"prompt":"summarize","context":"private"}`, false},
		"a workflow with a context":      {`{` + base + `,"workflow":"wf1","context":"full"}`, false},
		"a full prompt":                  {`{` + base + `,"prompt":"summarize","context":"full"}`, true},
		"a web only prompt":              {`{` + base + `,"prompt":"summarize","context":"web_only"}`, true},
		"a workflow without context":     {`{` + base + `,"workflow":"wf1"}`, true},
	}
	for name, tc := range cases {
		for _, surface := range []operation.Surface{operation.SurfaceCLI, operation.SurfaceChat} {
			t.Run(name+" on "+string(surface), func(t *testing.T) {
				input := strings.Replace(tc.input, "}", ","+destination+"}", 1)
				_, err := rig.Registry.Preview(t.Context(), "cron.add", &operation.Call{Caller: ownerCaller(surface), Input: json.RawMessage(input)})
				if tc.valid != (err == nil) || (!tc.valid && !fault.HasCode(err, operation.ErrorCodeInputInvalid)) {
					t.Errorf("preview = %v, want valid=%t", err, tc.valid)
				}
			})
		}
		t.Run(name+" through the agent tool", func(t *testing.T) {
			outcome := rig.Execute(t.Context(), operationtest.RequestFor("aura_cron_add", json.RawMessage(tc.input), "ctx-"+strings.ReplaceAll(name, " ", "-")))
			if tc.valid != (outcome.Class == "ok") {
				t.Errorf("outcome = %+v, want valid=%t", outcome, tc.valid)
			}
		})
	}
}

func TestWebOnlyCardWarnsThePromptLeavesToWebsites(t *testing.T) {
	const base = `{"name":"nightly","schedule":"0 21 * * *","timezone":"UTC","prompt":"summarize"`
	cases := map[string]struct {
		context string
		want    []string
		absent  []string
	}{
		"web only": {context: "web_only", want: []string{contextWebOnlyLine, webOnlyLeavesLine}, absent: []string{contextFullLine}},
		"full":     {context: "full", want: []string{contextFullLine}, absent: []string{webOnlyLeavesLine, contextWebOnlyLine}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			rig := newScheduleRig(t, ownerRoute{alias: "me", route: "telegram:owner"})
			outcome := rig.Execute(t.Context(), operationtest.RequestFor("aura_cron_add", json.RawMessage(base+`,"context":"`+tc.context+`"}`), "card-"+tc.context))
			if outcome.Class != "ok" {
				t.Fatalf("outcome = %+v, want ok", outcome)
			}
			cards := rig.Prompts()
			if len(cards) != 1 {
				t.Fatalf("cards = %d, want one", len(cards))
			}
			for _, line := range tc.want {
				if !strings.Contains(cards[0].Arguments, line) {
					t.Errorf("the card lacks %q:\n%s", line, cards[0].Arguments)
				}
			}
			for _, line := range tc.absent {
				if strings.Contains(cards[0].Arguments, line) {
					t.Errorf("the card carries %q:\n%s", line, cards[0].Arguments)
				}
			}
		})
	}
}

func TestConfirmWithoutTheCodeIsRefused(t *testing.T) {
	rig := newScheduleRig(t, ownerRoute{alias: "me", route: "telegram:owner"})
	job := rig.createTainted(t)
	input := `{"id":"` + job.ID + `"}`
	preview := rig.preview(t, "cron.confirm", input, operation.SurfaceCLI)
	apply := func(code string) error {
		_, err := rig.Registry.Invoke(t.Context(), "cron.confirm", &operation.Call{
			Caller: ownerCaller(operation.SurfaceCLI), Input: json.RawMessage(input), IdempotencyKey: "confirm-" + code,
			RequestDigest: operation.Digest(json.RawMessage(input)), PreviewDigest: preview.Digest, Confirmation: code,
		})
		return err
	}
	for name, code := range map[string]string{"no code": "", "a wrong code": "0000-0000"} {
		if err := apply(code); !fault.HasCode(err, operation.ErrorCodeConfirmation) {
			t.Errorf("%s: err = %v, want %s", name, err, operation.ErrorCodeConfirmation)
		}
		if got := rig.job(t, job.ID); !got.Tainted || got.Version != job.Version {
			t.Errorf("%s changed the job: %+v", name, got)
		}
	}
	if _, err := rig.service.Edit(t.Context(), job.ID, &JobEdit{
		Name: "renamed", CronExpression: job.CronExpression, Timezone: job.Timezone, Prompt: job.Prompt,
		OriginChannel: job.OriginChannel, OriginDestination: job.OriginDestination, OverlapPolicy: job.OverlapPolicy, CatchUpGraceSeconds: job.CatchUpGraceSeconds,
	}); err != nil {
		t.Fatalf("Edit: %v", err)
	}
	if err := apply(preview.Code); err == nil {
		t.Error("the code of an earlier preview confirmed a job that changed since")
	}
	if got := rig.job(t, job.ID); !got.Tainted {
		t.Errorf("a refused confirmation cleared the mark: %+v", got)
	}
}

func TestConfirmCodeNeverEntersSessionEvents(t *testing.T) {
	rig := newScheduleRig(t, ownerRoute{alias: "me", route: "telegram:owner"})
	job := rig.createTainted(t)
	input := json.RawMessage(`{"id":"` + job.ID + `"}`)
	if descriptor, found := rig.Registry.Descriptor("cron.confirm"); !found || descriptor.Exposure != operation.ExposureOwner || descriptor.GeneratesTool() {
		t.Fatalf("cron.confirm = %+v, want an owner operation with no agent tool", descriptor)
	}
	agent := operation.Caller{Surface: operation.SurfaceAgent, PrincipalID: principal.OwnerID, Class: principal.ClassOwner}
	if preview, err := rig.Registry.Preview(t.Context(), "cron.confirm", &operation.Call{Caller: agent, Input: input}); err == nil || preview.Code != "" {
		t.Errorf("the agent surface previewed cron.confirm: %+v, %v", preview, err)
	}
	if _, err := rig.Registry.Invoke(t.Context(), "cron.confirm", &operation.Call{Caller: agent, Input: input}); err == nil {
		t.Error("the agent surface ran cron.confirm")
	}
	outcome := rig.Execute(t.Context(), operationtest.RequestFor("aura_cron_list", json.RawMessage(`{}`), "list-1"))
	if outcome.Class != "ok" || strings.Contains(string(outcome.Output), "--apply") {
		t.Errorf("the list the agent reads = %+v, want no confirmation command", outcome)
	}
	ownerPreview := rig.preview(t, "cron.confirm", string(input), operation.SurfaceChat)
	if !strings.Contains(strings.Join(ownerPreview.Lines, "\n"), ownerPreview.Code) || ownerPreview.Code == "" {
		t.Errorf("the owner preview = %+v, want the code printed only there", ownerPreview)
	}
}

func TestConfirmChangesContextOnlyThroughTheCode(t *testing.T) {
	rig := newScheduleRig(t, ownerRoute{alias: "me", route: "telegram:owner"})
	created, err := rig.service.Create(t.Context(), defaultRunSpec())
	if err != nil {
		t.Fatal(err)
	}
	input := `{"id":"` + created.ID + `","context":"web_only"}`
	preview := rig.preview(t, "cron.confirm", input, operation.SurfaceChat)
	if !strings.Contains(strings.Join(preview.Lines, "\n"), webOnlyLeavesLine) {
		t.Errorf("preview = %v, want the web-only warning", preview.Lines)
	}
	if got := rig.job(t, created.ID); got.Context != ContextFull {
		t.Fatalf("the preview changed the context: %+v", got)
	}
	if _, err := rig.Registry.Invoke(t.Context(), "cron.confirm", &operation.Call{
		Caller: ownerCaller(operation.SurfaceChat), Input: json.RawMessage(input), IdempotencyKey: "ctx-1",
		RequestDigest: operation.Digest(json.RawMessage(input)), PreviewDigest: preview.Digest, Confirmation: preview.Code,
	}); err != nil {
		t.Fatalf("cron.confirm: %v", err)
	}
	if got := rig.job(t, created.ID); got.Context != ContextWebOnly {
		t.Errorf("context = %q, want web_only", got.Context)
	}
	if empty := rig.preview(t, "cron.confirm", `{"id":"`+created.ID+`"}`, operation.SurfaceChat); !empty.Empty {
		t.Errorf("a clean job with nothing to change previewed %+v, want an empty preview", empty)
	}
}

func TestCronConfirmWritesOnlyInsideCommit(t *testing.T) {
	rig := newScheduleRig(t, ownerRoute{alias: "me", route: "telegram:owner"})
	job := rig.createTainted(t)
	operationtest.RequireWritesOnlyInCommit(t, &operationtest.WriteProbe{
		Registry: rig.Registry, DB: rig.db, ID: "cron.confirm", Input: json.RawMessage(`{"id":"` + job.ID + `"}`),
		Outside: rig.starter.fingerprint, Previewed: true,
	})
}
