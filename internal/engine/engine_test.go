package engine

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Evimore/carryon/internal/config"
	"github.com/Evimore/carryon/internal/paths"
)

func envMap(env []string) map[string]string {
	m := map[string]string{}
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		m[k] = v // last one wins, as in a process environment
	}
	return m
}

func countVar(env []string, name string) int {
	n := 0
	for _, kv := range env {
		if strings.HasPrefix(kv, name+"=") {
			n++
		}
	}
	return n
}

func TestEnv(t *testing.T) {
	dirs := paths.Dirs{Conf: "/etc/carryon", Cache: "/var/cache/carryon"}
	inherited := []string{
		"PATH=/usr/bin", "RESTIC_PASSWORD=leaked", "RESTIC_REPOSITORY=/elsewhere", "RESTIC_CACHE_DIR=/tmp/c",
		"RESTIC_COMPRESSION=max", "CARRYON_RCLONE_USER=mallory",
	}
	tests := []struct {
		name string
		repo config.Repository
		base []string
		want map[string]string // "" means must be absent
	}{
		{
			name: "default password file, HOME filled in for systemd",
			repo: config.Repository{URL: "/mnt/backup/carryon"},
			base: inherited,
			want: map[string]string{
				"RESTIC_REPOSITORY":       "/mnt/backup/carryon",
				"RESTIC_PASSWORD_FILE":    "/etc/carryon/repository-password",
				"RESTIC_PASSWORD_COMMAND": "",
				"RESTIC_PASSWORD":         "",
				"RESTIC_CACHE_DIR":        "/var/cache/carryon/restic",
				"RESTIC_COMPRESSION":      "max",
				"CARRYON_RCLONE_USER":     "",
				"PATH":                    "/usr/bin",
			},
		},
		{
			name: "password command",
			repo: config.Repository{URL: "sftp:alice@example.com:/backups", PasswordCommand: "systemd-creds decrypt /etc/carryon/pw.cred -"},
			base: append([]string{"HOME=/root"}, inherited...),
			want: map[string]string{
				"HOME":                    "/root",
				"RESTIC_PASSWORD_COMMAND": "systemd-creds decrypt /etc/carryon/pw.cred -",
				"RESTIC_PASSWORD_FILE":    "",
			},
		},
		{
			name: "rclone passes the user to the shim",
			repo: config.Repository{URL: "rclone:my-cloud:backups", PasswordFile: "/etc/carryon/pw", RcloneUser: "alice"},
			base: inherited,
			want: map[string]string{
				"RESTIC_PASSWORD_FILE": "/etc/carryon/pw",
				"CARRYON_RCLONE_USER":  "alice",
			},
		},
		{
			name: "no rclone user for other repositories",
			repo: config.Repository{URL: "/srv/repo", RcloneUser: "alice"},
			base: inherited,
			want: map[string]string{"CARRYON_RCLONE_USER": ""},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := New(tt.repo, dirs, "desktop")
			e.BaseEnv = tt.base
			env := e.Env()
			got := envMap(env)
			if got["HOME"] == "" {
				t.Errorf("HOME isn't set: %v", env)
			}
			for k, want := range tt.want {
				if want == "" {
					if v, ok := got[k]; ok {
						t.Errorf("%s = %q, want it unset", k, v)
					}
				} else if got[k] != want {
					t.Errorf("%s = %q, want %q", k, got[k], want)
				}
			}
			for _, k := range ownedEnv {
				if n := countVar(env, k); n > 1 {
					t.Errorf("%s is set %d times", k, n)
				}
			}
		})
	}
}

func TestArgs(t *testing.T) {
	tests := []struct {
		name, url, self string
		want            []string
		wantErr         string
	}{
		{name: "local", url: "/mnt/backup", want: []string{"--retry-lock", "30m", "snapshots"}},
		{name: "rclone", url: "rclone:my-cloud:backups", self: "/usr/bin/carryon",
			want: []string{"--retry-lock", "30m", "-o", "rclone.program=/usr/bin/carryon rclone-shim", "snapshots"}},
		{name: "rclone, path with a space", url: "rclone:my-cloud:backups", self: "/opt/my apps/carryon",
			want: []string{"--retry-lock", "30m", "-o", "rclone.program='/opt/my apps/carryon' rclone-shim", "snapshots"}},
		{name: "rclone, path restic can't split", url: "rclone:my-cloud:backups", self: `/opt/alice's/carryon`, wantErr: "can't pass to rclone"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := New(config.Repository{URL: tt.url}, paths.Dirs{}, "desktop")
			e.Self = tt.self
			got, err := e.Args("snapshots")
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(got, tt.want) {
				t.Errorf("Args = %q\nwant   %q", got, tt.want)
			}
		})
	}
}

// fakeRunner records the command and plays back output.
type fakeRunner struct {
	got            Cmd
	stdout, stderr string
	code           int
	err            error
}

func (f *fakeRunner) run(_ context.Context, c Cmd) (int, error) {
	f.got = c
	if f.err != nil {
		return -1, f.err
	}
	if c.Stdout != nil {
		_, _ = c.Stdout.Write([]byte(f.stdout))
	}
	if c.Stderr != nil {
		// in two writes, splitting a line, as a pipe might
		half := len(f.stderr) / 2
		_, _ = c.Stderr.Write([]byte(f.stderr[:half]))
		_, _ = c.Stderr.Write([]byte(f.stderr[half:]))
	}
	return f.code, nil
}

func TestRunQuiet(t *testing.T) {
	f := &fakeRunner{stdout: "[]", stderr: "Fatal: wrong password or no key found\n", code: ExitBadPassword}
	e := New(config.Repository{URL: "/mnt/backup"}, paths.Dirs{Conf: "/etc/carryon"}, "desktop")
	e.Program = "/usr/bin/restic"
	e.Runner = f.run
	r, err := e.Run(context.Background(), "snapshots", "--json")
	if err != nil {
		t.Fatal(err)
	}
	if f.got.Path != "/usr/bin/restic" || !slices.Equal(f.got.Args, []string{"--retry-lock", "30m", "snapshots", "--json"}) {
		t.Errorf("ran %s %q", f.got.Path, f.got.Args)
	}
	if r.Code != ExitBadPassword || r.Stdout != "[]" || !strings.Contains(r.Stderr, "wrong password") {
		t.Errorf("result = %+v", r)
	}
	var re *Error
	if !errors.As(r.Err(), &re) {
		t.Fatalf("Err() = %v", r.Err())
	}
	if re.Cause != "wrong password or no key found" || !strings.Contains(re.Fix, "/etc/carryon/repository-password") {
		t.Errorf("problem = %+v", re.Problem)
	}
}

func TestRunCantStart(t *testing.T) {
	e := New(config.Repository{URL: "/mnt/backup"}, paths.Dirs{}, "desktop")
	e.Runner = (&fakeRunner{err: errors.New(`exec: "restic": executable file not found in $PATH`)}).run
	_, err := e.Run(context.Background(), "snapshots")
	var re *Error
	if !errors.As(err, &re) || re.Code != -1 || !strings.Contains(re.Fix, "install restic") {
		t.Fatalf("err = %#v", err)
	}
}

func TestLiveKeepsTheLastLines(t *testing.T) {
	var lines []string
	for i := 1; i <= 40; i++ {
		lines = append(lines, "line "+strings.Repeat("x", i%3)+string(rune('0'+i%10)))
	}
	f := &fakeRunner{stdout: `{"message_type":"summary"}`, stderr: strings.Join(lines, "\n") + "\nFatal: unable to save snapshot", code: 1}
	var shown bytes.Buffer
	e := New(config.Repository{URL: "/mnt/backup"}, paths.Dirs{}, "desktop")
	e.Runner, e.Stderr = f.run, &shown

	r, err := e.Live(context.Background(), nil, "backup", "--json", "/etc")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(shown.String(), "$ restic backup --json /etc\n") || !strings.HasSuffix(shown.String(), f.stderr) {
		t.Errorf("shown output:\n%s", shown.String())
	}
	kept := strings.Split(r.Stderr, "\n")
	if len(kept) != TailLines || kept[len(kept)-1] != "Fatal: unable to save snapshot" || kept[0] != lines[11] {
		t.Errorf("kept %d lines, first %q, last %q", len(kept), kept[0], kept[len(kept)-1])
	}
	if r.Stdout != f.stdout {
		t.Errorf("stdout = %q", r.Stdout)
	}
	if err := r.Err(); err == nil || !strings.Contains(err.Error(), "unable to save snapshot") {
		t.Errorf("Err() = %v", err)
	}
}

func TestExecRunnerExitCodes(t *testing.T) {
	tests := []struct {
		script string
		want   int
	}{
		{"exit 0", 0},
		{"exit 12", 12},
		{"kill -TERM $$", 128 + 15},
	}
	for _, tt := range tests {
		code, err := ExecRunner(context.Background(), Cmd{Path: "/bin/sh", Args: []string{"-c", tt.script}})
		if err != nil || code != tt.want {
			t.Errorf("%q: code %d, err %v; want %d", tt.script, code, err, tt.want)
		}
	}
	if _, err := ExecRunner(context.Background(), Cmd{Path: "/nonexistent/restic"}); err == nil {
		t.Error("a missing program gave no error")
	}
}

func TestExecRunnerInterruptsOnCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	out := &lockedBuffer{}
	done := make(chan int)
	go func() {
		// Like restic: clean up on SIGINT and exit 130.
		code, _ := ExecRunner(ctx, Cmd{Path: "/bin/sh", Args: []string{"-c", `trap 'echo cleaned; exit 130' INT; echo ready; while :; do sleep 0.05; done`}, Stdout: out})
		done <- code
	}()
	for !strings.Contains(out.String(), "ready") {
		select {
		case <-done:
			t.Fatal("the script ended early")
		case <-time.After(10 * time.Millisecond):
		}
	}
	cancel()
	if code := <-done; code != ExitInterrupted {
		t.Errorf("code = %d, want %d", code, ExitInterrupted)
	}
	if !strings.Contains(out.String(), "cleaned") {
		t.Errorf("the script didn't get SIGINT: %q", out.String())
	}
}

// lockedBuffer is a buffer a test can read while a command writes to it.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
