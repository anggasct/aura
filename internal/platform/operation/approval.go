package operation

import (
	"github.com/anggasct/aura/internal/platform/toolbroker"
	"github.com/anggasct/aura/internal/platform/turn"
)

type Approval string

const (
	ApprovalNone           Approval = "none"
	ApprovalUntrustedInput Approval = "untrusted_input"
	ApprovalAsks           Approval = "asks"
	ApprovalAlways         Approval = "always"
	ApprovalRefusedHere    Approval = "refused_here"
	ApprovalOwner          Approval = "owner"
	ApprovalHost           Approval = "host"
)

const (
	markerAlways = "^"
	markerAsks   = "?"
)

func (a Approval) marker() string {
	switch a {
	case ApprovalAlways:
		return markerAlways
	case ApprovalAsks:
		return markerAsks
	case ApprovalNone, ApprovalUntrustedInput, ApprovalRefusedHere, ApprovalOwner, ApprovalHost:
		return ""
	}
	return ""
}

func (e *entry) approval(view *Turn) Approval {
	descriptor := e.descriptor
	switch {
	case !descriptor.Projects(SurfaceAgent) && descriptor.Projects(SurfaceChat):
		return ApprovalOwner
	case !descriptor.Projects(SurfaceAgent):
		return ApprovalHost
	case descriptor.ModuleTool != "":
		if descriptor.Sink {
			return ApprovalUntrustedInput
		}
		return ApprovalNone
	case e.definition == nil || view == nil || !view.Kind.valid():
		return ApprovalRefusedHere
	}
	disposition, err := toolbroker.OperationDisposition(e.definition, &toolbroker.OperationTurn{
		Origin:              view.Origin,
		Unattended:          view.Kind == TurnUnattended,
		Tainted:             view.Tainted,
		ContentTaintTracked: view.ContentTaintTracked,
	})
	if err != nil {
		return ApprovalRefusedHere
	}
	plainChange := descriptor.Risk == RiskChange && !descriptor.OwnerNotice
	switch disposition {
	case toolbroker.DispositionAlways:
		return ApprovalAlways
	case toolbroker.DispositionAsks:
		if view.ContentTaintTracked {
			return ApprovalUntrustedInput
		}
		return ApprovalAsks
	case toolbroker.DispositionRuns:
		if view.ContentTaintTracked && plainChange {
			return ApprovalUntrustedInput
		}
		return ApprovalNone
	case toolbroker.DispositionRefused:
		return ApprovalRefusedHere
	}
	return ApprovalRefusedHere
}

func markerTurn(tracked bool) *Turn {
	return &Turn{Origin: turn.OriginTerminal, Kind: TurnAttended, ContentTaintTracked: tracked}
}
