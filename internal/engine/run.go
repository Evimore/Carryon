package engine

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Runner starts a command, waits for it and returns its exit code. The error is non-nil only when
// the command couldn't be started. A command killed by signal n returns 128+n, as a shell would.
type Runner func(ctx context.Context, c Cmd) (int, error)

// stopGrace is how long restic gets to remove its locks after being asked to stop.
const stopGrace = 30 * time.Second

// ExecRunner runs commands with os/exec. When ctx is cancelled the command gets SIGINT, so restic
// can remove its lock, and is killed if it is still running stopGrace later.
func ExecRunner(ctx context.Context, c Cmd) (int, error) {
	cmd := exec.CommandContext(ctx, c.Path, c.Args...)
	cmd.Env = c.Env
	cmd.Stdin, cmd.Stdout, cmd.Stderr = c.Stdin, c.Stdout, c.Stderr
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGINT) }
	cmd.WaitDelay = stopGrace
	err := cmd.Run()
	return exitCode(cmd, err)
}

// exitCode turns the result of exec.Cmd.Run into an exit code, or an error if it never ran.
func exitCode(cmd *exec.Cmd, err error) (int, error) {
	if err == nil {
		return 0, nil
	}
	var exit *exec.ExitError
	if !errors.As(err, &exit) {
		if cmd.ProcessState != nil { // it ran, but copying its output failed
			return cmd.ProcessState.ExitCode(), nil
		}
		return -1, err
	}
	if ws, ok := exit.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return 128 + int(ws.Signal()), nil
	}
	return exit.ExitCode(), nil
}

// tailWriter passes writes through to w and keeps the last n lines.
type tailWriter struct {
	mu    sync.Mutex
	w     io.Writer
	n     int
	lines []string
	part  []byte // a line not finished yet
}

func newTailWriter(w io.Writer, n int) *tailWriter { return &tailWriter{w: w, n: n} }

func (t *tailWriter) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	data := append(t.part, p...)
	for {
		i := bytes.IndexByte(data, '\n')
		if i < 0 {
			break
		}
		t.keep(string(data[:i]))
		data = data[i+1:]
	}
	t.part = append([]byte(nil), data...)
	// Showing the output matters less than running restic to the end: ignore write errors.
	_, _ = t.w.Write(p)
	return len(p), nil
}

func (t *tailWriter) keep(line string) {
	t.lines = append(t.lines, strings.TrimRight(line, "\r"))
	if len(t.lines) > t.n {
		t.lines = t.lines[len(t.lines)-t.n:]
	}
}

// Flush keeps an unfinished last line.
func (t *tailWriter) Flush() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(t.part) > 0 {
		t.keep(string(t.part))
		t.part = nil
	}
}

// String is the kept lines.
func (t *tailWriter) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return strings.Join(t.lines, "\n")
}
