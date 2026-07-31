package main

import (
	"os"

	"github.com/anggasct/aura/internal/cli"
)

func main() {
	os.Exit(cli.Execute())
}
