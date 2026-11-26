package app

import (
	"github.com/anggasct/aura/internal/platform/sqlite"
	"github.com/anggasct/aura/internal/store/history"
)

func Migrations() []sqlite.Migration {
	return history.Migrations()
}
