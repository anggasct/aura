package schedule

import "time"

const (
	outcomeResult     = "result"
	outcomeOverloaded = "overloaded"
	outcomeExpired    = "expired"
	outcomeCancelled  = "cancelled"
)

type turnOutcome struct {
	Kind          string    `json:"kind"`
	TurnID        string    `json:"turn_id,omitempty"`
	Text          string    `json:"text,omitempty"`
	Succeeded     bool      `json:"succeeded,omitempty"`
	SafeErrorCode string    `json:"safe_error_code,omitempty"`
	Destination   string    `json:"destination,omitempty"`
	First         time.Time `json:"first,omitzero"`
	Deadline      time.Time `json:"deadline,omitzero"`
}

func resultOutcome(result *TurnResult, destination string) turnOutcome {
	return turnOutcome{
		Kind: outcomeResult, TurnID: result.TurnID, Text: result.Text, Succeeded: result.Succeeded,
		SafeErrorCode: result.SafeErrorCode, Destination: destination,
	}
}

type submission struct {
	occurrence *Occurrence
	attempt    int
	first      time.Time
}

type admission struct {
	first    time.Time
	deadline time.Time
	expired  bool
}

func admissionOf(job *Job, sub *submission, now time.Time) admission {
	grace := graceOf(job)
	first := sub.first
	if sub.attempt == 1 {
		first = now
	}
	instant := sub.occurrence.ScheduledForUTC
	reference := instant
	if isManual(instant) {
		reference = first
	}
	deadline := reference.Add(grace)
	if sub.attempt > 1 {
		return admission{first: first, deadline: deadline, expired: !now.Before(deadline)}
	}
	stale := !isManual(instant) && now.Sub(sub.occurrence.CreatedAt) > grace
	return admission{first: first, deadline: deadline, expired: stale}
}
