package verify

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Evimore/carryon/internal/backup"
	"github.com/Evimore/carryon/internal/catalog"
	"github.com/Evimore/carryon/internal/config"
	"github.com/Evimore/carryon/internal/dest"
	"github.com/Evimore/carryon/internal/paths"
	"github.com/Evimore/carryon/internal/pkgmgr"
	"github.com/Evimore/carryon/internal/record"
	"github.com/Evimore/carryon/internal/sysexec"
)

// fakePM is a package manager with a few packages, the repositories' packages and file owners.
type fakePM struct {
	pkgs    []pkgmgr.Package
	sync    []string
	syncErr error
	owners  map[string]string
}

var _ pkgmgr.Manager = (*fakePM)(nil)

func (f *fakePM) Name() string                                         { return "pacman" }
func (f *fakePM) Present() bool                                        { return true }
func (f *fakePM) Packages(context.Context) ([]pkgmgr.Package, error)   { return f.pkgs, nil }
func (f *fakePM) Installed(context.Context) ([]string, error)          { return nil, nil }
func (f *fakePM) Versions(context.Context) ([]byte, error)             { return []byte("base 3-2\n"), nil }
func (f *fakePM) SyncPackages(context.Context) ([]string, error)       { return f.sync, f.syncErr }
func (f *fakePM) SyncRepos(context.Context) (map[string]string, error) { return nil, nil }
func (f *fakePM) OwnedFiles(context.Context) ([]string, error) {
	return []string{"/etc", "/etc/passwd", "/etc/group", "/etc/hostname", "/etc/os-release"}, nil
}
func (f *fakePM) FilesOf(context.Context, ...string) (map[string][]string, error) { return nil, nil }
func (f *fakePM) Owners(_ context.Context, ps []string) (map[string]string, error) {
	out := map[string]string{}
	for _, p := range ps {
		if o, ok := f.owners[p]; ok {
			out[p] = o
		}
	}
	return out, nil
}
func (f *fakePM) ModifiedConfigs(context.Context) ([]string, error)              { return nil, nil }
func (f *fakePM) EditedFiles(context.Context) ([]pkgmgr.EditedFile, error)       { return nil, nil }
func (f *fakePM) ConfiguredRepos(context.Context) ([]string, error)              { return nil, nil }
func (f *fakePM) ExtraRepos(context.Context) ([]pkgmgr.Repo, error)              { return nil, nil }
func (f *fakePM) ExtraKeys(context.Context) ([]pkgmgr.Key, error)                { return nil, nil }
func (f *fakePM) ExportKeys(context.Context, []string) ([]byte, error)           { return nil, nil }
func (f *fakePM) TrustKeys(context.Context, string, []string) ([]string, error)  { return nil, nil }
func (f *fakePM) Install(context.Context, []string, pkgmgr.InstallOptions) error { return nil }
func (f *fakePM) InstallEach(context.Context, []string, bool) []string           { return nil }

// fakeFacts are the facts of a made-up desktop.
type fakeFacts struct{}

func (fakeFacts) Hostname() string                            { return "desktop" }
func (fakeFacts) Fingerprint() string                         { return "0123456789abcdef" }
func (fakeFacts) Chassis(context.Context) string              { return "desktop" }
func (fakeFacts) FingerprintReaders(context.Context) []string { return nil }

// fakeChecker answers every destination check the same way and counts the checks.
type fakeChecker struct {
	res   dest.Result
	mu    sync.Mutex
	calls int
}

func (c *fakeChecker) Check(context.Context, string, dest.Identity) dest.Result {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	return c.res
}

// machine is a small made-up machine: one user, alice, and a few settings.
var machine = map[string]string{
	"etc/passwd":                    "root:x:0:0::/root:/usr/bin/bash\nalice:x:1000:1000:Alice:/home/alice:/usr/bin/bash\n",
	"etc/group":                     "root:x:0:root\nalice:x:1000:\n",
	"etc/hostname":                  "desktop\n",
	"etc/os-release":                "ID=arch\n",
	"etc/fstab":                     "UUID=0000-1111 / ext4 rw 0 1\n",
	"etc/carryon/config.toml":       "machine_name = \"desktop\"\n",
	"root/.bashrc":                  "alias ll='ls -l'\n",
	"home/alice/notes.txt":          "remember the milk\n",
	"home/alice/.config/app/a.conf": "colour = blue\n",
}

// testEnv is a fake machine with its record and Carryon's folders, in temp folders.
type testEnv struct {
	t    *testing.T
	tmp  string
	root string
	dirs paths.Dirs
	pm   *fakePM
	run  *sysexec.Fake
	now  time.Time
	log  *testLog
}

func newEnv(t *testing.T) *testEnv {
	t.Helper()
	tmp := t.TempDir()
	root := filepath.Join(tmp, "root")
	for name, body := range machine {
		write(t, filepath.Join(root, name), body)
	}
	dirs := paths.Dirs{
		Conf: filepath.Join(root, "etc/carryon"), State: filepath.Join(tmp, "state"),
		Cache: filepath.Join(tmp, "cache"), Run: filepath.Join(tmp, "run"), Share: filepath.Join(tmp, "share"),
	}
	write(t, dirs.DefaultExcludes(), "*.tmp\n")
	env := &testEnv{
		t: t, tmp: tmp, root: root, dirs: dirs,
		pm: &fakePM{
			pkgs: []pkgmgr.Package{{Name: "base", Repo: "core"}, {Name: "htop", Repo: "extra"}},
			sync: []string{"base", "htop", "linux"},
		},
		run: captureRunner(),
		now: time.Date(2026, 10, 1, 3, 0, 0, 0, time.UTC),
		log: &testLog{},
	}
	return env
}

// captureRunner answers the programs a capture runs on the fake machine.
func captureRunner() *sysexec.Fake {
	f := &sysexec.Fake{}
	f.On("systemctl list-unit-files --no-legend --no-pager --state=enabled,masked", sysexec.Response{})
	f.On("npm ls -g --depth=0 --parseable", sysexec.Response{Missing: true})
	f.On("flatpak list --app --system --columns=application,origin", sysexec.Response{Missing: true})
	f.On("flatpak list --app --user --columns=application,origin", sysexec.Response{Missing: true})
	f.On("cargo install --list", sysexec.Response{Missing: true})
	f.On("pipx list --short", sysexec.Response{Missing: true})
	return f
}

func (env *testEnv) config() config.Config {
	cfg := config.Default()
	cfg.MachineName = "desktop"
	cfg.Backup.ExtraPaths = []string{"/root", "/srv"}
	return cfg
}

func (env *testEnv) system() backup.System {
	return backup.System{
		Root:     env.root,
		Runner:   env.run,
		Packages: env.pm,
		Facts:    fakeFacts{},
		IsRoot:   true,
		Mounts:   dest.MountTable{{ID: 1, MountPoint: "/", FSType: "ext4", Source: "/dev/sda2"}},
		Catalog:  &catalog.Catalog{},
	}
}

// capture writes the machine's record, as a backup would, captured at env.now.
func (env *testEnv) capture() {
	env.t.Helper()
	_, err := backup.Capture(context.Background(), backup.CaptureOptions{
		Dirs: env.dirs, Config: env.config(), System: env.system(), Out: env.dirs.RecordDir(),
		Now: func() time.Time { return env.now },
	})
	if err != nil {
		env.t.Fatal(err)
	}
	env.setCaptured(env.now)
}

func (env *testEnv) setCaptured(t time.Time) {
	write(env.t, filepath.Join(env.dirs.RecordDir(), record.LastCaptureFile), t.Format(time.RFC3339)+"\n")
}

func (env *testEnv) options() Options {
	return Options{
		Dirs:    env.dirs,
		Config:  env.config(),
		System:  env.system(),
		Machine: "desktop",
		Log:     env.log,
		Now:     func() time.Time { return env.now.Add(time.Hour) },
	}
}

// verifier returns a verifier for env, with the record loaded.
func (env *testEnv) verifier(opt Options) *verifier {
	env.t.Helper()
	v, err := newVerifier(opt)
	if err != nil {
		env.t.Fatal(err)
	}
	env.t.Cleanup(func() { os.RemoveAll(v.tmp) })
	if rec, err := record.Load(env.dirs.RecordDir()); err == nil {
		v.rec = rec
	}
	return v
}

// withItems returns a verifier whose record holds items, for one user, alice.
func (env *testEnv) withItems(opt Options, items ...record.Item) *verifier {
	v := env.verifier(opt)
	v.rec = &record.Record{
		Meta:  record.Meta{Format: record.FormatCurrent, Users: []record.User{{Name: "alice", Home: "/home/alice"}}},
		Items: items,
	}
	return v
}

func write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// testLog keeps what was logged.
type testLog struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *testLog) Info(format string, a ...any) { l.add("info: "+format, a...) }
func (l *testLog) Warn(format string, a ...any) { l.add("warn: "+format, a...) }
func (l *testLog) add(format string, a ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.b.WriteString(strings.TrimSpace(fmt.Sprintf(format, a...)) + "\n")
}

// lineAbout finds the report line whose text contains text.
func lineAbout(r *Report, text string) (Line, bool) {
	for _, s := range r.Sections {
		for _, l := range s.Lines {
			if strings.Contains(l.Text, text) {
				return l, true
			}
		}
	}
	return Line{}, false
}

// levels lists the report's lines as "level: text", for failure messages.
func levels(r *Report) string {
	var b strings.Builder
	for _, s := range r.Sections {
		b.WriteString("# " + s.Title + "\n")
		for _, l := range s.Lines {
			b.WriteString(string(l.Level) + ": " + l.Text + "\n")
			for _, d := range l.Details {
				b.WriteString("    " + d + "\n")
			}
		}
	}
	return b.String()
}

// want checks that the report has a line containing text at level.
func want(t *testing.T, r *Report, level Level, text string) Line {
	t.Helper()
	l, ok := lineAbout(r, text)
	if !ok {
		t.Fatalf("no line about %q in:\n%s", text, levels(r))
	}
	if l.Level != level {
		t.Fatalf("line %q is %s, want %s; report:\n%s", l.Text, l.Level, level, levels(r))
	}
	return l
}
