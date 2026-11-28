package profile

import (
	"github.com/anggasct/aura/internal/platform/operation"
)

const (
	GroupName = "profile"

	operationAccept = "profile.accept"
	operationDelete = "profile.delete"
	operationList   = "profile.list"
	operationReject = "profile.reject"
	operationReview = "profile.review"
	operationSet    = "profile.set"
	operationShow   = "profile.show"

	maxFactIDBytes   = 128
	maxExpiresBytes  = 64
	reviewedFactsCap = 1000
)

func factIDParam() operation.Param {
	return operation.Param{Name: "id", Kind: operation.KindString, Summary: "fact id", Required: true, Position: 1, MinLength: 1, MaxLength: maxFactIDBytes}
}

func categoryEnum() []operation.EnumValue {
	values := make([]operation.EnumValue, 0, len(Categories))
	for _, category := range Categories {
		values = append(values, operation.EnumValue{Value: category})
	}
	return values
}

func statusEnum() []operation.EnumValue {
	statuses := []string{StatusCandidate, StatusActive, StatusRejected, StatusExpired, StatusDeleted}
	values := make([]operation.EnumValue, 0, len(statuses))
	for _, status := range statuses {
		values = append(values, operation.EnumValue{Value: status})
	}
	return values
}

func textOutput() operation.Output {
	return operation.Output{Kind: operation.OutputText}
}

func setParams() []operation.Param {
	return []operation.Param{
		{Name: "category", Kind: operation.KindEnum, Summary: "fact category", Required: true, Enum: categoryEnum()},
		{Name: "key", Kind: operation.KindString, Summary: "fact key", Required: true, MinLength: 1, MaxLength: maxKeyChars},
		{Name: "value", Kind: operation.KindString, Summary: "fact value", Required: true, MinLength: 1, MaxLength: maxValueChars},
		{Name: "expires_at", Kind: operation.KindString, Summary: "RFC3339 expiry in the future", MinLength: 1, MaxLength: maxExpiresBytes},
	}
}

func Catalog() operation.Catalog {
	needs := []string{"profile"}
	return operation.Catalog{
		Group: operation.Group{Name: GroupName, Summary: "Inspect and manage the learned owner profile"},
		Descriptors: []operation.Descriptor{
			{
				ID: operationAccept, Summary: "Accept a learned fact as owner-verified", Exposure: operation.ExposureOwner,
				Risk: operation.RiskChange, Commit: operation.CommitTransactional, Needs: needs,
				Description: "Marks a candidate or learned fact owner-verified, which puts it into every owner turn. A fact that another verified fact already holds the slot of is refused. Only the owner accepts; the agent has no accept.",
				Params:      []operation.Param{factIDParam()}, Output: textOutput(),
			},
			{
				ID: operationDelete, Summary: "Preview or delete a profile fact", Exposure: operation.ExposureAgent,
				Risk: operation.RiskDestructive, Commit: operation.CommitTransactional, Preview: true, Needs: needs,
				Description: "Deletes a profile fact so it is never injected again and keeps a tombstone of the deletion. The preview names the fact and the delete removes exactly that fact.",
				Params:      []operation.Param{factIDParam()}, Output: textOutput(),
			},
			{
				ID: operationList, Summary: "List profile facts", Exposure: operation.ExposureAgent,
				Risk: operation.RiskRead, Commit: operation.CommitNone, UntrustedOutput: true, Needs: needs,
				Description: "Lists the learned and owner-set facts with status, confidence and origin. The values come from conversations, not from the owner alone.",
				Params: []operation.Param{
					{Name: "category", Kind: operation.KindEnum, Summary: "only facts of this category", Enum: categoryEnum()},
					{Name: "status", Kind: operation.KindEnum, Summary: "only facts in this status", Enum: statusEnum()},
				},
				Output: textOutput(),
			},
			{
				ID: operationReject, Summary: "Reject a profile fact", Exposure: operation.ExposureAgent,
				Risk: operation.RiskChange, Commit: operation.CommitTransactional, Needs: needs,
				Description: "Rejects a fact so it is no longer injected into owner turns.",
				Params:      []operation.Param{factIDParam()}, Output: textOutput(),
			},
			{
				ID: operationReview, Summary: "List facts that need review, conflicts first", Exposure: operation.ExposureAgent,
				Risk: operation.RiskRead, Commit: operation.CommitNone, UntrustedOutput: true, Needs: needs,
				Description: "Lists candidates and unverified active facts that wait for the owner, the ones that conflict with a verified fact first. The values come from conversations, not from the owner alone.",
				Output:      textOutput(),
			},
			{
				ID: operationSet, Summary: "Set a profile fact", Exposure: operation.ExposureAgent,
				Risk: operation.RiskChange, Commit: operation.CommitTransactional, Needs: needs,
				Description: "Stores an owner-verified fact, which is injected into every owner turn.",
				Texts: []operation.SurfaceText{
					{Surface: operation.SurfaceAgent, Text: "Records a fact with origin agent. It is not verified and is not injected into owner turns: it waits in the owner's review with the learned facts until the owner accepts it. You cannot verify a fact yourself."},
				},
				Params: setParams(), Output: textOutput(),
			},
			{
				ID: operationShow, Summary: "Show one profile fact with its provenance", Exposure: operation.ExposureAgent,
				Risk: operation.RiskRead, Commit: operation.CommitNone, UntrustedOutput: true, Needs: needs,
				Description: "Shows a fact with its status, origin, conflicts and the evidence it was learned from. The value comes from conversations, not from the owner alone.",
				Params:      []operation.Param{factIDParam()}, Output: textOutput(),
			},
		},
	}
}
