// Package sysexec runs external programs (pacman, restic, systemctl...) for the rest of Carryon.
//
// Everything that starts a program goes through a Runner, so tests can swap in a Fake and never
// run anything real. Exec is the real runner. AsUser wraps a runner so commands run as the
// backed-up user, with that user's HOME and session bus, when Carryon itself runs as root.
//
// It ports the prototype's sh, call, have and user_cmd (legacy/sysbackup.py).
package sysexec

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

// Runner runs programs.
type Runner interface {
	// Output runs name with args and returns its standard output. If the program fails, the error
	// includes the command, its exit status and the last lines of its standard error.
	Output(ctx context.Context, name string, args ...string) ([]byte, error)
	// Run runs name with args attached to the terminal (stdin, stdout, stderr), echoing the command first.
	Run(ctx context.Context, name string, args ...string) error
	// Exists reports whether a program is on PATH.
	Exists(name string) bool
}

// stderrLines is how many lines of standard error a failed command's error keeps.
const stderrLines = 20

// ExitError is returned when a program ran but failed.
type ExitError struct {
	Args   []string // the command line, program first
	Code   int      // exit code, or -1 if the program was killed by a signal
	Status string   // how it ended, as Go words it: "exit status 1", "signal: killed"
	Stderr string   // the last lines of standard error; empty for Run, whose stderr is the terminal
}

func (e *ExitError) Error() string {
	msg := fmt.Sprintf("%s failed (%s)", Join(e.Args), e.Status)
	if e.Stderr != "" {
		msg += ":\n" + e.Stderr
	}
	return msg
}

// ExitCode is the program's exit code. Packages that declare their own small Runner interface
// (pkgmgr, facts, dest) read it with errors.As and interface{ ExitCode() int }, without importing
// this package.
func (e *ExitError) ExitCode() int { return e.Code }

// ExitCode returns the exit code of a program that failed with err, and whether err came from a
// program that ran and failed (rather than one that couldn't start or was never registered).
func ExitCode(err error) (int, bool) {
	var ee *ExitError
	if errors.As(err, &ee) {
		return ee.Code, true
	}
	return 0, false
}

// notFound is the error for a program that isn't on PATH. It wraps exec.ErrNotFound.
func notFound(name string) error {
	return fmt.Errorf("%s isn't installed (not found on PATH): %w", name, exec.ErrNotFound)
}

// tail returns the last n lines of s, without trailing blank space.
func tail(s string, n int) string {
	s = strings.TrimRight(s, " \t\r\n")
	lines := strings.Split(s, "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

// Join quotes args for a POSIX shell and joins them with spaces, like Python's shlex.join. It is
// how commands are echoed, shown in errors and registered with a Fake.
func Join(args []string) string {
	quoted := make([]string, len(args))
	for i, a := range args {
		quoted[i] = quote(a)
	}
	return strings.Join(quoted, " ")
}

func quote(s string) string {
	if s == "" {
		return "''"
	}
	safe := true
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("@%+=:,./_-", r)) {
			safe = false
			break
		}
	}
	if safe {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'"
}
