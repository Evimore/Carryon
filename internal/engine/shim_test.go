package engine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Evimore/carryon/internal/config"
	"github.com/Evimore/carryon/internal/paths"
)

func fakeLookups(euid int) ShimLookups {
	return ShimLookups{
		Euid: euid,
		LookPath: func(name string) (string, error) {
			if name == "rclone" || name == "runuser" || name == "env" {
				return "/usr/bin/" + name, nil
			}
			return "", errors.New("not found")
		},
		HomeOf: func(name string) (string, error) {
			if name == "alice" {
				return "/home/alice", nil
			}
			return "", fmt.Errorf("user: unknown user %s", name)
		},
	}
}

func TestShimCommand(t *testing.T) {
	args := []string{"serve", "restic", "--stdio", "--b2-hard-delete", "my-cloud:backups"}
	rootEnv := []string{
		"PATH=/usr/bin", "HOME=/root", "XDG_CONFIG_HOME=/root/.config", "RESTIC_PASSWORD_FILE=/etc/carryon/repository-password",
		"RESTIC_REPOSITORY=rclone:my-cloud:backups", "RCLONE_TRANSFERS=8", RcloneUserEnv + "=alice",
	}
	tests := []struct {
		name     string
		euid     int
		env      []string
		wantPath string
		wantArgv []string
		wantEnv  []string // must be present
		dropEnv  []string // must be absent
		wantErr  string
	}{
		{
			name:     "root with a user: runuser with the user's HOME",
			euid:     0,
			env:      rootEnv,
			wantPath: "/usr/bin/runuser",
			wantArgv: append([]string{"runuser", "-u", "alice", "--", "/usr/bin/env", "HOME=/home/alice", "/usr/bin/rclone"}, args...),
			wantEnv:  []string{"PATH=/usr/bin", "RCLONE_TRANSFERS=8"},
			dropEnv:  []string{"HOME", "XDG_CONFIG_HOME", "RESTIC_PASSWORD_FILE", "RESTIC_REPOSITORY", RcloneUserEnv},
		},
		{
			name:     "root without a user: plain rclone",
			euid:     0,
			env:      []string{"PATH=/usr/bin", "HOME=/root"},
			wantPath: "/usr/bin/rclone",
			wantArgv: append([]string{"rclone"}, args...),
			wantEnv:  []string{"HOME=/root"},
		},
		{
			name:     "not root: plain rclone, even with a user",
			euid:     1000,
			env:      []string{"PATH=/usr/bin", "HOME=/home/alice", RcloneUserEnv + "=alice"},
			wantPath: "/usr/bin/rclone",
			wantArgv: append([]string{"rclone"}, args...),
			wantEnv:  []string{"HOME=/home/alice"},
			dropEnv:  []string{RcloneUserEnv},
		},
		{
			name:    "unknown user",
			euid:    0,
			env:     []string{RcloneUserEnv + "=mallory"},
			wantErr: `rclone_user "mallory" isn't a user on this machine`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, err := ShimCommand(tt.env, args, fakeLookups(tt.euid))
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if c.Path != tt.wantPath || !slices.Equal(c.Argv, tt.wantArgv) {
				t.Errorf("runs %s %q\nwant %s %q", c.Path, c.Argv, tt.wantPath, tt.wantArgv)
			}
			for _, kv := range tt.wantEnv {
				if !slices.Contains(c.Env, kv) {
					t.Errorf("env lacks %s: %q", kv, c.Env)
				}
			}
			got := envMap(c.Env)
			for _, k := range tt.dropEnv {
				if _, ok := got[k]; ok {
					t.Errorf("env keeps %s: %q", k, c.Env)
				}
			}
		})
	}
}

func TestShimWithoutRclone(t *testing.T) {
	sys := fakeLookups(0)
	sys.LookPath = func(string) (string, error) { return "", errors.New("not found") }
	if _, err := ShimCommand(nil, []string{"version"}, sys); err == nil || !strings.Contains(err.Error(), "rclone isn't installed") {
		t.Errorf("err = %v", err)
	}
}

func TestRunShimExecs(t *testing.T) {
	dir := t.TempDir()
	rclone := filepath.Join(dir, "rclone")
	if err := os.WriteFile(rclone, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	t.Setenv(RcloneUserEnv, "")
	var gotPath string
	var gotArgv []string
	execFn = func(path string, argv, _ []string) error {
		gotPath, gotArgv = path, argv
		return errors.New("exec stopped by the test")
	}
	defer func() { execFn = realExec }()

	err := RunShim([]string{"version"})
	if err == nil || !strings.Contains(err.Error(), "exec stopped by the test") {
		t.Errorf("err = %v", err)
	}
	if gotPath != rclone || !slices.Equal(gotArgv, []string{"rclone", "version"}) {
		t.Errorf("exec %s %q", gotPath, gotArgv)
	}
}

// fakeRcloneScript records what it was started with and the first bytes restic sends, then fails.
const fakeRcloneScript = `#!/bin/sh
log="$FAKE_RCLONE_LOG"
printf '%s\n' "$@" > "$log/args"
env > "$log/env"
head -c 3 > "$log/stdin"
echo "fake rclone: 401 Unauthorized" >&2
exit 1
`

// TestResticRunsRcloneThroughTheShim pins the rclone.program approach against a real restic: restic
// starts "<carryon> rclone-shim" with rclone's arguments and the remote last, the shim replaces
// itself with rclone, and restic's stdin pipe reaches rclone.
func TestResticRunsRcloneThroughTheShim(t *testing.T) {
	needRestic(t)
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	bin, log := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "rclone"), []byte(fakeRcloneScript), 0o755); err != nil {
		t.Fatal(err)
	}
	pw := filepath.Join(t.TempDir(), "password")
	if err := os.WriteFile(pw, []byte("correct horse\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	repo := config.Repository{URL: "rclone:my-cloud:backups", PasswordFile: pw}
	e := New(repo, paths.Dirs{Cache: t.TempDir()}, "desktop")
	e.Self = self
	e.BaseEnv = []string{
		"PATH=" + bin + string(os.PathListSeparator) + os.Getenv("PATH"),
		"HOME=" + t.TempDir(),
		"FAKE_RCLONE_LOG=" + log,
		shimTestEnv + "=1",
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	r, err := e.Run(ctx, "-o", "rclone.timeout=20s", "cat", "config")
	if err != nil {
		t.Fatal(err)
	}
	if r.Code == 0 {
		t.Fatal("restic succeeded against a fake rclone")
	}

	args, err := os.ReadFile(filepath.Join(log, "args"))
	if err != nil {
		t.Fatalf("the fake rclone never ran: %v\nrestic said:\n%s", err, r.Stderr)
	}
	want := "serve\nrestic\n--stdio\n--b2-hard-delete\nmy-cloud:backups\n"
	if string(args) != want {
		t.Errorf("rclone got args %q, want %q", args, want)
	}
	if stdin, _ := os.ReadFile(filepath.Join(log, "stdin")); string(stdin) != "PRI" {
		t.Errorf("rclone read %q from stdin, want the start of restic's HTTP/2 preface", stdin)
	}
	env, _ := os.ReadFile(filepath.Join(log, "env"))
	if strings.Contains(string(env), RcloneUserEnv+"=") {
		t.Errorf("the shim passed %s on to rclone", RcloneUserEnv)
	}
	if p := r.Err().(*Error); !strings.Contains(p.Fix, "rclone reconnect my-cloud:") {
		t.Errorf("problem = %+v\nstderr:\n%s", p.Problem, r.Stderr)
	}
}
