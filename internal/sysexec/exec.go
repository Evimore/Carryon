package sysexec

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"time"
)

// DefaultTimeout is how long New lets Output wait for a program, like the prototype's sh.
const DefaultTimeout = 10 * time.Minute

// stderrKeep caps how much of a program's standard error Output holds in memory.
const stderrKeep = 64 << 10

// Exec runs real programs. The zero value works: it uses the process's terminal and sets no
// timeout.
type Exec struct {
	// Timeout limits how long Output waits for a program. Zero means no limit beyond ctx's. Run has
	// no timeout of its own: it waits for the user.
	Timeout time.Duration
	// The terminal Run attaches programs to, and where it echoes commands. Nil means the process's
	// own stdin, stdout and stderr.
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
}

// New returns a runner for real programs with DefaultTimeout.
func New() *Exec {
	return &Exec{Timeout: DefaultTimeout}
}

// Output runs name with args, with no input, and returns its standard output. When the program
// fails, Output still returns what it printed, along with an *ExitError.
func (e *Exec) Output(ctx context.Context, name string, args ...string) ([]byte, error) {
	if e.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, e.Timeout)
		defer cancel()
	}
	var stdout bytes.Buffer
	stderr := &tailBuffer{max: stderrKeep}
	cmd := e.command(ctx, name, args)
	cmd.Stdout = &stdout
	cmd.Stderr = stderr
	err := cmd.Run()
	return stdout.Bytes(), e.check(ctx, cmd, err, stderr.String())
}

// Run runs name with args attached to the terminal, after echoing "$ command" to stderr, like
// the prototype's call. It's for programs that may ask the user something, such as pacman.
func (e *Exec) Run(ctx context.Context, name string, args ...string) error {
	stdin, stdout, stderr := e.Stdin, e.Stdout, e.Stderr
	if stdin == nil {
		stdin = os.Stdin
	}
	if stdout == nil {
		stdout = os.Stdout
	}
	if stderr == nil {
		stderr = os.Stderr
	}
	fmt.Fprintf(stderr, "$ %s\n", Join(append([]string{name}, args...)))
	cmd := e.command(ctx, name, args)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = stdin, stdout, stderr
	return e.check(ctx, cmd, cmd.Run(), "")
}

// Exists reports whether name is a program on PATH (or, if it holds a slash, an executable file).
func (e *Exec) Exists(name string) bool {
	_, err := exec.LookPath(name)
	return err == nil
}

// AsUser returns a runner that runs commands as u. See the package-level AsUser.
func (e *Exec) AsUser(u User) Runner {
	return AsUser(e, u, os.Geteuid() == 0, os.Getenv("PATH"))
}

func (e *Exec) command(ctx context.Context, name string, args []string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, name, args...)
	// A killed program's children may keep its pipes open; don't wait for them forever.
	cmd.WaitDelay = 5 * time.Second
	return cmd
}

// check turns the result of cmd.Run into Carryon's errors.
func (e *Exec) check(ctx context.Context, cmd *exec.Cmd, err error, stderr string) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, exec.ErrNotFound) {
		return notFound(cmd.Args[0])
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		if errors.Is(ctxErr, context.DeadlineExceeded) {
			return fmt.Errorf("%s took too long and was stopped: %w", Join(cmd.Args), ctxErr)
		}
		return fmt.Errorf("%s was stopped: %w", Join(cmd.Args), ctxErr)
	}
	var xe *exec.ExitError
	if errors.As(err, &xe) {
		return &ExitError{
			Args:   cmd.Args,
			Code:   xe.ExitCode(),
			Status: xe.ProcessState.String(),
			Stderr: tail(stderr, stderrLines),
		}
	}
	return fmt.Errorf("running %s: %w", Join(cmd.Args), err)
}

// tailBuffer keeps the last max bytes written to it.
type tailBuffer struct {
	max int
	buf []byte
}

func (b *tailBuffer) Write(p []byte) (int, error) {
	n := len(p)
	if len(p) >= b.max {
		b.buf = append(b.buf[:0], p[len(p)-b.max:]...)
		return n, nil
	}
	if over := len(b.buf) + len(p) - b.max; over > 0 {
		b.buf = append(b.buf[:0], b.buf[over:]...)
	}
	b.buf = append(b.buf, p...)
	return n, nil
}

func (b *tailBuffer) String() string { return string(b.buf) }
