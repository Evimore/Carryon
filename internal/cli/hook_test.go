package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Evimore/carryon/internal/backup"
	"github.com/Evimore/carryon/internal/catalog"
	"github.com/Evimore/carryon/internal/config"
	"github.com/Evimore/carryon/internal/dest"
	"github.com/Evimore/carryon/internal/paths"
	"github.com/Evimore/carryon/internal/pkgmgr"
	"github.com/Evimore/carryon/internal/state"
	"github.com/Evimore/carryon/internal/sysexec"
)

// tinyPM is a package manager with one package.
type tinyPM struct{ fail error }

func (p tinyPM) Name() string  { return "pacman" }
func (p tinyPM) Present() bool { return true }
func (p tinyPM) Packages(context.Context) ([]pkgmgr.Package, error) {
	return []pkgmgr.Package{{Name: "base", Repo: "core"}}, p.fail
}
func (tinyPM) Installed(context.Context) ([]string, error)          { return []string{"base"}, nil }
func (tinyPM) Versions(context.Context) ([]byte, error)             { return []byte("base 3-2\n"), nil }
func (tinyPM) SyncPackages(context.Context) ([]string, error)       { return nil, nil }
func (tinyPM) SyncRepos(context.Context) (map[string]string, error) { return nil, nil }
func (tinyPM) OwnedFiles(context.Context) ([]string, error) {
	return []string{"/etc", "/etc/passwd"}, nil
}
func (tinyPM) FilesOf(context.Context, ...string) (map[string][]string, error) { return nil, nil }
func (tinyPM) Owners(context.Context, []string) (map[string]string, error)     { return nil, nil }
func (tinyPM) ModifiedConfigs(context.Context) ([]string, error)               { return nil, nil }
func (tinyPM) EditedFiles(context.Context) ([]pkgmgr.EditedFile, error)        { return nil, nil }
func (tinyPM) ConfiguredRepos(context.Context) ([]string, error)               { return nil, nil }
func (tinyPM) ExtraRepos(context.Context) ([]pkgmgr.Repo, error)               { return nil, nil }
func (tinyPM) ExtraKeys(context.Context) ([]pkgmgr.Key, error)                 { return nil, nil }
func (tinyPM) ExportKeys(context.Context, []string) ([]byte, error)            { return nil, nil }
func (tinyPM) TrustKeys(context.Context, string, []string) ([]string, error)   { return nil, nil }
func (tinyPM) Install(context.Context, []string, pkgmgr.InstallOptions) error  { return nil }
func (tinyPM) InstallEach(context.Context, []string, bool) []string            { return nil }

type tinyFacts struct{}

func (tinyFacts) Hostname() string                            { return "desktop" }
func (tinyFacts) Fingerprint() string                         { return "0123456789abcdef" }
func (tinyFacts) Chassis(context.Context) string              { return "desktop" }
func (tinyFacts) FingerprintReaders(context.Context) []string { return nil }

// fakeMachine sets up Carryon's folders and a fake machine for the commands, as root.
func fakeMachine(t *testing.T, withConfig bool, pmErr error) paths.Dirs {
	t.Helper()
	asRoot(t, true)
	tmp := t.TempDir()
	dirs := paths.Dirs{
		Conf: filepath.Join(tmp, "conf"), State: filepath.Join(tmp, "state"), Cache: filepath.Join(tmp, "cache"),
		Run: filepath.Join(tmp, "run"), Share: filepath.Join(tmp, "share"),
	}
	root := filepath.Join(tmp, "root")
	for name, body := range map[string]string{
		"etc/passwd":         "root:x:0:0::/root:/usr/bin/bash\nalice:x:1000:1000::/home/alice:/usr/bin/bash\n",
		"etc/group":          "alice:x:1000:\n",
		"home/alice/a.txt":   "hello\n",
		"etc/hostname":       "desktop\n",
		"etc/os-release":     "ID=arch\n",
		"etc/example.conf":   "x\n",
		"root/.bash_history": "",
	} {
		writeFile(t, filepath.Join(root, name), body)
	}
	if withConfig {
		cfg := config.Default()
		cfg.MachineName = "desktop"
		cfg.Repository.URL = filepath.Join(tmp, "repo")
		if err := os.MkdirAll(dirs.Conf, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := config.Write(dirs.ConfigFile(), cfg); err != nil {
			t.Fatal(err)
		}
	}
	f := &sysexec.Fake{}
	f.On("systemctl list-unit-files --no-legend --no-pager --state=enabled,masked", sysexec.Response{})
	for _, c := range []string{"npm ls -g --depth=0 --parseable", "flatpak list --app --system --columns=application,origin",
		"flatpak list --app --user --columns=application,origin", "cargo install --list", "pipx list --short"} {
		f.On(c, sysexec.Response{Missing: true})
	}
	oldSys, oldTimers, oldHost := backupSystem, hookTimers, backupHost
	backupSystem = func(config.Config) (backup.System, error) {
		return backup.System{
			Root: root, Runner: f, Packages: tinyPM{fail: pmErr}, Facts: tinyFacts{}, IsRoot: true,
			Mounts: dest.MountTable{{ID: 1, MountPoint: "/", FSType: "ext4", Source: "/dev/sda2"}}, Catalog: &catalog.Catalog{},
		}, nil
	}
	hookTimers = func(backup.System) state.Timers { return nil }
	backupHost = func(context.Context, sysexec.Runner) hostInfo { return hostInfo{} }
	t.Cleanup(func() { backupSystem, hookTimers, backupHost = oldSys, oldTimers, oldHost })
	return dirs
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestHookRefreshesTheRecordAndNotesABackup(t *testing.T) {
	dirs := fakeMachine(t, true, nil)
	if err := state.WriteDestination(dirs, dest.Identity{MountPoint: "/", RepoID: "abc"}); err != nil {
		t.Fatal(err)
	}
	out, err := run(t, dirs, "hook")
	if err != nil {
		t.Fatalf("hook: %v\n%s", err, out)
	}
	if _, err := os.Stat(filepath.Join(dirs.RecordDir(), "items.jsonl")); err != nil {
		t.Errorf("no record: %v\n%s", err, out)
	}
	if p, ok, _ := state.ReadPending(dirs); !ok || p.Reason != state.ReasonChange {
		t.Errorf("pending = %+v %v", p, ok)
	}
	if !strings.Contains(out, "backs up these changes once nothing has changed for 1h") {
		t.Errorf("hints:\n%s", out)
	}
	runs, _ := state.ReadRuns(dirs)
	if len(runs) != 1 || runs[0].Trigger != state.TriggerHook || runs[0].Result != state.ResultOK {
		t.Errorf("runs = %+v", runs)
	}
}

// The hook exits successfully however it goes wrong.
func TestHookNeverFails(t *testing.T) {
	cases := map[string]func(t *testing.T) paths.Dirs{
		"not set up": func(t *testing.T) paths.Dirs { return fakeMachine(t, false, nil) },
		"broken config": func(t *testing.T) paths.Dirs {
			d := fakeMachine(t, false, nil)
			writeFile(t, d.ConfigFile(), "[backup\nnot toml")
			return d
		},
		"not root": func(t *testing.T) paths.Dirs {
			d := fakeMachine(t, true, nil)
			asRoot(t, false)
			return d
		},
		"no package manager": func(t *testing.T) paths.Dirs {
			d := fakeMachine(t, true, nil)
			backupSystem = func(config.Config) (backup.System, error) {
				return backup.System{}, errors.New("pacman isn't installed")
			}
			return d
		},
		"capture fails": func(t *testing.T) paths.Dirs { return fakeMachine(t, true, errors.New("database locked")) },
		"capture panics": func(t *testing.T) paths.Dirs {
			d := fakeMachine(t, true, nil)
			backupSystem = func(config.Config) (backup.System, error) {
				return backup.System{Runner: &sysexec.Fake{}, Packages: panicPM{}}, nil
			}
			return d
		},
		"state folder unwritable": func(t *testing.T) paths.Dirs {
			d := fakeMachine(t, true, nil)
			writeFile(t, d.State, "a file where the folder should be")
			return d
		},
		"capture hangs": func(t *testing.T) paths.Dirs {
			d := fakeMachine(t, true, nil)
			old := hookTimeout
			hookTimeout = 300 * time.Millisecond
			t.Cleanup(func() { hookTimeout = old })
			backupSystem = func(config.Config) (backup.System, error) {
				return backup.System{Runner: &sysexec.Fake{}, Packages: slowPM{}}, nil
			}
			return d
		},
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			dirs := setup(t)
			start := time.Now()
			out, err := run(t, dirs, "hook")
			if err != nil {
				t.Fatalf("hook returned %v\n%s", err, out)
			}
			if d := time.Since(start); d > 10*time.Second {
				t.Errorf("took %v", d)
			}
		})
	}
}

type panicPM struct{ tinyPM }

func (panicPM) Packages(context.Context) ([]pkgmgr.Package, error) { panic("nil pointer") }

type slowPM struct{ tinyPM }

func (slowPM) Packages(context.Context) ([]pkgmgr.Package, error) {
	time.Sleep(time.Hour)
	return nil, nil
}

func TestHookWaitsWhileRestoring(t *testing.T) {
	dirs := fakeMachine(t, true, nil)
	writeFile(t, dirs.RestoringFlag(), "")
	if out, err := run(t, dirs, "hook"); err != nil || out != "" {
		t.Fatalf("hook: %v %q", err, out)
	}
	if _, err := os.Stat(dirs.RecordDir()); err == nil {
		t.Error("the hook captured while restoring")
	}
}

func TestCapturePublishesTheRecord(t *testing.T) {
	dirs := fakeMachine(t, true, nil)
	out, err := run(t, dirs, "capture")
	if err != nil || !strings.Contains(out, "first record") {
		t.Fatalf("capture: %v\n%s", err, out)
	}
	out, err = run(t, dirs, "capture")
	if err != nil || !strings.Contains(out, "Nothing changed") {
		t.Fatalf("second capture: %v\n%s", err, out)
	}
	if runs, _ := state.ReadRuns(dirs); len(runs) != 2 || runs[1].Kind != state.KindCapture {
		t.Errorf("runs = %+v", runs)
	}
}

func TestBackupNeedsRoot(t *testing.T) {
	asRoot(t, false)
	_, err := run(t, paths.Dirs{Conf: t.TempDir()}, "backup")
	if err == nil || !strings.Contains(err.Error(), "sudo") {
		t.Fatalf("err = %v", err)
	}
}

func TestBackupWithoutInit(t *testing.T) {
	dirs := fakeMachine(t, true, nil)
	_, err := run(t, dirs, "backup", "--quiet")
	if err == nil || !strings.Contains(err.Error(), "carryon init") {
		t.Fatalf("err = %v", err)
	}
}

func TestSettleWithNothingOwed(t *testing.T) {
	dirs := fakeMachine(t, true, nil)
	if out, err := run(t, dirs, "settle"); err != nil || !strings.Contains(out, "No backup is owed") {
		t.Fatalf("settle: %v %q", err, out)
	}
}

func TestSettleTooSoon(t *testing.T) {
	dirs := fakeMachine(t, true, nil)
	if err := state.MarkPending(dirs, state.ReasonChange, time.Now()); err != nil {
		t.Fatal(err)
	}
	if out, err := run(t, dirs, "settle"); err != nil || !strings.Contains(out, "waits until nothing has changed for 1h") {
		t.Fatalf("settle: %v %q", err, out)
	}
}

// The hook looks for new data in the folders of the packages pacman gives it, and prints a line
// for each one it puts in the inbox. A busy inbox skips the suggestion, quietly and quickly.
func TestHookSuggestsNewDataFolders(t *testing.T) {
	for _, busy := range []bool{false, true} {
		t.Run(map[bool]string{false: "free", true: "busy"}[busy], func(t *testing.T) {
			dirs := fakeMachine(t, true, nil)
			root := filepath.Join(filepath.Dir(dirs.Conf), "root")
			writeFile(t, filepath.Join(root, "var/lib/base/state.db"), "x\n")
			writeFile(t, filepath.Join(root, "var/lib/other/state.db"), "x\n") // not base's
			writeFile(t, dirs.KnownFolders(), "/var/lib/pacman\n")
			if busy {
				held, done, finished := make(chan struct{}), make(chan struct{}), make(chan struct{})
				go func() {
					defer close(finished)
					_, _ = state.UpdateInbox(dirs, time.Second, func(*state.Inbox) error { close(held); <-done; return nil })
				}()
				<-held
				defer func() { close(done); <-finished }()
			}
			var out strings.Builder
			start := time.Now()
			runHook(context.Background(), dirs, strings.NewReader("base\n"), &out)
			if took := time.Since(start); took > hookInboxWait+5*time.Second {
				t.Errorf("took %s", took)
			}
			in, _ := state.ReadInbox(dirs)
			if busy {
				if strings.Contains(out.String(), "/var/lib/") || strings.Contains(out.String(), "couldn't look") {
					t.Errorf("hints:\n%s", out.String())
				}
				return
			}
			e, ok := in.Find("/var/lib/base")
			if !ok || len(in.Entries) != 1 || e.By != state.ByHook || e.App != "base" || e.Bundle != "app-base" {
				t.Errorf("inbox %+v", in.Entries)
			}
			if !strings.Contains(out.String(), "Carryon: /var/lib/base holds new data (probably base, 2 B). Back it up with: sudo carryon add "+e.ID) {
				t.Errorf("hints:\n%s", out.String())
			}
		})
	}
}
