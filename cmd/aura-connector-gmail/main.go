package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/anggasct/aura/internal/connectors/gmail"
	"github.com/anggasct/aura/internal/connectors/wire"
)

func main() {
	os.Exit(run())
}

func run() int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := wire.Serve(ctx, os.Stdin, os.Stdout, gmail.New()); err != nil {
		return 1
	}
	return 0
}
