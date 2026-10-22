package cli

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// Interrupting carryon while restic runs waits for restic to stop, so it removes its lock. The
// signal goes to carryon alone, as `kill -INT` sends it; carryon passes it on to restic.
func TestInterruptWaitsForRestic(t *testing.T) {
	dirs, repo := resticSetup(t)
	if out, err := run(t, dirs, "restic", "init"); err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}
	ctx, stop := interruptible(context.Background())
	defer stop()
	var out bytes.Buffer
	root := NewRoot(&out, dirs)
	root.SetErr(&out)
	// a backup that reads until it's stopped, holding its lock all along
	root.SetArgs([]string{"restic", "backup", "--stdin-filename", "endless", "--stdin-from-command", "--",
		"sh", "-c", "while head -c 4096 /dev/urandom; do sleep 0.01; done"})
	done := make(chan error, 1)
	go func() { done <- root.ExecuteContext(ctx) }()

	locks := filepath.Join(repo, "locks")
	for deadline := time.Now().Add(20 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		if entries, _ := os.ReadDir(locks); len(entries) > 0 {
			break
		}
		select {
		case err := <-done:
			t.Fatalf("restic ended before it was interrupted: %v\n%s", err, out.String())
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("restic took no lock\n%s", out.String())
		}
	}
	if err := syscall.Kill(os.Getpid(), syscall.SIGINT); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		var exit exitError
		if !errors.As(err, &exit) || exit.code != 130 {
			t.Errorf("err = %v, want restic's exit code 130\n%s", err, out.String())
		}
	case <-time.After(40 * time.Second):
		t.Fatal("carryon still runs after the interrupt")
	}
	if entries, _ := os.ReadDir(locks); len(entries) != 0 {
		t.Errorf("restic left %d lock(s) behind", len(entries))
	}
}

// A second Ctrl+C ends carryon at once, even while a command still waits for restic. It runs in
// a child process, which the signal ends.
func TestSecondInterruptEndsCarryonAtOnce(t *testing.T) {
	if os.Getenv("CARRYON_TEST_INTERRUPT") == "1" {
		ctx, stop := interruptible(context.Background())
		defer stop()
		fmt.Println("ready")
		<-ctx.Done()
		fmt.Println("stopping")
		time.Sleep(time.Minute) // still waiting for restic
		os.Exit(0)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestSecondInterruptEndsCarryonAtOnce$")
	cmd.Env = append(os.Environ(), "CARRYON_TEST_INTERRUPT=1")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill() })
	lines := bufio.NewScanner(stdout)
	waitFor := func(want string) {
		t.Helper()
		for lines.Scan() {
			if lines.Text() == want {
				return
			}
		}
		t.Fatalf("the child never said %q", want)
	}
	waitFor("ready")
	if err := cmd.Process.Signal(syscall.SIGINT); err != nil {
		t.Fatal(err)
	}
	waitFor("stopping")
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	// Pressed again: the first press after carryon stops listening ends it.
	for deadline := time.Now().Add(5 * time.Second); ; {
		_ = cmd.Process.Signal(syscall.SIGINT)
		select {
		case <-exited:
			ws, ok := cmd.ProcessState.Sys().(syscall.WaitStatus)
			if !ok || !ws.Signaled() || ws.Signal() != syscall.SIGINT {
				t.Errorf("the child ended with %v, want killed by SIGINT", cmd.ProcessState)
			}
			return
		case <-time.After(100 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			t.Fatal("a second Ctrl+C didn't end carryon")
		}
	}
}
