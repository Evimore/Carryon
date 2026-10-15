package sysexec

import (
	"context"
	"fmt"
	"strings"
	"sync"
)

// Fake is a Runner for tests. It answers each command line with a registered Response and records
// every call. A command with no response fails with "unexpected command: ...", so a test notices
// any program the code runs that it didn't plan for.
//
// Command lines are written the way Join prints them: words separated by single spaces, quoted
// for a shell where needed, such as "pacman -Qqe" or "sh -c 'echo hi'". The "unexpected command"
// error shows the exact line to register.
//
// A Fake is safe for concurrent use. Don't copy one after its first call.
type Fake struct {
	// Responses maps a command line to its answer.
	Responses map[string]Response
	// Programs are the names Exists reports, besides the first word of every registered command
	// that isn't Missing.
	Programs []string
	// Root makes AsUser wrap commands in runuser, as it would for a process running as root. Leave
	// it false to see the user's commands unwrapped; set it to test the wrapping itself.
	Root bool
	// Path is the PATH AsUser extends when Root is set. Empty means "/usr/bin".
	Path string

	mu    sync.Mutex
	calls []Call
}

// Response is a Fake's answer to one command line.
type Response struct {
	Stdout string
	Stderr string // ends up in the *ExitError when Code isn't zero
	Code   int    // exit code; non-zero makes the call fail with an *ExitError
	// Missing makes the call fail the way a program that isn't installed does (wrapping
	// exec.ErrNotFound), and Exists report false for it.
	Missing bool
	// Err, if set, is returned as is, after Stdout.
	Err error
}

// Call is one command a Fake was asked to run.
type Call struct {
	Args     []string // program first
	Attached bool     // run with Run (attached to the terminal) rather than Output
}

// String returns the command line, as Join prints it.
func (c Call) String() string { return Join(c.Args) }

// On registers r as the answer to cmdline and returns f, so registrations can be chained.
func (f *Fake) On(cmdline string, r Response) *Fake {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Responses == nil {
		f.Responses = map[string]Response{}
	}
	f.Responses[cmdline] = r
	return f
}

// Output returns the registered Stdout, and an *ExitError if Code isn't zero.
func (f *Fake) Output(_ context.Context, name string, args ...string) ([]byte, error) {
	r, err := f.call(false, name, args)
	if err != nil {
		return nil, err
	}
	var out []byte
	if r.Stdout != "" {
		out = []byte(r.Stdout)
	}
	return out, r.result(name, args, true)
}

// Run records the call and fails like Output does. Nothing is written to the terminal.
func (f *Fake) Run(_ context.Context, name string, args ...string) error {
	r, err := f.call(true, name, args)
	if err != nil {
		return err
	}
	return r.result(name, args, false)
}

// Exists reports whether name is in Programs or starts a registered command that isn't Missing.
func (f *Fake) Exists(name string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, p := range f.Programs {
		if p == name {
			return true
		}
	}
	for line, r := range f.Responses {
		first, _, _ := strings.Cut(line, " ")
		if first == name && !r.Missing {
			return true
		}
	}
	return false
}

// AsUser returns a runner that runs commands as u through f. See the package-level AsUser and
// the Root field.
func (f *Fake) AsUser(u User) Runner {
	return AsUser(f, u, f.Root, f.Path)
}

// Calls returns every call so far, in order.
func (f *Fake) Calls() []Call {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Call(nil), f.calls...)
}

// Commands returns the command line of every call so far, in order.
func (f *Fake) Commands() []string {
	calls := f.Calls()
	lines := make([]string, len(calls))
	for i, c := range calls {
		lines[i] = c.String()
	}
	return lines
}

// call records a call and looks up its response.
func (f *Fake) call(attached bool, name string, args []string) (Response, error) {
	argv := append([]string{name}, args...)
	line := Join(argv)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, Call{Args: argv, Attached: attached})
	r, ok := f.Responses[line]
	if !ok {
		return Response{}, fmt.Errorf("unexpected command: %s", line)
	}
	return r, nil
}

// result is the error a call with response r ends with.
func (r Response) result(name string, args []string, withStderr bool) error {
	switch {
	case r.Missing:
		return notFound(name)
	case r.Err != nil:
		return r.Err
	case r.Code != 0:
		e := &ExitError{
			Args:   append([]string{name}, args...),
			Code:   r.Code,
			Status: fmt.Sprintf("exit status %d", r.Code),
		}
		if withStderr {
			e.Stderr = tail(r.Stderr, stderrLines)
		}
		return e
	}
	return nil
}
