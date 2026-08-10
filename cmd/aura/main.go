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
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := cli.ExecuteContext(ctx)
	stop()
	os.Exit(code)
}
