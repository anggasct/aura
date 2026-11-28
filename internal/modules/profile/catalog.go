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
	maxExpiresBytes  = 40
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

func fieldsOutput(kind operation.OutputKind, names ...string) operation.Output {
	fields := make([]operation.OutputField, 0, len(names))
	for _, name := range names {
		fields = append(fields, operation.OutputField{Name: name})
	}
	return operation.Output{Kind: kind, Fields: fields}
}

func setParams() []operation.Param {
	return []operation.Param{
		{Name: "category", Kind: operation.KindEnum, Summary: "fact category", Required: true, Enum: categoryEnum()},
		{
			Name: "key", Kind: operation.KindString, Summary: "fact key", Required: true, MinLength: 1, MaxLength: maxKeyChars,
			Bounds: []operation.SurfaceLength{{Surface: operation.SurfaceAgent, MaxLength: AgentKeyChars}},
		},
		{
			Name: "value", Kind: operation.KindString, Summary: "fact value", Required: true, MinLength: 1, MaxLength: maxValueChars,
			Bounds: []operation.SurfaceLength{{Surface: operation.SurfaceAgent, MaxLength: AgentValueChars}},
		},
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
				Risk: operation.RiskChange, Commit: operation.CommitTransactional, UntrustedOutput: true, Needs: needs,
				Description: "Marks a candidate or learned fact owner-verified, which puts it into every owner turn, and prints the whole value it verified. A fact that another verified fact already holds the slot of is refused. Only the owner accepts; the agent has no accept.",
				Params:      []operation.Param{factIDParam()},
				Output:      fieldsOutput(operation.OutputRecord, "accepted", "category", "key", "value", "origin"),
			},
			{
				ID: operationDelete, Summary: "Preview or delete a profile fact", Exposure: operation.ExposureAgent,
				Risk: operation.RiskDestructive, Commit: operation.CommitTransactional, Preview: true, Needs: needs,
				Description: "Deletes a profile fact so it is never injected again and keeps a tombstone of the deletion. The preview shows the whole fact, key and value, and the delete removes exactly that fact; a fact too long to show whole is deleted from the CLI or chat command instead. A fact that is already deleted needs nothing.",
				Params:      []operation.Param{factIDParam()}, Output: textOutput(),
			},
			{
				ID: operationList, Summary: "List profile facts", Exposure: operation.ExposureAgent,
				Risk: operation.RiskRead, Commit: operation.CommitNone, UntrustedOutput: true, Needs: needs,
				Description: "Lists the learned and owner-set facts with status, confidence, origin and the whole value. The values come from conversations, not from the owner alone.",
				Params: []operation.Param{
					{Name: "category", Kind: operation.KindEnum, Summary: "only facts of this category", Enum: categoryEnum()},
					{Name: "status", Kind: operation.KindEnum, Summary: "only facts in this status", Enum: statusEnum()},
				},
				Output: fieldsOutput(operation.OutputTable, "id", "category", "key", "value", "status", "confidence", "origin", "verified", "conflicts_with"),
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
				Description: "Lists candidates and unverified active facts that wait for the owner, the ones that conflict with a verified fact first, with origin and the whole value. The values come from conversations, not from the owner alone.",
				Output:      fieldsOutput(operation.OutputTable, "state", "id", "category", "key", "value", "origin", "confidence", "conflicts_with"),
			},
			{
				ID: operationSet, Summary: "Set a profile fact", Exposure: operation.ExposureAgent,
				Risk: operation.RiskChange, Commit: operation.CommitTransactional, Needs: needs,
				Description: "Stores an owner-verified fact, which is injected into every owner turn. A value that an agent proposed or that was rejected becomes owner-verified at once.",
				Texts: []operation.SurfaceText{
					{Surface: operation.SurfaceAgent, Text: "Records a fact with origin agent. It is not verified and is not injected into owner turns: it waits in the owner's review with the learned facts until the owner accepts it. You cannot verify a fact yourself. The key takes up to 32 and the value up to 200 printable characters on one line, with no control, direction or prompt-marker characters, so the owner can read the whole fact on the review screen."},
				},
				Params: setParams(), Output: textOutput(),
			},
			{
				ID: operationShow, Summary: "Show one profile fact with its provenance", Exposure: operation.ExposureAgent,
				Risk: operation.RiskRead, Commit: operation.CommitNone, UntrustedOutput: true, Needs: needs,
				Description: "Shows a fact with its status, origin, conflicts, the whole value and the evidence it was learned from. The value comes from conversations, not from the owner alone.",
				Params:      []operation.Param{factIDParam()},
				Output: fieldsOutput(operation.OutputRecord, "id", "category", "key", "value", "status", "origin", "verified", "confidence",
					"created", "conflicts_with", "expires_at", "evidence"),
			},
		},
	}
}
