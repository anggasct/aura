package cli

import (
	"context"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/anggasct/aura/internal/app"
	"github.com/anggasct/aura/internal/channels/terminal"
)

func ExecuteContext(ctx context.Context, interrupts <-chan os.Signal, args ...string) int {
	effective := args
	if len(effective) == 0 {
		effective = os.Args[1:]
	}
	root := app.NewRootCommand()
	root.SetArgs(effective)
	ctx = app.WithInterrupts(ctx, interrupts)
	executionCtx, stopInterrupts := contextForCommand(ctx, root, effective, interrupts)
	defer stopInterrupts()
	err := root.ExecuteContext(executionCtx)
	if err != nil {
		fmt.Fprintln(os.Stderr, "aura:", terminal.SanitizeText(err.Error()))
	}
	return app.ExitCode(root, effective, err)
}

func contextForCommand(ctx context.Context, root *cobra.Command, args []string, signals <-chan os.Signal) (executionCtx context.Context, stop func()) {
	command, _, err := root.Find(args)
	if err != nil || command.Name() == "chat" {
		return ctx, func() {}
	}
	if signals == nil {
		return ctx, func() {}
	}
	interruptCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		select {
		case _, ok := <-signals:
			if ok {
				cancel()
			}
		case <-interruptCtx.Done():
		}
	}()
	return interruptCtx, func() {
		cancel()
		<-done
	}
}
