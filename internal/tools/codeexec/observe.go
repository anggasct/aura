package codeexec

import (
	"context"
	"encoding/json"
	"time"
)

type Phase string

const (
	PhaseBuild Phase = "build"
	PhaseRun   Phase = "run"
)

type Usage struct {
	Duration time.Duration
	CPUTime  time.Duration
}

type Observation struct {
	Runtime       string
	Class         ResultClass
	Phase         Phase
	Compiled      bool
	Build         Usage
	Run           Usage
	StdoutBytes   int64
	StderrBytes   int64
	ArtifactBytes int64
	Imported      int
	Refused       int
}

type Observer func(ctx context.Context, observation *Observation)

func newObservation(outcome *Outcome, compiled bool) *Observation {
	observation := &Observation{
		Runtime: outcome.Runtime, Class: outcome.Class, Phase: outcome.Phase, Compiled: compiled,
		Build: outcome.Build, Run: outcome.Run,
		StdoutBytes: int64(len(outcome.Stdout)), StderrBytes: int64(len(outcome.Stderr)),
	}
	for i := range outcome.Artifacts {
		if outcome.Artifacts[i].Refused != "" {
			observation.Refused++
			continue
		}
		observation.Imported++
		observation.ArtifactBytes += outcome.Artifacts[i].Size
	}
	return observation
}

type auditArtifact struct {
	ArtifactID string `json:"artifact_id"`
	Digest     string `json:"digest"`
	Size       int64  `json:"size"`
}

type auditRecord struct {
	RequestDigest    string          `json:"request_digest"`
	Runtime          string          `json:"runtime"`
	RuntimeVersion   string          `json:"runtime_version"`
	ResultClass      ResultClass     `json:"result_class"`
	Phase            Phase           `json:"phase"`
	ExitCode         int             `json:"exit_code"`
	Signal           int             `json:"signal"`
	DurationMS       int64           `json:"duration_ms"`
	ArtifactBytes    int64           `json:"artifact_bytes"`
	ArtifactsRefused int             `json:"artifacts_refused"`
	Artifacts        []auditArtifact `json:"artifacts"`
}

func newAudit(requestDigest string, outcome *Outcome) (json.RawMessage, error) {
	observation := newObservation(outcome, false)
	record := auditRecord{
		RequestDigest: requestDigest, Runtime: outcome.Runtime, RuntimeVersion: outcome.RuntimeVersion,
		ResultClass: outcome.Class, Phase: outcome.Phase, ExitCode: outcome.ExitCode, Signal: outcome.Signal,
		DurationMS: outcome.Duration.Milliseconds(), ArtifactBytes: observation.ArtifactBytes,
		ArtifactsRefused: observation.Refused, Artifacts: []auditArtifact{},
	}
	for i := range outcome.Artifacts {
		entry := &outcome.Artifacts[i]
		if entry.Refused == "" {
			record.Artifacts = append(record.Artifacts, auditArtifact{ArtifactID: entry.ArtifactID, Digest: entry.Digest, Size: entry.Size})
		}
	}
	encoded, err := json.Marshal(record)
	if err != nil {
		return nil, wrapf(ErrorCodeRunFailed, err, "encode the audit record")
	}
	return encoded, nil
}
