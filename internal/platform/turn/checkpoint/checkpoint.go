package runtimecheckpoint

import "context"

const EventKindRunCheckpoint = "run.checkpoint.v1"

type ResumeValidation struct {
	SessionID            string
	TurnID               string
	OwnerID              string
	PrincipalID          string
	CapabilityDigest     string
	PolicyVersion        string
	CurrentEventSequence uint64
	CurrentStateDigest   string
	ResumeGeneration     uint64
	ApprovalValid        bool
	EffectStateValid     bool
}

type EffectResumeValidator interface {
	ValidateResumeEffects(context.Context, string, string, []string) error
}
