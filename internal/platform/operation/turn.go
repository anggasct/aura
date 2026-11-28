package operation

import "github.com/anggasct/aura/internal/platform/turn"

type TurnKind string

const (
	TurnAttended   TurnKind = "attended"
	TurnUnattended TurnKind = "unattended"
)

func (k TurnKind) valid() bool {
	return k == TurnAttended || k == TurnUnattended
}

func TurnKindOf(origin turn.Origin, terminalHasApprover bool) TurnKind {
	if origin.Unattended() || (origin == turn.OriginTerminal && !terminalHasApprover) {
		return TurnUnattended
	}
	return TurnAttended
}

type Turn struct {
	Origin              turn.Origin
	Kind                TurnKind
	Tainted             bool
	ContentTaintTracked bool
}
