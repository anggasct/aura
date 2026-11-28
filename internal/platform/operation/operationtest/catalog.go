package operationtest

import (
	"github.com/anggasct/aura/internal/platform/operation"
)

const (
	Group       = "stub"
	Read        = "stub.read"
	Change      = "stub.change"
	Notice      = "stub.notice"
	Fresh       = "stub.fresh"
	Grant       = "stub.grant"
	Delete      = "stub.delete"
	External    = "stub.external"
	Idempotent  = "stub.idempotent"
	Host        = "stub.host"
	OwnerOnly   = "stub.owner-only"
	DefaultNote = "stub operation"
)

func ToolOf(id string) string {
	return operation.ToolName(id)
}

func subjectParam() operation.Param {
	return operation.Param{Name: "subject", Kind: operation.KindString, Summary: "what the operation acts on", Required: true, Position: 1, MinLength: 1, MaxLength: 64}
}

func output() operation.Output {
	return operation.Output{Kind: operation.OutputRecord, Fields: []operation.OutputField{{Name: "done"}, {Name: "operation"}}}
}

func Catalog() operation.Catalog {
	descriptor := func(id string, exposure operation.Exposure, risk operation.Risk, commit operation.CommitClass) operation.Descriptor {
		return operation.Descriptor{
			ID: id, Summary: DefaultNote, Exposure: exposure, Risk: risk, Commit: commit,
			Params: []operation.Param{subjectParam()}, Output: output(),
		}
	}
	read := descriptor(Read, operation.ExposureAgent, operation.RiskRead, operation.CommitNone)
	change := descriptor(Change, operation.ExposureAgent, operation.RiskChange, operation.CommitTransactional)
	notice := descriptor(Notice, operation.ExposureAgent, operation.RiskChange, operation.CommitExternalIdempotent)
	notice.OwnerNotice = true
	fresh := descriptor(Fresh, operation.ExposureAgent, operation.RiskChange, operation.CommitTransactional)
	fresh.FreshApproval = true
	grant := descriptor(Grant, operation.ExposureAgent, operation.RiskTrustGrant, operation.CommitTransactional)
	grant.Preview = true
	grant.Confirmation = operation.ConfirmRunDirectly
	deletion := descriptor(Delete, operation.ExposureAgent, operation.RiskDestructive, operation.CommitTransactional)
	deletion.Preview = true
	external := descriptor(External, operation.ExposureAgent, operation.RiskChange, operation.CommitExternal)
	idempotent := descriptor(Idempotent, operation.ExposureAgent, operation.RiskChange, operation.CommitExternalIdempotent)
	host := descriptor(Host, operation.ExposureHost, operation.RiskChange, operation.CommitNone)
	ownerOnly := descriptor(OwnerOnly, operation.ExposureOwner, operation.RiskRead, operation.CommitNone)
	return operation.Catalog{
		Group: operation.Group{Name: Group, Summary: "stub operations for tests"},
		Descriptors: []operation.Descriptor{
			read, change, notice, fresh, grant, deletion, external, idempotent, host, ownerOnly,
		},
	}
}
