package schedule

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/anggasct/aura/internal/platform/durable"
)

const fireKeyLayout = "20060102T150405.000000000Z"

type Starter interface {
	Start(ctx context.Context, req durable.StartRequest) (durable.RunRef, error)
}

type firePayload struct {
	JobID      string    `json:"job_id"`
	Instant    time.Time `json:"instant"`
	Chain      bool      `json:"chain"`
	Expression string    `json:"expression"`
	Timezone   string    `json:"timezone"`
}

func fireKey(jobID string, instant time.Time) string {
	return HandlerName + "/" + jobID + "/" + instant.UTC().Format(fireKeyLayout)
}

func (f *firePayload) validate() error {
	if strings.TrimSpace(f.JobID) == "" || f.Instant.IsZero() {
		return Errorf(ErrorCodeInvalidArgument, "schedule run needs a job and an instant")
	}
	if f.Chain && (f.Expression == "" || f.Timezone == "") {
		return Errorf(ErrorCodeInvalidArgument, "scheduled run needs its expression and timezone")
	}
	return nil
}

func decodeFire(raw []byte) (firePayload, error) {
	var fire firePayload
	if err := json.Unmarshal(raw, &fire); err != nil {
		return firePayload{}, Errorf(ErrorCodeInvalidArgument, "schedule run payload is not valid")
	}
	if err := fire.validate(); err != nil {
		return firePayload{}, err
	}
	fire.Instant = fire.Instant.UTC()
	return fire, nil
}

type FireRequest struct {
	Starter    Starter
	JobID      string
	Instant    time.Time
	Chain      bool
	Expression string
	Timezone   string
	Reuse      durable.ReusePolicy
}

func StartFire(ctx context.Context, req *FireRequest) error {
	if req == nil || req.Starter == nil {
		return Errorf(ErrorCodeInvalidArgument, "fire start needs a starter")
	}
	fire := firePayload{
		JobID: req.JobID, Instant: req.Instant.UTC(), Chain: req.Chain,
		Expression: req.Expression, Timezone: req.Timezone,
	}
	if err := fire.validate(); err != nil {
		return err
	}
	payload, err := json.Marshal(fire)
	if err != nil {
		return err
	}
	_, err = req.Starter.Start(ctx, durable.StartRequest{Handler: HandlerName, Key: fireKey(fire.JobID, fire.Instant), Payload: payload, Reuse: req.Reuse})
	return err
}

type NextRequest struct {
	Starter Starter
	Job     *Job
	After   time.Time
}

func StartNext(ctx context.Context, req *NextRequest) error {
	if req == nil || req.Job == nil {
		return Errorf(ErrorCodeInvalidArgument, "next fire start needs a job")
	}
	fire, err := nextFireOf(req.Job, req.After)
	if err != nil {
		return err
	}
	return StartFire(ctx, &FireRequest{
		Starter: req.Starter, JobID: req.Job.ID, Instant: fire, Chain: true,
		Expression: req.Job.CronExpression, Timezone: req.Job.Timezone,
	})
}

func nextFireOf(job *Job, after time.Time) (time.Time, error) {
	schedule, err := ParseExpression(job.CronExpression)
	if err != nil {
		return time.Time{}, err
	}
	location, err := time.LoadLocation(job.Timezone)
	if err != nil {
		return time.Time{}, Errorf(ErrorCodeTimezoneInvalid, "timezone is not a valid IANA name")
	}
	return NextFire(&schedule, location, after)
}
