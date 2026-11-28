package schedule

import (
	"encoding/json"

	"github.com/anggasct/aura/internal/platform/operation"
)

const (
	GroupName      = "cron"
	operationAdd   = "cron.add"
	operationList  = "cron.list"
	operationPause = "cron.pause"
	operationResum = "cron.resume"
	operationDel   = "cron.delete"
	operationRuns  = "cron.runs"
	operationNow   = "cron.run-now"

	maxJobIDBytes    = 128
	listedJobsLimit  = 200
	maxRunsPerListed = 1000
	defaultRunsLimit = "50"
)

func ownerOnlyChannels() []operation.Surface {
	return []operation.Surface{operation.SurfaceCLI, operation.SurfaceChat}
}

func jobIDParam() operation.Param {
	return operation.Param{Name: "id", Kind: operation.KindString, Summary: "job id", Required: true, Position: 1, MinLength: 1, MaxLength: maxJobIDBytes}
}

func stateOutput() operation.Output {
	return operation.Output{Kind: operation.OutputRecord, Fields: []operation.OutputField{{Name: "id"}, {Name: "state"}}}
}

func addParams() []operation.Param {
	return []operation.Param{
		{Name: "name", Kind: operation.KindString, Summary: "job name", Required: true, MinLength: 1, MaxLength: maxNameRunes},
		{Name: "schedule", Kind: operation.KindString, Summary: "five-field cron expression", Required: true, MinLength: 1, MaxLength: 256},
		{Name: "timezone", Kind: operation.KindString, Summary: "IANA timezone (default from config)", MaxLength: 64},
		{Name: "channel", Kind: operation.KindString, Summary: "origin channel source", Required: true, MinLength: 1, MaxLength: maxOriginRunes, Surfaces: ownerOnlyChannels()},
		{Name: "destination", Kind: operation.KindString, Summary: "origin destination alias", Required: true, MinLength: 1, MaxLength: maxOriginRunes, Surfaces: ownerOnlyChannels()},
		{Name: "prompt", Kind: operation.KindString, Summary: "turn prompt", MinLength: 1, MaxLength: maxPromptBytes},
		{Name: "workflow", Kind: operation.KindString, Summary: "workflow or routine id to start instead of a prompt", MinLength: 1, MaxLength: maxJobIDBytes},
		{Name: "input", Kind: operation.KindJSON, Summary: `workflow input as JSON, for example {"objective":"..."}`, Schema: json.RawMessage(`{"type":"object","properties":{"objective":{"type":"string"}},"additionalProperties":false}`)},
		{Name: "overlap", Kind: operation.KindEnum, Summary: "overlap policy", Default: json.RawMessage(`"skip"`), Enum: []operation.EnumValue{{Value: OverlapSkip}, {Value: OverlapQueueOne}}},
		{Name: "grace", Kind: operation.KindDuration, Summary: "catch-up grace (default from config)"},
	}
}

func Catalog() operation.Catalog {
	return operation.Catalog{
		Group: operation.Group{Name: GroupName, Summary: "Manage scheduled recurring jobs"},
		Descriptors: []operation.Descriptor{
			{
				ID: operationAdd, Summary: "Create a scheduled job and start its run", Exposure: operation.ExposureAgent,
				Risk: operation.RiskTrustGrant, Commit: operation.CommitTransactional, Sink: true, Preview: true,
				Description: "Stores a prompt or workflow start that later runs unattended with the owner's authority. The owner picks the destination of its output; a call from the agent always uses the owner destination.",
				Params:      addParams(), Needs: []string{"durable"},
				Output: operation.Output{Kind: operation.OutputRecord, Fields: []operation.OutputField{{Name: "id"}}},
			},
			{
				ID: operationDel, Summary: "Soft-delete a job and retain its history", Exposure: operation.ExposureAgent,
				Risk: operation.RiskChange, Commit: operation.CommitTransactional,
				Params: []operation.Param{jobIDParam()}, Output: stateOutput(),
			},
			{
				ID: operationList, Summary: "List jobs with their next fire instant", Exposure: operation.ExposureAgent,
				Risk: operation.RiskRead, Commit: operation.CommitNone,
				Output: operation.Output{Kind: operation.OutputTable, Fields: []operation.OutputField{
					{Name: "id"}, {Name: "name"}, {Name: "schedule"}, {Name: "timezone"}, {Name: "state"}, {Name: "next_fire"},
				}},
			},
			{
				ID: operationPause, Summary: "Pause a job after its in-flight turn settles", Exposure: operation.ExposureAgent,
				Risk: operation.RiskChange, Commit: operation.CommitTransactional,
				Params: []operation.Param{jobIDParam()}, Output: stateOutput(),
			},
			{
				ID: operationResum, Summary: "Resume a paused job and restart its run", Exposure: operation.ExposureAgent,
				Risk: operation.RiskChange, Commit: operation.CommitTransactional, Needs: []string{"durable"},
				Params: []operation.Param{jobIDParam()}, Output: stateOutput(),
			},
			{
				ID: operationNow, Summary: "Record a manual fire and start the job run", Exposure: operation.ExposureAgent,
				Risk: operation.RiskChange, Commit: operation.CommitTransactional, Needs: []string{"durable"},
				Params: []operation.Param{jobIDParam()},
				Output: operation.Output{Kind: operation.OutputRecord, Fields: []operation.OutputField{{Name: "id"}, {Name: "occurrence"}}},
			},
			{
				ID: operationRuns, Summary: "List occurrence history for a job", Exposure: operation.ExposureAgent,
				Risk: operation.RiskRead, Commit: operation.CommitNone,
				Params: []operation.Param{
					jobIDParam(),
					{Name: "limit", Kind: operation.KindInteger, Summary: "maximum rows to show", Minimum: ptr(1), Maximum: ptr(maxRunsPerListed), Default: json.RawMessage(defaultRunsLimit)},
				},
				Output: operation.Output{Kind: operation.OutputTable, Fields: []operation.OutputField{
					{Name: "id"}, {Name: "scheduled_for"}, {Name: "state"}, {Name: "turn"},
				}},
			},
		},
	}
}

func ptr(value int64) *int64 {
	return &value
}
