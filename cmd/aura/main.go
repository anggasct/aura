package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/anggasct/aura/internal/cli"
	"github.com/anggasct/aura/internal/platform/sandbox"
	"github.com/anggasct/aura/internal/platform/server"
)

func main() {
	if sandbox.IsChild(os.Args) {
		os.Exit(sandbox.RunChild())
		return
	}
	ctx, cancel := context.WithCancelCause(context.Background())
	terms := make(chan os.Signal, 1)
	signal.Notify(terms, syscall.SIGTERM)
	go func() {
		if sig, ok := <-terms; ok {
			cancel(&server.SignalCause{Signal: sig})
		}
	}()
	interrupts := make(chan os.Signal, 2)
	signal.Notify(interrupts, os.Interrupt)
	code := cli.ExecuteContext(ctx, interrupts)
	signal.Stop(terms)
	signal.Stop(interrupts)
	cancel(nil)
	os.Exit(code)
}
