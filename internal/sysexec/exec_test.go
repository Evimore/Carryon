package sysexec

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func TestJoin(t *testing.T) {
	tests := []struct {
		args []string
		want string
	}{
		{[]string{"pacman", "-Qqe"}, "pacman -Qqe"},
		{[]string{"sh", "-c", "echo hi"}, "sh -c 'echo hi'"},
		{[]string{"echo", ""}, "echo ''"},
		{[]string{"echo", "it's"}, `echo 'it'"'"'s'`},
		{[]string{"env", "PATH=/a:/b", "user@example.com,x+y%z"}, "env PATH=/a:/b user@example.com,x+y%z"},
		{[]string{"ls", "$HOME", "a*"}, "ls '$HOME' 'a*'"},
	}
	for _, tt := range tests {
		if got := Join(tt.args); got != tt.want {
			t.Errorf("Join(%q) = %s, want %s", tt.args, got, tt.want)
		}
	}
}

func TestTail(t *testing.T) {
	var lines []string
	for i := 1; i <= 30; i++ {
		lines = append(lines, fmt.Sprintf("line %d", i))
	}
	got := tail(strings.Join(lines, "\n")+"\n\n", 20)
	if want := strings.Join(lines[10:], "\n"); got != want {
		t.Errorf("tail = %q, want %q", got, want)
	}
	if got := tail("one\n", 20); got != "one" {
		t.Errorf("tail(short) = %q", got)
	}
}

func TestTailBuffer(t *testing.T) {
	b := &tailBuffer{max: 5}
	for _, s := range []string{"ab", "cd", "efg", "h"} {
		b.Write([]byte(s))
	}
	if got := b.String(); got != "defgh" {
		t.Errorf("after small writes = %q", got)
	}
	b.Write([]byte("0123456789"))
	if got := b.String(); got != "56789" {
		t.Errorf("after a big write = %q", got)
	}
}

func TestOutput(t *testing.T) {
	ctx := context.Background()
	tests := []struct {
		name       string
		args       []string
		wantOut    string
		wantCode   int // -1: no error
		wantInErr  []string
		notInErr   []string
		wantNotFnd bool
	}{
		{name: "success", args: []string{"sh", "-c", "echo hello; echo noise >&2"}, wantOut: "hello\n", wantCode: -1},
		{name: "true", args: []string{"true"}, wantCode: -1},
		{name: "false", args: []string{"false"}, wantCode: 1, wantInErr: []string{"false failed (exit status 1)"}},
		{
			name:     "failure keeps stdout and stderr",
			args:     []string{"sh", "-c", "echo partial; echo 'disk is full' >&2; exit 3"},
			wantOut:  "partial\n",
			wantCode: 3,
			wantInErr: []string{
				"sh -c 'echo partial; echo '\"'\"'disk is full'\"'\"' >&2; exit 3' failed (exit status 3)",
				"disk is full",
			},
		},
		{
			name:      "only the last lines of stderr",
			args:      []string{"sh", "-c", "for i in $(seq 1 50); do echo err$i >&2; done; exit 2"},
			wantCode:  2,
			wantInErr: []string{"err31\n", "err50"},
			notInErr:  []string{"err30\n"},
		},
		{name: "missing program", args: []string{"carryon-no-such-program"}, wantNotFnd: true,
			wantInErr: []string{"carryon-no-such-program isn't installed"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out, err := New().Output(ctx, tt.args[0], tt.args[1:]...)
			if string(out) != tt.wantOut {
				t.Errorf("stdout = %q, want %q", out, tt.wantOut)
			}
			if tt.wantNotFnd {
				if !errors.Is(err, exec.ErrNotFound) {
					t.Fatalf("err = %v, want exec.ErrNotFound", err)
				}
			} else if tt.wantCode == -1 {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			} else {
				code, ok := ExitCode(err)
				if !ok || code != tt.wantCode {
					t.Fatalf("ExitCode(%v) = %d, %v; want %d", err, code, ok, tt.wantCode)
				}
			}
			for _, s := range tt.wantInErr {
				if !strings.Contains(err.Error(), s) {
					t.Errorf("error %q doesn't contain %q", err, s)
				}
			}
			for _, s := range tt.notInErr {
				if strings.Contains(err.Error(), s) {
					t.Errorf("error %q contains %q", err, s)
				}
			}
		})
	}
}

func TestOutputTimeout(t *testing.T) {
	r := &Exec{Timeout: 100 * time.Millisecond}
	start := time.Now()
	_, err := r.Output(context.Background(), "sleep", "10")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want a deadline error", err)
	}
	if !strings.Contains(err.Error(), "sleep 10 took too long") {
		t.Errorf("err = %v", err)
	}
	if _, ok := ExitCode(err); ok {
		t.Errorf("a timeout shouldn't look like an exit code")
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("took %s to give up", d)
	}
}

func TestOutputCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := (&Exec{}).Output(ctx, "true")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

func TestRun(t *testing.T) {
	var stdout, stderr bytes.Buffer
	r := &Exec{Stdin: strings.NewReader("typed\n"), Stdout: &stdout, Stderr: &stderr}
	err := r.Run(context.Background(), "sh", "-c", "read x; echo got $x; echo warn >&2")
	if err != nil {
		t.Fatal(err)
	}
	if got := stdout.String(); got != "got typed\n" {
		t.Errorf("stdout = %q", got)
	}
	if got, want := stderr.String(), "$ sh -c 'read x; echo got $x; echo warn >&2'\nwarn\n"; got != want {
		t.Errorf("stderr = %q, want %q", got, want)
	}
}

func TestRunFailure(t *testing.T) {
	var stderr bytes.Buffer
	r := &Exec{Stdin: strings.NewReader(""), Stdout: &bytes.Buffer{}, Stderr: &stderr}
	err := r.Run(context.Background(), "sh", "-c", "echo oops >&2; exit 4")
	code, ok := ExitCode(err)
	if !ok || code != 4 {
		t.Fatalf("ExitCode(%v) = %d, %v; want 4", err, code, ok)
	}
	var ee *ExitError
	errors.As(err, &ee)
	if ee.Stderr != "" {
		t.Errorf("Run's error shouldn't hold stderr, it went to the terminal: %q", ee.Stderr)
	}
	if !strings.Contains(stderr.String(), "oops") {
		t.Errorf("stderr = %q", stderr.String())
	}

	err = r.Run(context.Background(), "carryon-no-such-program")
	if !errors.Is(err, exec.ErrNotFound) {
		t.Errorf("err = %v, want exec.ErrNotFound", err)
	}
}

func TestExists(t *testing.T) {
	r := New()
	if !r.Exists("sh") {
		t.Error(`Exists("sh") = false`)
	}
	if r.Exists("carryon-no-such-program") {
		t.Error("Exists(missing program) = true")
	}
}

func TestExitErrorMessage(t *testing.T) {
	e := &ExitError{Args: []string{"pacman", "-S", "foo bar"}, Code: 1, Status: "exit status 1"}
	if got, want := e.Error(), "pacman -S 'foo bar' failed (exit status 1)"; got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
	e.Stderr = "error: target not found: foo bar"
	if got, want := e.Error(), "pacman -S 'foo bar' failed (exit status 1):\nerror: target not found: foo bar"; got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
	wrapped := fmt.Errorf("installing packages: %w", e)
	if code, ok := ExitCode(wrapped); !ok || code != 1 {
		t.Errorf("ExitCode(wrapped) = %d, %v", code, ok)
	}
	if _, ok := ExitCode(errors.New("other")); ok {
		t.Error("ExitCode(other error) reported an exit code")
	}
}

// Packages with their own small Runner interface read the exit code through an interface, so
// *ExitError must have the method.
func TestExitErrorHasExitCodeMethod(t *testing.T) {
	_, err := New().Output(context.Background(), "sh", "-c", "exit 3")
	var e interface{ ExitCode() int }
	if !errors.As(err, &e) {
		t.Fatalf("%T has no ExitCode() int method", err)
	}
	if got := e.ExitCode(); got != 3 {
		t.Errorf("ExitCode() = %d, want 3", got)
	}
}
