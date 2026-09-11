// Package cli is seer's command line: flag parsing, progress output, JSONL
// output and exit codes. It is the only package that knows every other one.
package cli

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/spf13/cobra"
)

// Exit codes, as promised to scripts.
const (
	ExitOK                 = 0 // the Run completed, even if some Captures failed
	ExitUsage              = 1 // usage or input error
	ExitBrowserUnavailable = 2 // no browser could be provisioned
	ExitAborted            = 3 // the Run was stopped before every Target had a Capture
)

// exitError carries the exit code a verb wants alongside its message.
type exitError struct {
	code int
	err  error
}

func (e *exitError) Error() string { return e.err.Error() }
func (e *exitError) Unwrap() error { return e.err }

func exitWith(code int, err error) error {
	return &exitError{code: code, err: err}
}

// Run executes seer with the given arguments and streams; main is a one-liner
// over it. It returns the process exit code.
func Run(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	root := &cobra.Command{
		Use:           "seer",
		Short:         "Visit a set of Targets once and keep a Screenshot and Response for each",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.SetIn(stdin)
	root.SetOut(stdout)
	root.SetErr(stderr)
	root.SetArgs(args)
	root.AddCommand(newCaptureCmd())

	err := root.ExecuteContext(ctx)
	if err == nil {
		return ExitOK
	}

	fmt.Fprintf(stderr, "seer: %v\n", err)
	var ee *exitError
	if errors.As(err, &ee) {
		return ee.code
	}
	// Anything else is cobra reporting a usage problem.
	fmt.Fprintf(stderr, "Run 'seer --help' for usage.\n")
	return ExitUsage
}
