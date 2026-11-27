package terminal

import (
	"context"
	"os"
)

func ForwardInterrupts(ctx context.Context, signals <-chan os.Signal) (events <-chan struct{}, stop func()) {
	if signals == nil {
		return nil, func() {}
	}
	forwardCtx, cancel := context.WithCancel(ctx)
	out := make(chan struct{}, 2)
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer close(out)
		for {
			select {
			case <-forwardCtx.Done():
				return
			case _, ok := <-signals:
				if !ok {
					return
				}
				select {
				case out <- struct{}{}:
				case <-forwardCtx.Done():
					return
				}
			}
		}
	}()
	stop = func() {
		cancel()
		<-done
	}
	return out, stop
}
