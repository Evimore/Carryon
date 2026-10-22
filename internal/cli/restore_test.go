package cli

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Evimore/carryon/internal/config"
	"github.com/Evimore/carryon/internal/engine"
	"github.com/Evimore/carryon/internal/paths"
	"github.com/Evimore/carryon/internal/pkgmgr/pacman"
	"github.com/Evimore/carryon/internal/record"
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
	out, err := run(t, dirs, "restore", "--plan", "--record", "../record/testdata/format2")
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
	if strings.Contains(out, "Apply this plan?") {
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

func TestRestoreRefusesAPrototypeRecord(t *testing.T) {
	fakeRestoreEnv(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, record.MetaFile), []byte(`{"format": 1, "host": "desktop", "user": "alice"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := run(t, paths.Dirs{State: t.TempDir()}, "restore", "--plan", "--record", dir)
	if err == nil || !strings.Contains(err.Error(), "no longer restores from it") || !strings.Contains(err.Error(), "carryon restic restore") {
		t.Errorf("err = %v", err)
	}
}

func TestRestoreDefaultsOnlyShowsThePlan(t *testing.T) {
	fake := fakeRestoreEnv(t)
	out, err := run(t, paths.Dirs{State: t.TempDir()}, "restore", "--defaults", "--record", "../record/testdata/format2")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if !strings.Contains(out, "Nothing was changed. To apply this plan without a terminal, add --yes.") {
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
	_, err := run(t, paths.Dirs{State: t.TempDir()}, "restore", "--plan", "--record", "../record/testdata/format2", "--user", "alice")
	if err == nil || !strings.Contains(err.Error(), "OLD=NEW") {
		t.Errorf("err = %v", err)
	}
}

func TestRestoreUserMapping(t *testing.T) {
	fakeRestoreEnv(t)
	_, err := run(t, paths.Dirs{State: t.TempDir()}, "restore", "--plan", "--record", "../record/testdata/format2", "--user", "alice=dave")
	if err == nil || !strings.Contains(err.Error(), "there is no user dave here") {
		t.Errorf("err = %v", err)
	}
}

// okRunner succeeds at every command but the failing ones, and records them all. Every program is
// installed.
type okRunner struct {
	mu    sync.Mutex
	calls []string
	fail  map[string]bool
	out   map[string]string
}

func (r *okRunner) do(name string, args []string) error {
	line := sysexec.Join(append([]string{name}, args...))
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, line)
	if r.fail[line] {
		return &sysexec.ExitError{Args: append([]string{name}, args...), Code: 1, Status: "exit status 1"}
	}
	return nil
}

func (r *okRunner) Output(_ context.Context, name string, args ...string) ([]byte, error) {
	err := r.do(name, args)
	return []byte(r.out[sysexec.Join(append([]string{name}, args...))]), err
}
func (r *okRunner) Run(_ context.Context, name string, args ...string) error { return r.do(name, args) }
func (r *okRunner) Exists(string) bool                                       { return true }

// emptyRepo is a backup whose snapshot has nothing in it.
type emptyRepo struct{}

func (emptyRepo) Snapshots(_ context.Context, host string) ([]engine.Snapshot, error) {
	return []engine.Snapshot{{ID: "abc123", ShortID: "abc123", Hostname: host}}, nil
}
func (emptyRepo) Run(context.Context, ...string) (engine.Result, error) { return engine.Result{}, nil }

// applyEnv lets carryon restore apply plans to a fake laptop, as root, with every command
// succeeding but the failing ones.
func applyEnv(t *testing.T, ask ui.Prompter, fail ...string) *okRunner {
	t.Helper()
	fakeRestoreEnv(t)
	planEnv := restoreEnv(paths.Dirs{})
	r := &okRunner{fail: map[string]bool{}, out: map[string]string{
		"systemctl is-enabled -- sshd.service": "disabled\n",
	}}
	for _, f := range fail {
		r.fail[f] = true
	}
	old := restoreEnv
	restoreEnv = func(paths.Dirs) restoreSystem {
		e := planEnv
		e.Run = r
		e.Installer = pacman.New(r, pacman.Options{Root: e.Root})
		e.Chown = func(string, int, int) error { return nil }
		e.TempDir = t.TempDir()
		e.Repo = func(config.Config) restore.Repo { return emptyRepo{} }
		if ask != nil {
			e.Ask = func(bool) ui.Prompter { return ask }
		}
		return e
	}
	t.Cleanup(func() { restoreEnv = old })
	oldUID := geteuid
	geteuid = func() int { return 0 }
	t.Cleanup(func() { geteuid = oldUID })
	return r
}

// smallRecord writes a record with one service and one kernel parameter.
func smallRecord(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range map[string]string{
		"meta.json":    `{"format": 2, "host": "desktop", "machine_name": "desktop", "user": "alice", "home": "/home/alice", "users": [{"name": "alice", "home": "/home/alice"}], "chassis": "desktop", "complete": true, "kernel": "6.17", "machine": ""}`,
		"bundles.json": `{"services": {"title": "Enabled system services", "when": "always"}, "kparams": {"title": "Kernel parameters", "when": "always"}}`,
		"items.jsonl": `{"bundle": "services", "id": "sshd.service", "type": "svc"}
{"bundle": "kparams", "id": "splash", "source": "/etc/kernel/cmdline", "type": "kparam"}
`,
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func restoringDirs(t *testing.T) paths.Dirs {
	t.Helper()
	dirs := paths.Dirs{State: t.TempDir(), Share: "../.."} // the catalog is in ../../catalog
	if err := os.WriteFile(dirs.RestoringFlag(), []byte("2026-10-14T09:00:00Z\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return dirs
}

func flagged(dirs paths.Dirs) bool {
	_, err := os.Lstat(dirs.RestoringFlag())
	return err == nil
}

func TestRestoreAppliesWithYes(t *testing.T) {
	r := applyEnv(t, nil)
	dirs := restoringDirs(t)
	out, err := run(t, dirs, "restore", "--defaults", "--yes", "--record", smallRecord(t))
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	for _, want := range []string{"systemctl daemon-reload", "systemctl enable -- sshd.service", "mkinitcpio -P"} {
		if !slices.Contains(r.calls, want) {
			t.Errorf("didn't run %s: %q", want, r.calls)
		}
	}
	for _, want := range []string{
		"Done. Restart the computer",
		"Automatic backups are on again.",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if flagged(dirs) {
		t.Error("the restoring flag is still there")
	}
}

func TestRestoreKeepsBackupsPausedAfterAFailure(t *testing.T) {
	applyEnv(t, nil, "systemctl enable -- sshd.service")
	dirs := restoringDirs(t)
	os.Remove(dirs.RestoringFlag())
	out, err := run(t, dirs, "restore", "--defaults", "--yes", "--record", smallRecord(t))
	if err == nil {
		t.Fatalf("no error:\n%s", out)
	}
	for _, want := range []string{
		"Finished, but these need your attention:\n  - couldn't enable sshd.service",
		"Automatic backups stay paused",
		"remove " + dirs.RestoringFlag(),
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if !flagged(dirs) {
		t.Error("automatic backups weren't paused")
	}
}

func TestRestoreOneBundle(t *testing.T) {
	r := applyEnv(t, nil)
	dirs := restoringDirs(t)
	out, err := run(t, dirs, "restore", "--yes", "--bundle", "services", "--record", smallRecord(t))
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if !strings.Contains(out, "(1/2 bundles)") || strings.Contains(out, "Kernel params") {
		t.Errorf("output:\n%s", out)
	}
	if !slices.Contains(r.calls, "systemctl enable -- sshd.service") || slices.Contains(r.calls, "mkinitcpio -P") {
		t.Errorf("ran %q", r.calls)
	}
	if !flagged(dirs) {
		t.Error("--bundle started automatic backups again")
	}
	if _, err := os.Stat(filepath.Join(dirs.State, "selections")); err == nil {
		t.Error("--bundle was remembered as a choice")
	}
	_, err = run(t, dirs, "restore", "--yes", "--bundle", "app-nothing", "--record", smallRecord(t))
	if err == nil || !strings.Contains(err.Error(), "has no bundle app-nothing") {
		t.Errorf("err = %v", err)
	}
}

func TestRestoreAsksBeforeApplying(t *testing.T) {
	ask := &ui.Script{Answers: []any{ui.UseDefault, false}}
	r := applyEnv(t, ask)
	out, err := run(t, restoringDirs(t), "restore", "--record", smallRecord(t))
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if len(ask.Asked) != 2 || ask.Asked[1].Title != "Apply this plan?" {
		t.Errorf("asked %+v", ask.Asked)
	}
	if !strings.HasSuffix(out, "Nothing was changed.\n") {
		t.Errorf("output:\n%s", out)
	}
	onlyLooked(t, r)
}

// onlyLooked fails if anything ran besides what planning reads.
func onlyLooked(t *testing.T, r *okRunner) {
	t.Helper()
	for _, c := range r.calls {
		switch c {
		case "hostnamectl chassis", "bootctl status --no-pager", "systemctl is-enabled -- sshd.service":
		default:
			t.Errorf("ran %s", c)
		}
	}
}

func TestRestoreApplyingNeedsRoot(t *testing.T) {
	r := applyEnv(t, nil)
	geteuid = func() int { return 1000 }
	_, err := run(t, restoringDirs(t), "restore", "--defaults", "--yes", "--record", smallRecord(t))
	if err == nil || !strings.Contains(err.Error(), "needs root: run it again with sudo") {
		t.Errorf("err = %v", err)
	}
	onlyLooked(t, r)
}

func TestRestoreNothingToDoStartsBackups(t *testing.T) {
	applyEnv(t, nil)
	dirs := restoringDirs(t)
	rec := smallRecord(t)
	if err := os.WriteFile(filepath.Join(rec, "items.jsonl"), []byte(`{"bundle": "kparams", "id": "quiet", "type": "kparam"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := run(t, dirs, "restore", "--defaults", "--record", rec)
	if err != nil || !strings.Contains(out, "Nothing to do") || flagged(dirs) {
		t.Errorf("%v, flag %v:\n%s", err, flagged(dirs), out)
	}
}
