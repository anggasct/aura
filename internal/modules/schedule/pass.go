package schedule

import (
	"context"
	"errors"
	"time"
)

const passLimit = 512

type PassStore interface {
	ActiveJobs(ctx context.Context, limit int) ([]Job, error)
	FiredOccurrences(ctx context.Context, limit int) ([]Occurrence, error)
	ActiveOccurrence(ctx context.Context, jobID string) (Occurrence, bool, error)
}

type RunsRequest struct {
	Starter Starter
	Store   PassStore
	Now     time.Time
}

type passTally struct {
	started  int
	problems []error
}

func (p *passTally) add(err error) {
	if err != nil {
		p.problems = append(p.problems, err)
		return
	}
	p.started++
}

func StartRuns(ctx context.Context, req *RunsRequest) (int, error) {
	if req == nil || req.Starter == nil || req.Store == nil {
		return 0, Errorf(ErrorCodeInvalidArgument, "schedule pass needs a starter and a store")
	}
	fired, err := req.Store.FiredOccurrences(ctx, passLimit)
	if err != nil {
		return 0, err
	}
	tally := &passTally{}
	for i := range fired {
		tally.add(StartFire(ctx, &FireRequest{Starter: req.Starter, JobID: fired[i].JobID, Instant: fired[i].ScheduledForUTC}))
	}
	jobs, err := req.Store.ActiveJobs(ctx, passLimit)
	if err != nil {
		return tally.started, errors.Join(append(tally.problems, err)...)
	}
	for i := range jobs {
		_, busy, err := req.Store.ActiveOccurrence(ctx, jobs[i].ID)
		if err == nil && busy {
			continue
		}
		if err == nil {
			err = StartNext(ctx, &NextRequest{Starter: req.Starter, Job: &jobs[i], After: req.Now})
		}
		tally.add(err)
	}
	return tally.started, errors.Join(tally.problems...)
}
