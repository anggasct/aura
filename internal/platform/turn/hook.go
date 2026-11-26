package turn

import "context"

type TurnHook interface {
	AfterTurn(ctx context.Context, req *TurnRequest, events []Event) error
}
