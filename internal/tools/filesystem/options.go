package filesystem

import (
	"errors"
	"path/filepath"
)

type Options struct {
	Workspace     string
	MaxFileBytes  int64
	MaxDirEntries int
}

func validateOptions(options *Options) error {
	if !filepath.IsAbs(options.Workspace) {
		return errors.New("filesystem: workspace must be an absolute path")
	}
	if options.MaxFileBytes <= 0 {
		options.MaxFileBytes = 8 << 20
	}
	if options.MaxDirEntries <= 0 {
		options.MaxDirEntries = 10000
	}
	return nil
}
