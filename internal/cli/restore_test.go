package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Evimore/carryon/internal/config"
	"github.com/Evimore/carryon/internal/paths"
	"github.com/Evimore/carryon/internal/restore"
	"github.com/Evimore/carryon/internal/sysexec"
	"github.com/Evimore/carryon/internal/ui"
)

type cliPackages struct{}

func (cliPackages) Installed(context.Context) ([]string, error) { return []string{"base"}, nil }
func (cliPackages) SyncPackages(context.Context) ([]string, error) {
	return []string{"base", "fuse3"}, nil
}
func (cliPackages) ConfiguredRepos(context.Context) ([]string, error) {
	return []string{"core", "extra"}, nil
}

// fakeRestoreEnv points carryon restore at a small fake machine, laptop, with alice on it.
func fakeRestoreEnv(t *testing.T) *sysexec.Fake {
	t.Helper()
	root := t.TempDir()
	for name, body := range map[string]string{
		"proc/self/mountinfo":      "25 1 0:24 / / rw - ext4 /dev/sda2 rw\n",
		"proc/sys/kernel/hostname": "laptop\n",
		"etc/passwd":               "root:x:0:0::/root:/usr/bin/bash\nalice:x:1000:1000::/home/alice:/usr/bin/zsh\n",
		"etc/group":                "wheel:x:998:alice\n",
		"etc/kernel/cmdline":       "root=/dev/sda2 rw quiet\n",
	} {
		p := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	run := &sysexec.Fake{Responses: map[string]sysexec.Response{
		"systemctl is-enabled -- sshd.service": {Stdout: "disabled\n", Code: 1},
	}}
	old := restoreEnv
	restoreEnv = func(paths.Dirs) restoreSystem {
		return restoreSystem{
			Root: root, Run: run, Packages: cliPackages{},
			Ask:      func(bool) ui.Prompter { return ui.Defaults{} },
			Repo:     func(config.Config) restore.Repo { t.Fatal("the repository was opened"); return nil },
			Location: time.UTC,
			Now:      func() time.Time { return time.Date(2026, 10, 14, 9, 0, 0, 0, time.UTC) },
		}
	}
	t.Cleanup(func() { restoreEnv = old })
	return run
}

func TestRestorePlanFromRecord(t *testing.T) {
	fakeRestoreEnv(t)
	dirs := paths.Dirs{State: t.TempDir()}
	out, err := run(t, dirs, "restore", "--plan", "--record", "../record/testdata/format1")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	for _, want := range []string{
		"Restore plan: desktop -> laptop  (12/14 bundles)",
		"  Packages           1  fuse3",
		"  Full plan with diffs: " + filepath.Join(dirs.State, "restore-plan.txt"),
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "T15") {
		t.Errorf("--plan said more than the plan:\n%s", out)
	}
	full, err := os.ReadFile(filepath.Join(dirs.State, "restore-plan.txt"))
	if err != nil || !strings.Contains(string(full), "[Services] enable     sshd.service") {
		t.Errorf("plan file: %v\n%s", err, full)
	}
	if fi, err := os.Stat(filepath.Join(dirs.State, "restore-plan.txt")); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("plan file mode: %v %v", fi, err)
	}
	// Without a terminal, nothing is remembered: the default ticks weren't a choice.
	if _, err := os.Stat(filepath.Join(dirs.State, "selections")); err == nil {
		t.Error("default ticks were remembered as choices")
	}
}

func TestRestoreWithoutPlanChangesNothing(t *testing.T) {
	fake := fakeRestoreEnv(t)
	out, err := run(t, paths.Dirs{State: t.TempDir()}, "restore", "--defaults", "--record", "../record/testdata/format1")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if !strings.Contains(out, "Applying the plan comes with a later version of Carryon (roadmap T15). Nothing was changed.") {
		t.Errorf("output:\n%s", out)
	}
	for _, c := range fake.Commands() {
		if c != "systemctl is-enabled -- sshd.service" {
			t.Errorf("ran %s", c)
		}
	}
}

func TestRestoreNeedsRootForTheBackup(t *testing.T) {
	fakeRestoreEnv(t)
	old := geteuid
	geteuid = func() int { return 1000 }
	t.Cleanup(func() { geteuid = old })
	_, err := run(t, paths.Dirs{State: t.TempDir()}, "restore", "--plan")
	if err == nil || !strings.Contains(err.Error(), "needs root") || !strings.Contains(err.Error(), "--record DIR") {
		t.Errorf("err = %v", err)
	}
}

func TestRestoreBadUserFlag(t *testing.T) {
	fakeRestoreEnv(t)
	_, err := run(t, paths.Dirs{State: t.TempDir()}, "restore", "--plan", "--record", "../record/testdata/format1", "--user", "alice")
	if err == nil || !strings.Contains(err.Error(), "OLD=NEW") {
		t.Errorf("err = %v", err)
	}
}

func TestRestoreUserMapping(t *testing.T) {
	fakeRestoreEnv(t)
	_, err := run(t, paths.Dirs{State: t.TempDir()}, "restore", "--plan", "--record", "../record/testdata/format1", "--user", "alice=dave")
	if err == nil || !strings.Contains(err.Error(), "there is no user dave here") {
		t.Errorf("err = %v", err)
	}
}
