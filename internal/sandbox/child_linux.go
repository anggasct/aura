//go:build linux

package sandbox

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

const childInitFailedExit = 126

// childConfig is the contract the parent streams to a re-executed child over
// an inherited pipe. The child applies the limits and confinement, then execs
// Command with Args under the allowlisted environment.
type childConfig struct {
	WorkingDir     string   `json:"working_dir"`
	ReadOnlyPaths  []string `json:"read_only_paths"`
	ReadWritePaths []string `json:"read_write_paths"`
	AllowEnv       []string `json:"allow_env"`
	Limits         Limits   `json:"limits"`
	Command        string   `json:"command"`
	Args           []string `json:"args"`
}

// RunChild is the entry point for a sandbox child re-execution. It reads the
// streamed config, applies the resource and confinement layers in order, and
// execs the target. It returns only when setup fails (exit 126, with the
// cause written to the init-error pipe); a successful setup ends in execve
// and never returns.
func RunChild() int {
	cfg, err := readChildConfig()
	if err != nil {
		reportChildInit(err)
		return childInitFailedExit
	}
	if err := setupChild(&cfg); err != nil {
		reportChildInit(err)
		return childInitFailedExit
	}
	_ = unix.Close(4) // init-error pipe: silence EOF signals a clean setup
	if err := unix.Exec(cfg.Command, append([]string{cfg.Command}, cfg.Args...), cfg.AllowEnv); err != nil {
		reportChildInit(fmt.Errorf("exec %s: %w", cfg.Command, err))
		return childInitFailedExit
	}
	return 0
}

func readChildConfig() (childConfig, error) {
	var cfg childConfig
	f := os.NewFile(3, "config")
	if f == nil {
		return childConfig{}, errors.New("config pipe unavailable")
	}
	defer func() { _ = f.Close() }()
	if err := json.NewDecoder(f).Decode(&cfg); err != nil {
		return childConfig{}, fmt.Errorf("read config: %w", err)
	}
	return cfg, nil
}

// setupChild applies confinement in the order the kernel requires: close
// stray inherited descriptors first, then rlimits, then no_new_privs (needed
// by both Landlock restrict_self and seccomp), then Landlock, then the seccomp
// filter immediately before exec.
func setupChild(cfg *childConfig) error {
	if err := closeExtraFds(); err != nil {
		return err
	}
	if err := applyRlimits(cfg.Limits); err != nil {
		return err
	}
	if err := unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); err != nil {
		return fmt.Errorf("set no_new_privs: %w", err)
	}
	spec := &Spec{WorkingDir: cfg.WorkingDir, ReadOnlyPaths: cfg.ReadOnlyPaths, ReadWritePaths: cfg.ReadWritePaths}
	if err := applyLandlock(spec); err != nil {
		return err
	}
	return applySeccomp()
}

func reportChildInit(err error) {
	if f := os.NewFile(4, "init-err"); f != nil {
		_, _ = f.WriteString(err.Error())
		_ = f.Close()
	}
}

// closeExtraFds closes every inherited descriptor above the config (3) and
// init-error (4) pipes. A parent fd without close-on-exec would otherwise
// survive execve into the confined process; close_range atomically clears the
// range so a leaked descriptor carrying secret data cannot reach the child.
// Any failure — including a kernel without close_range — fails closed here so
// the child never execs with an unclean descriptor set.
func closeExtraFds() error {
	if err := unix.CloseRange(5, ^uint(0), 0); err != nil {
		return fmt.Errorf("close extra fds: %w", err)
	}
	return nil
}
