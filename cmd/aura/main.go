package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/anggasct/aura/internal/cli"
	"github.com/anggasct/aura/internal/sandbox"
)

func main() {
	if sandbox.IsChild(os.Args) {
		os.Exit(sandbox.RunChild())
		return
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM)
	interrupts := make(chan os.Signal, 2)
	signal.Notify(interrupts, os.Interrupt)
	ctx = cli.WithInterrupts(ctx, interrupts)
	code := cli.ExecuteContext(ctx)
	stop()
	signal.Stop(interrupts)
	os.Exit(code)
}
