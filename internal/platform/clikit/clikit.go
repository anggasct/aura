package clikit

import (
	"errors"
	"fmt"
	"io"
	"text/tabwriter"

	"github.com/spf13/cobra"
)

type UsageError struct {
	Err error
}

func NewUsageError(err error) error {
	if err == nil {
		return nil
	}
	return &UsageError{Err: err}
}

func (e *UsageError) Error() string {
	return e.Err.Error()
}

func (e *UsageError) Unwrap() error {
	return e.Err
}

func (e *UsageError) UsageError() bool {
	return true
}

func IsUsageError(err error) bool {
	var target *UsageError
	return errors.As(err, &target)
}

type ExitCodeError struct {
	Code int
	Err  error
}

func NewExitCodeError(code int, err error) error {
	if err == nil {
		return nil
	}
	return &ExitCodeError{Code: code, Err: err}
}

func (e *ExitCodeError) Error() string {
	return e.Err.Error()
}

func (e *ExitCodeError) Unwrap() error {
	return e.Err
}

func (e *ExitCodeError) ExitCode() int {
	return e.Code
}

func ExitCodeOf(err error) (int, bool) {
	var target *ExitCodeError
	if errors.As(err, &target) {
		return target.Code, true
	}
	return 0, false
}

type Table struct {
	w   *tabwriter.Writer
	err error
}

func NewTable(out io.Writer) *Table {
	return &Table{w: tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)}
}

func (t *Table) Printf(format string, args ...any) {
	if t.err != nil {
		return
	}
	_, t.err = fmt.Fprintf(t.w, format, args...)
}

func (t *Table) Flush() error {
	if t.err != nil {
		return t.err
	}
	return t.w.Flush()
}

func NoPositionalArgs(cmd *cobra.Command, args []string) error {
	if len(args) > 0 {
		return NewUsageError(fmt.Errorf("%s: unexpected argument %q", cmd.CommandPath(), args[0]))
	}
	return nil
}

func WriteLines(cmd *cobra.Command, lines []string) error {
	out := cmd.OutOrStdout()
	for _, line := range lines {
		if _, err := fmt.Fprintln(out, line); err != nil {
			return err
		}
	}
	return nil
}
