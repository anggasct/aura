package toolcall

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
)

type pendingWait struct {
	done chan error
}

func startWait(ctx context.Context, wait func(context.Context) error) *pendingWait {
	pending := &pendingWait{done: make(chan error, 1)}
	go func() { pending.done <- wait(ctx) }()
	return pending
}

func (p *pendingWait) finished() (bool, error) {
	synctest.Wait()
	select {
	case err := <-p.done:
		return true, err
	default:
		return false, nil
	}
}

func requirePending(t *testing.T, what string, p *pendingWait) {
	t.Helper()
	if finished, err := p.finished(); finished {
		t.Fatalf("%s returned early with %v", what, err)
	}
}

func requireReleased(t *testing.T, what string, p *pendingWait) {
	t.Helper()
	finished, err := p.finished()
	if !finished {
		t.Fatalf("%s is still waiting", what)
	}
	if err != nil {
		t.Fatalf("%s = %v, want nil", what, err)
	}
}

func TestToolCallSlotsOrderTheirPhases(t *testing.T) {
	t.Run("authorization follows call order", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			batch := NewBatch(1, 4)
			wait := startWait(t.Context(), batch.Slot(2).AwaitAuthorization)
			requirePending(t, "authorization of call 2", wait)
			batch.Slot(0).Authorized(false)
			requirePending(t, "authorization of call 2 after call 0", wait)
			batch.Slot(1).Close()
			requireReleased(t, "authorization of call 2", wait)
		})
	})
	t.Run("the first call never waits", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			batch := NewBatch(1, 2)
			requireReleased(t, "authorization of call 0", startWait(t.Context(), batch.Slot(0).AwaitAuthorization))
			batch.Slot(0).Authorized(true)
			requireReleased(t, "execution of call 0", startWait(t.Context(), batch.Slot(0).AwaitExecution))
		})
	})
	t.Run("a call that runs alone executes after every earlier call closed", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			batch := NewBatch(1, 4)
			for i := range 3 {
				batch.Slot(i).Authorized(false)
			}
			batch.Slot(3).Authorized(true)
			wait := startWait(t.Context(), batch.Slot(3).AwaitExecution)
			requirePending(t, "execution of call 3", wait)
			batch.Slot(0).Close()
			batch.Slot(1).Close()
			requirePending(t, "execution of call 3 with call 2 open", wait)
			batch.Slot(2).Close()
			requireReleased(t, "execution of call 3", wait)
		})
	})
	t.Run("a call that does not run alone waits only for earlier calls that do", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			batch := NewBatch(1, 4)
			batch.Slot(0).Authorized(true)
			batch.Slot(1).Authorized(false)
			batch.Slot(2).Authorized(false)
			batch.Slot(3).Authorized(false)
			wait := startWait(t.Context(), batch.Slot(2).AwaitExecution)
			requirePending(t, "execution of call 2", wait)
			batch.Slot(0).Close()
			requireReleased(t, "execution of call 2", wait)
			requireReleased(t, "execution of call 3", startWait(t.Context(), batch.Slot(3).AwaitExecution))
		})
	})
	t.Run("the first answer about running alone stands", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			batch := NewBatch(1, 2)
			batch.Slot(0).Authorized(false)
			batch.Slot(0).Authorized(true)
			batch.Slot(1).Authorized(false)
			requireReleased(t, "execution of call 1", startWait(t.Context(), batch.Slot(1).AwaitExecution))
		})
	})
	t.Run("a cancelled context ends each wait", func(t *testing.T) {
		waits := map[string]func(batch *Batch) func(context.Context) error{
			"authorization": func(batch *Batch) func(context.Context) error {
				return batch.Slot(1).AwaitAuthorization
			},
			"execution": func(batch *Batch) func(context.Context) error {
				batch.Slot(0).Authorized(true)
				batch.Slot(1).Authorized(false)
				return batch.Slot(1).AwaitExecution
			},
		}
		for name, prepare := range waits {
			t.Run(name, func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					ctx, cancel := context.WithCancel(t.Context())
					wait := startWait(ctx, prepare(NewBatch(1, 2)))
					requirePending(t, name+" of call 1", wait)
					cancel()
					if finished, err := wait.finished(); !finished || !errors.Is(err, context.Canceled) {
						t.Errorf("%s wait after cancel = %v (finished %v), want context.Canceled", name, err, finished)
					}
				})
			})
		}
	})
	t.Run("close releases both phases of a call that never reached them", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			batch := NewBatch(1, 3)
			authorization := startWait(t.Context(), batch.Slot(1).AwaitAuthorization)
			requirePending(t, "authorization of call 1", authorization)
			batch.Slot(0).Close()
			requireReleased(t, "authorization of call 1", authorization)
			batch.Slot(1).Authorized(false)
			batch.Slot(2).Authorized(true)
			execution := startWait(t.Context(), batch.Slot(2).AwaitExecution)
			requirePending(t, "execution of call 2", execution)
			batch.Slot(1).Close()
			batch.Slot(1).Close()
			requireReleased(t, "execution of call 2", execution)
		})
	})
	t.Run("key and action name the batch and the call", func(t *testing.T) {
		slot := NewBatch(3, 4).Slot(2)
		if got := slot.Key(); got != "call-3-2" {
			t.Errorf("Key() = %q, want call-3-2", got)
		}
		if got := slot.Action("policy"); got != "call-3-2/policy" {
			t.Errorf("Action(policy) = %q, want call-3-2/policy", got)
		}
	})
}
