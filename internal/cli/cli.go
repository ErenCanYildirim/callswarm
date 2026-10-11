// Package cli implements the callswarm command line.
package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"runtime/debug"
	"strings"
)

// Exit codes.
const (
	ExitOK        = 0
	ExitFailure   = 1   // the run could not be carried out
	ExitUsage     = 2   // bad command line
	ExitCancelled = 130 // interrupted, as shells report for Ctrl-C
)

const usage = `callswarm plays the caller against a voice agent: it runs scripted
conversations as many concurrent calls and measures how the agent responds.

Usage:
  callswarm run <scenario.yaml> --url <ws-url> [flags]
  callswarm fake-agent [flags]
  callswarm version

Commands:
  run         run a scenario against an agent and report the results
  fake-agent  serve a key-free echo agent with known timing, for trying
              callswarm out and for checking its measurements
  version     print the version

Run "callswarm <command> -h" for a command's flags.
`

// Run executes the command line in args (without the program name) and
// returns the process exit code. Cancelling ctx stops a run in progress.
func Run(ctx context.Context, args []string, version string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		_, _ = fmt.Fprint(stderr, usage)
		return ExitUsage
	}
	switch args[0] {
	case "run":
		return runCommand(ctx, args[1:], version, stdout, stderr)
	case "fake-agent":
		return fakeAgentCommand(ctx, args[1:], stdout, stderr)
	case "version", "--version", "-version":
		_, _ = fmt.Fprintln(stdout, resolveVersion(version))
		return ExitOK
	case "help", "-h", "--help", "-help":
		_, _ = fmt.Fprint(stdout, usage)
		return ExitOK
	}
	_, _ = fmt.Fprintf(stderr, "callswarm: unknown command %q\n\n%s", args[0], usage)
	return ExitUsage
}

// resolveVersion prefers the version set at build time, then the module
// version recorded by "go install", then "dev".
func resolveVersion(v string) string {
	if v != "" && v != "dev" {
		return v
	}
	if bi, ok := debug.ReadBuildInfo(); ok && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
		return bi.Main.Version
	}
	return "dev"
}

// parseInterleaved parses flags that may appear before or after positional
// arguments, which the standard flag package does not allow on its own.
func parseInterleaved(fs *flag.FlagSet, args []string) ([]string, error) {
	var positional []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		if fs.NArg() == 0 {
			return positional, nil
		}
		positional = append(positional, fs.Arg(0))
		args = fs.Args()[1:]
	}
}

// usageError is a command-line mistake: print it with a hint, exit 2.
type usageError struct{ msg string }

func (e usageError) Error() string { return e.msg }

func usageErrorf(format string, args ...any) error {
	return usageError{fmt.Sprintf(format, args...)}
}

// exitCode reports a command's error and picks its exit code.
func exitCode(err error, command string, stderr io.Writer) int {
	var ue usageError
	switch {
	case err == nil:
		return ExitOK
	case errors.Is(err, flag.ErrHelp):
		return ExitOK
	case errors.As(err, &ue):
		_, _ = fmt.Fprintf(stderr, "callswarm %s: %s\nRun \"callswarm %s -h\" for usage.\n", command, ue.msg, command)
		return ExitUsage
	default:
		_, _ = fmt.Fprintf(stderr, "callswarm %s: %s\n", command, strings.TrimSpace(err.Error()))
		return ExitFailure
	}
}
