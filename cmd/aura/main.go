package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/anggasct/aura/internal/cli"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := cli.ExecuteContext(ctx)
	stop()
	os.Exit(code)
}
