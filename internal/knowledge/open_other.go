//go:build !unix

package knowledge

import "os"

func openNoFollow(string) (*os.File, error) {
	return nil, Errorf(ErrorCodeSourceUnsupported, "local files are not supported on this platform")
}

func isSymlinkOpenError(error) bool { return false }
