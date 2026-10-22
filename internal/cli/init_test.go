package cli

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Evimore/carryon/internal/config"
	"github.com/Evimore/carryon/internal/engine"
	"github.com/Evimore/carryon/internal/paths"
	"github.com/Evimore/carryon/internal/setup"
	"github.com/Evimore/carryon/internal/sysexec"
	"github.com/Evimore/carryon/internal/ui"
)

// fakeSetup makes init and recovery-kit use a fake mount table (one system disk), no systemctl,
// and restic with a clean environment.
func fakeSetup(t *testing.T) paths.Dirs {
	t.Helper()
	tmp := t.TempDir()
	dirs := paths.Dirs{
		Conf: filepath.Join(tmp, "conf"), State: filepath.Join(tmp, "state"), Cache: filepath.Join(tmp, "cache"),
		Run: filepath.Join(tmp, "run"), Share: filepath.Join(tmp, "share"),
	}
	root := filepath.Join(tmp, "root")
	if err := os.MkdirAll(filepath.Join(root, "proc/self"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "proc/self/mountinfo"), []byte("20 1 259:2 / / rw shared:1 - ext4 /dev/nvme0n1p2 rw\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	oldEnv, oldPrompt := setupEnv, newPrompter
	setupEnv = func(d paths.Dirs, p ui.Prompter, out, errOut io.Writer) *setup.Env {
		env := oldEnv(d, p, out, errOut)
		env.Root = root
		env.Runner = &sysexec.Fake{}
		env.SystemdDir = filepath.Join(tmp, "systemd")
		env.Hostname = func() (string, error) { return "desktop", nil }
		env.Fingerprint = func() string { return "0123456789abcdef" }
		env.Engine = func(repo config.Repository, machine string) *engine.Engine {
			e := engine.New(repo, d, machine)
			e.BaseEnv = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + tmp}
			e.Stderr = io.Discard
			return e
		}
		return env
	}
	newPrompter = func(context.Context, bool) ui.Prompter { return ui.Defaults{} } // no terminal in tests
	t.Cleanup(func() { setupEnv, newPrompter = oldEnv, oldPrompt })
	return dirs
}

func TestInitAndRecoveryKit(t *testing.T) {
	if _, err := exec.LookPath("restic"); err != nil {
		t.Skip("restic isn't installed")
	}
	asUser(t, 0)
	dirs := fakeSetup(t)
	repo := filepath.Join(filepath.Dir(dirs.Conf), "backup")
	out, err := run(t, dirs, "init", "--defaults", "--repo", repo, "--users", "alice,bob", "--schedule", "hourly", "--kit-saved")
	if err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}
	for _, s := range []string{"Carryon recovery kit", "Repository:    " + repo, "systemctl isn't available", "Backups are set up: they run hourly"} {
		if !strings.Contains(out, s) {
			t.Errorf("init output lacks %q:\n%s", s, out)
		}
	}
	cfg, _, err := config.Load(dirs.ConfigFile())
	if err != nil || strings.Join(cfg.Backup.Users, ",") != "alice,bob" || cfg.MachineName != "desktop" {
		t.Fatalf("config = %+v, %v", cfg, err)
	}
	if !setup.KitConfirmed(dirs) {
		t.Error("--kit-saved didn't note the kit as saved")
	}

	pw, err := setup.ReadPasswordFile(dirs.PasswordFile())
	if err != nil {
		t.Fatal(err)
	}
	out, err = run(t, dirs, "recovery-kit")
	if err != nil || !strings.Contains(out, "Password:      "+pw) || !strings.Contains(out, "Machine name:  desktop") {
		t.Fatalf("recovery-kit: %v\n%s", err, out)
	}
}

func TestInitAndRecoveryKitNeedRoot(t *testing.T) {
	asUser(t, 1000)
	dirs := fakeSetup(t)
	for _, args := range [][]string{{"init"}, {"init", "--connect", "/mnt/backup"}, {"recovery-kit"}} {
		_, err := run(t, dirs, args...)
		if err == nil || !strings.Contains(err.Error(), "sudo") {
			t.Errorf("%q: err = %v", args, err)
		}
	}
}

func TestInitRefusesFlagsThatDontGoTogether(t *testing.T) {
	asUser(t, 0)
	dirs := fakeSetup(t)
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"init", "--connect", "/mnt/backup", "--repo", "/mnt/other"}, "--repo is for"},
		{[]string{"init", "--settings-from", "desktop"}, "only works with --connect"},
		{[]string{"init", "--password-file", "/x", "--password-command", "cat /x"}, "not both"},
	} {
		_, err := run(t, dirs, c.args...)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%q: err = %v, want %q", c.args, err, c.want)
		}
	}
}

func TestRecoveryKitNeedsSettings(t *testing.T) {
	asUser(t, 0)
	dirs := fakeSetup(t)
	_, err := run(t, dirs, "recovery-kit")
	if err == nil || !strings.Contains(err.Error(), "sudo carryon init") {
		t.Fatalf("err = %v", err)
	}
}
