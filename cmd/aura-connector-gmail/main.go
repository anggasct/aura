package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/anggasct/aura/internal/modules/plugin/wire"
	"github.com/anggasct/aura/plugins/gmail/connector"
)

func main() {
	os.Exit(run())
}

func run() int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := wire.Serve(ctx, os.Stdin, os.Stdout, connector.New()); err != nil {
		return 1
	}
	return 0
}
