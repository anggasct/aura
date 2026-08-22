//go:build !linux

package filesystem

import (
	"errors"

	"github.com/anggasct/aura/internal/toolbroker"
)

func New(options Options) (map[string]toolbroker.Adapter, error) {
	if err := validateOptions(&options); err != nil {
		return nil, err
	}
	return nil, errors.New("filesystem: race-safe workspace access requires Linux")
}
