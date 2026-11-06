//go:build unix

package knowledge

import (
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestLoadFileRefusesNamedPipeWithoutBlocking(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pipe.md")
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Fatalf("mkfifo: %v", err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := loadFile(path)
		done <- err
	}()
	select {
	case err := <-done:
		requireCode(t, err, ErrorCodeSourceNotRegular)
	case <-time.After(20 * time.Second):
		t.Fatal("opening a named pipe blocked")
	}
}
