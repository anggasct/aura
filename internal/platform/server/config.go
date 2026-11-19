package server

import (
	"fmt"

	"github.com/anggasct/aura/internal/platform/configkit"
)

type Config struct {
	Host string `json:"host"`
	Port int    `json:"port"`
}

func Default() Config {
	return Config{
		Host: "127.0.0.1",
		Port: 8280,
	}
}

func (c Config) Validate() error {
	if c.Port < 1 || c.Port > 65535 {
		return configkit.Invalid("port", fmt.Sprintf("%d is out of range (1-65535)", c.Port))
	}
	return nil
}
