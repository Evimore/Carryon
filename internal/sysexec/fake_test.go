package sysexec

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"reflect"
	"strings"
	"sync"
	"testing"
)

func isRoot() bool { return os.Geteuid() == 0 }

func TestFake(t *testing.T) {
	boom := errors.New("boom")
	f := (&Fake{}).
		On("pacman -Qqe", Response{Stdout: "base\nlinux\n"}).
		On("pacman -Qi nope", Response{Stdout: "partial", Stderr: "error: package 'nope' was not found", Code: 1}).
		On("paru --version", Response{Missing: true}).
		On("sh -c 'exit 0'", Response{Err: boom})

	tests := []struct {
		cmd      []string
		wantOut  string
		wantCode int // 0 with wantErr empty: success
		wantErr  string
		wantIs   error
	}{
		{cmd: []string{"pacman", "-Qqe"}, wantOut: "base\nlinux\n"},
		{cmd: []string{"pacman", "-Qi", "nope"}, wantOut: "partial", wantCode: 1,
			wantErr: "pacman -Qi nope failed (exit status 1):\nerror: package 'nope' was not found"},
		{cmd: []string{"paru", "--version"}, wantErr: "paru isn't installed", wantIs: exec.ErrNotFound},
		{cmd: []string{"sh", "-c", "exit 0"}, wantErr: "boom", wantIs: boom},
		{cmd: []string{"systemctl", "is-enabled", "sshd"}, wantErr: "unexpected command: systemctl is-enabled sshd"},
	}
	for _, tt := range tests {
		t.Run(Join(tt.cmd), func(t *testing.T) {
			out, err := f.Output(context.Background(), tt.cmd[0], tt.cmd[1:]...)
			if string(out) != tt.wantOut {
				t.Errorf("stdout = %q, want %q", out, tt.wantOut)
			}
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.HasPrefix(err.Error(), tt.wantErr) {
				t.Fatalf("err = %v, want %q", err, tt.wantErr)
			}
			if tt.wantIs != nil && !errors.Is(err, tt.wantIs) {
				t.Errorf("err = %v doesn't wrap %v", err, tt.wantIs)
			}
			if code, ok := ExitCode(err); tt.wantCode != 0 && (!ok || code != tt.wantCode) {
				t.Errorf("ExitCode = %d, %v; want %d", code, ok, tt.wantCode)
			}
		})
	}

	want := []string{"pacman -Qqe", "pacman -Qi nope", "paru --version", "sh -c 'exit 0'", "systemctl is-enabled sshd"}
	if got := f.Commands(); !reflect.DeepEqual(got, want) {
		t.Errorf("Commands() = %q, want %q", got, want)
	}
}

func TestFakeRun(t *testing.T) {
	f := &Fake{Responses: map[string]Response{
		"pacman -S --needed vim": {},
		"pacman -S nope":         {Stderr: "error: target not found: nope", Code: 1},
	}}
	ctx := context.Background()
	if err := f.Run(ctx, "pacman", "-S", "--needed", "vim"); err != nil {
		t.Fatal(err)
	}
	err := f.Run(ctx, "pacman", "-S", "nope")
	if code, ok := ExitCode(err); !ok || code != 1 {
		t.Fatalf("ExitCode(%v) = %d, %v", err, code, ok)
	}
	if strings.Contains(err.Error(), "target not found") {
		t.Errorf("Run's error holds stderr like a real attached run wouldn't: %v", err)
	}
	if err := f.Run(ctx, "reboot"); err == nil || err.Error() != "unexpected command: reboot" {
		t.Errorf("err = %v", err)
	}
	calls := f.Calls()
	if len(calls) != 3 || !calls[0].Attached || !reflect.DeepEqual(calls[0].Args, []string{"pacman", "-S", "--needed", "vim"}) {
		t.Errorf("calls = %+v", calls)
	}
}

func TestFakeExists(t *testing.T) {
	f := &Fake{
		Programs: []string{"restic"},
		Responses: map[string]Response{
			"pacman -Qqe":    {},
			"paru --version": {Missing: true},
		},
	}
	for name, want := range map[string]bool{"restic": true, "pacman": true, "paru": false, "yay": false} {
		if got := f.Exists(name); got != want {
			t.Errorf("Exists(%q) = %v, want %v", name, got, want)
		}
	}
	if len(f.Calls()) != 0 {
		t.Error("Exists shouldn't record calls")
	}
}

func TestFakeConcurrent(t *testing.T) {
	f := (&Fake{}).On("true", Response{})
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			f.Output(context.Background(), "true")
			f.Exists("true")
		}()
	}
	wg.Wait()
	if n := len(f.Calls()); n != 20 {
		t.Errorf("recorded %d calls, want 20", n)
	}
}

// The fake and the real runner both satisfy Runner.
var (
	_ Runner = (*Exec)(nil)
	_ Runner = (*Fake)(nil)
)
