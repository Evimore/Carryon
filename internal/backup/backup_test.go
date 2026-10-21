package backup

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Evimore/carryon/internal/catalog"
	"github.com/Evimore/carryon/internal/config"
	"github.com/Evimore/carryon/internal/dest"
	"github.com/Evimore/carryon/internal/engine"
	"github.com/Evimore/carryon/internal/paths"
	"github.com/Evimore/carryon/internal/pkgmgr"
	"github.com/Evimore/carryon/internal/state"
	"github.com/Evimore/carryon/internal/sysexec"
)

// fakePM is a package manager with a handful of packages.
type fakePM struct{ pkgs []pkgmgr.Package }

var _ pkgmgr.Manager = (*fakePM)(nil)

func (f *fakePM) Name() string                                         { return "pacman" }
func (f *fakePM) Present() bool                                        { return true }
func (f *fakePM) Packages(context.Context) ([]pkgmgr.Package, error)   { return f.pkgs, nil }
func (f *fakePM) Installed(context.Context) ([]string, error)          { return nil, nil }
func (f *fakePM) Versions(context.Context) ([]byte, error)             { return []byte("base 3-2\n"), nil }
func (f *fakePM) SyncPackages(context.Context) ([]string, error)       { return nil, nil }
func (f *fakePM) SyncRepos(context.Context) (map[string]string, error) { return nil, nil }
func (f *fakePM) OwnedFiles(context.Context) ([]string, error) {
	return []string{"/etc", "/etc/passwd", "/etc/group", "/etc/hostname", "/etc/os-release"}, nil
}
func (f *fakePM) FilesOf(context.Context, ...string) (map[string][]string, error) { return nil, nil }
func (f *fakePM) Owners(context.Context, []string) (map[string]string, error) {
	return map[string]string{}, nil
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

// system is a small made-up machine: one user, alice, and a few settings.
var system = map[string]string{
	"etc/passwd":                    "root:x:0:0::/root:/usr/bin/bash\nalice:x:1000:1000:Alice:/home/alice:/usr/bin/bash\n",
	"etc/group":                     "root:x:0:root\nalice:x:1000:\n",
	"etc/hostname":                  "desktop\n",
	"etc/os-release":                "ID=arch\n",
	"etc/example.conf":              "added by hand\n",
	"root/.bashrc":                  "alias ll='ls -l'\n",
	"srv/www/index.html":            "<p>hello</p>\n",
	"home/alice/notes.txt":          "remember the milk\n",
	"home/alice/scratch.tmp":        "left out by the default excludes\n",
	"home/alice/.config/app/a.conf": "colour = blue\n",
}

// testEnv is a fake machine, Carryon's folders and a local restic repository, all in temp folders.
type testEnv struct {
	t       *testing.T
	root    string
	dirs    paths.Dirs
	e       *engine.Engine
	checker *fakeChecker
	repoID  string
	stderr  *bytes.Buffer
	log     *testLog
	now     time.Time
}

func needRestic(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("restic"); err != nil {
		t.Skip("restic isn't installed; the backup tests need it")
	}
}

func newEnv(t *testing.T) *testEnv {
	t.Helper()
	needRestic(t)
	tmp := t.TempDir()
	root := filepath.Join(tmp, "root")
	for name, body := range system {
		write(t, filepath.Join(root, name), body)
	}
	dirs := paths.Dirs{
		Conf: filepath.Join(tmp, "etc-carryon"), State: filepath.Join(tmp, "state"),
		Cache: filepath.Join(tmp, "cache"), Run: filepath.Join(tmp, "run"), Share: filepath.Join(tmp, "share"),
		Motd: filepath.Join(tmp, "motd.d"),
	}
	write(t, dirs.DefaultExcludes(), "*.tmp\n")
	write(t, dirs.KnownFolders(), "/var/lib/pacman\n")
	pw := filepath.Join(tmp, "password")
	write(t, pw, "correct horse battery staple\n")
	e := engine.New(config.Repository{URL: filepath.Join(tmp, "repo"), PasswordFile: pw}, dirs, "desktop")
	e.BaseEnv = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + tmp}
	env := &testEnv{t: t, root: root, dirs: dirs, e: e, stderr: &bytes.Buffer{}, log: &testLog{},
		now: time.Date(2026, 10, 1, 3, 0, 0, 0, time.UTC)}
	e.Stderr = &lockedWriter{w: env.stderr}
	r, err := e.Run(context.Background(), "init")
	if err != nil || r.Err() != nil {
		t.Fatalf("restic init: %v %v\n%s", err, r.Err(), r.Stderr)
	}
	env.repoID, err = e.RepositoryID(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := state.WriteDestination(dirs, dest.Identity{MountPoint: "/", RepoID: env.repoID}); err != nil {
		t.Fatal(err)
	}
	env.checker = &fakeChecker{res: dest.Result{Status: dest.OK}}
	return env
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

// runner answers the programs capture runs on the fake machine.
func runner() *sysexec.Fake {
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
	cfg.Repository = env.e.Repository()
	cfg.Backup.ExtraPaths = []string{"/root", "/srv", "/var/lib/missing"}
	return cfg
}

func (env *testEnv) options() Options {
	cfg := env.config()
	return Options{
		Dirs:   env.dirs,
		Config: cfg,
		System: System{
			Root:     env.root,
			Runner:   runner(),
			Packages: &fakePM{pkgs: []pkgmgr.Package{{Name: "base", Repo: "core"}, {Name: "htop", Repo: "extra"}}},
			Facts:    fakeFacts{},
			IsRoot:   true,
			Mounts:   dest.MountTable{{ID: 1, MountPoint: "/", FSType: "ext4", Source: "/dev/sda2"}},
			Catalog:  &catalog.Catalog{},
		},
		Engine:  env.e,
		Checker: env.checker,
		Machine: "desktop",
		Trigger: state.TriggerManual,
		Git:     sysexec.New(),
		Log:     env.log,
		Now:     func() time.Time { return env.now },
	}
}

// snapshots lists the repository's snapshots.
func (env *testEnv) snapshots() []engine.Snapshot {
	env.t.Helper()
	snaps, err := env.e.Snapshots(context.Background(), "")
	if err != nil {
		env.t.Fatal(err)
	}
	return snaps
}

// files lists the paths in a snapshot.
func (env *testEnv) files(id string) []string {
	env.t.Helper()
	nodes, err := env.e.Ls(context.Background(), id)
	if err != nil {
		env.t.Fatal(err)
	}
	var out []string
	for _, n := range nodes {
		out = append(out, n.Path)
	}
	return out
}

func TestBackupEndToEnd(t *testing.T) {
	env := newEnv(t)
	ctx := context.Background()
	if err := state.MarkPending(env.dirs, state.ReasonChange, env.now.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}

	res, err := Run(ctx, env.options())
	if err != nil {
		t.Fatalf("backup: %v\nlog:\n%s\nrestic:\n%s", err, env.log, env.stderr)
	}
	if res.Run.Result != state.ResultOK && res.Run.Result != state.ResultWarning {
		t.Fatalf("result %q: %v", res.Run.Result, res.Run.Warnings)
	}
	if !res.Captured.Published || !res.Captured.Publish.First || !res.Captured.Publish.Committed {
		t.Errorf("the first record wasn't published and committed: %+v", res.Captured.Publish)
	}
	snaps := env.snapshots()
	if len(snaps) != 1 {
		t.Fatalf("%d snapshots, want 1", len(snaps))
	}
	s := snaps[0]
	if s.Hostname != "desktop" || !slices.Contains(s.Tags, Tag) || !strings.HasPrefix(s.ID, res.Run.Snapshot) || res.Run.Snapshot == "" {
		t.Errorf("snapshot %+v, run snapshot %q", s, res.Run.Snapshot)
	}
	files := env.files(s.ID)
	for _, want := range []string{
		"/etc/example.conf", "/root/.bashrc", "/srv/www/index.html", "/home/alice/notes.txt",
		"/home/alice/.config/app/a.conf",
	} {
		if !slices.Contains(files, filepath.Join(env.root, want)) {
			t.Errorf("%s isn't in the snapshot", want)
		}
	}
	if slices.Contains(files, filepath.Join(env.root, "home/alice/scratch.tmp")) {
		t.Error("an excluded file is in the snapshot")
	}
	recordItems := filepath.Join(env.dirs.RecordDir(), "items.jsonl")
	if !slices.Contains(files, recordItems) {
		t.Errorf("the record isn't in the snapshot (want %s)", recordItems)
	}
	if _, ok, _ := state.ReadPending(env.dirs); ok {
		t.Error("the pending marker is still there after a backup")
	}
	if _, ok := state.LastPrune(env.dirs); !ok {
		t.Error("the first backup didn't start the prune clock")
	}

	// A second run with nothing changed makes a snapshot but no record commit.
	env.now = env.now.Add(24 * time.Hour)
	res2, err := Run(ctx, env.options())
	if err != nil {
		t.Fatalf("second backup: %v\n%s", err, env.stderr)
	}
	if res2.Captured.Publish.Committed || res2.Run.Changes != 0 {
		t.Errorf("the second run changed the history: %+v", res2.Captured.Publish)
	}
	if n := len(env.snapshots()); n != 2 {
		t.Errorf("%d snapshots after two runs, want 2", n)
	}
	out, err := exec.Command("git", "-C", env.dirs.RecordDir(), "rev-list", "--count", "HEAD").Output()
	if err != nil || strings.TrimSpace(string(out)) != "1" {
		t.Errorf("record history has %q commits (%v), want 1", out, err)
	}

	// The history and the status say how it went.
	runs, err := state.ReadRuns(env.dirs)
	if err != nil || len(runs) != 2 {
		t.Fatalf("runs = %d, %v", len(runs), err)
	}
	st, err := state.ReadStatus(env.dirs)
	if err != nil {
		t.Fatal(err)
	}
	if st.LastSuccess == nil || st.LastSuccess.Snapshot != res2.Run.Snapshot || len(st.Recent) != 2 {
		t.Errorf("status = %+v", st)
	}
	for _, f := range []string{state.RunStatusFile(env.dirs), env.dirs.StatusFile()} {
		fi, err := os.Stat(f)
		if err != nil || fi.Mode().Perm() != 0o644 {
			t.Errorf("%s: %v %v, want readable by everyone", f, fi, err)
		}
		data, _ := os.ReadFile(f)
		if strings.Contains(string(data), "correct horse") || strings.Contains(string(data), env.e.Repository().PasswordFile) {
			t.Errorf("%s holds a secret:\n%s", f, data)
		}
	}
}

func TestMismatchedDestinationWritesNothing(t *testing.T) {
	env := newEnv(t)
	for _, tc := range []struct {
		name    string
		status  dest.Status
		waiting bool
	}{
		{"not mounted", dest.NotMounted, true},
		{"unreachable", dest.Unreachable, true},
		{"different disk", dest.DifferentDisk, false},
		{"different repository", dest.RepoMismatch, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env.checker.res = dest.Result{Status: tc.status, Message: "The backup drive isn't the one set up.", Fix: "Plug it in."}
			res, err := Run(context.Background(), env.options())
			if tc.waiting {
				if !errors.Is(err, ErrWaiting) || res.Run.Result != state.ResultWaiting {
					t.Fatalf("err = %v, result %q; want waiting", err, res.Run.Result)
				}
				if p, ok, _ := state.ReadPending(env.dirs); !ok || p.Reason != state.ReasonWaiting {
					t.Errorf("pending = %+v, %v; want waiting", p, ok)
				}
			} else {
				var p *Problem
				if !errors.As(err, &p) || res.Run.Result != state.ResultFailed {
					t.Fatalf("err = %v, result %q; want a failure", err, res.Run.Result)
				}
			}
			if n := len(env.snapshots()); n != 0 {
				t.Errorf("%d snapshots written to a destination that didn't match", n)
			}
			if _, err := os.Stat(env.dirs.RecordDir()); err == nil {
				t.Error("the record was captured although the destination didn't match")
			}
		})
	}
}

// A real check of a local repository whose ID doesn't match what init recorded.
func TestRealCheckerRefusesAnotherRepository(t *testing.T) {
	env := newEnv(t)
	repo := env.e.Repository().URL
	mounts, err := dest.ReadMounts("/")
	if err != nil {
		t.Skip("no mount table here:", err)
	}
	m, ok := mounts.Containing(repo)
	if !ok {
		t.Skip("the temp folder is on no mount")
	}
	if err := state.WriteDestination(env.dirs, dest.Identity{MountPoint: m.MountPoint, RepoID: strings.Repeat("0", 64)}); err != nil {
		t.Fatal(err)
	}
	opt := env.options()
	opt.Checker = dest.Checker{Root: "/", Repo: env.e}
	res, err := Run(context.Background(), opt)
	var p *Problem
	if !errors.As(err, &p) || !strings.Contains(p.Cause, "isn't the one Carryon set up") {
		t.Fatalf("err = %v (result %q)", err, res.Run.Result)
	}
	if n := len(env.snapshots()); n != 0 {
		t.Errorf("%d snapshots written to the wrong repository", n)
	}
}

func TestNotSetUp(t *testing.T) {
	env := newEnv(t)
	os.Remove(state.DestinationPath(env.dirs))
	_, err := Run(context.Background(), env.options())
	if err == nil || !strings.Contains(err.Error(), "carryon init") {
		t.Fatalf("err = %v", err)
	}
	if env.checker.calls != 0 || len(env.snapshots()) != 0 {
		t.Error("a backup ran without a recorded destination")
	}
}

func TestPausedWhileRestoring(t *testing.T) {
	env := newEnv(t)
	write(t, env.dirs.RestoringFlag(), "")
	res, err := Run(context.Background(), env.options())
	if err != nil || res.Run.Result != state.ResultPaused {
		t.Fatalf("err = %v, result %q", err, res.Run.Result)
	}
	if env.checker.calls != 0 || len(env.snapshots()) != 0 {
		t.Error("a backup ran while restoring")
	}
	opt := env.options()
	opt.Force = true
	if res, err := Run(context.Background(), opt); err != nil || !res.Run.Succeeded() {
		t.Fatalf("--force: err = %v, result %q", err, res.Run.Result)
	}
}

func TestOneBackupAtATime(t *testing.T) {
	env := newEnv(t)
	unlock, err := state.Lock(env.dirs)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	if _, err := Run(context.Background(), env.options()); !errors.Is(err, state.ErrBusy) {
		t.Fatalf("err = %v, want ErrBusy", err)
	}
	if runs, _ := state.ReadRuns(env.dirs); len(runs) != 0 {
		t.Error("a skipped run was recorded")
	}
}

func TestFailedCaptureStillBacksUpFiles(t *testing.T) {
	env := newEnv(t)
	opt := env.options()
	opt.System.Packages = &brokenPM{}
	res, err := Run(context.Background(), opt)
	if err != nil {
		t.Fatalf("err = %v\n%s", err, env.stderr)
	}
	if res.Run.Result != state.ResultWarning || !strings.Contains(strings.Join(res.Run.Warnings, "\n"), "couldn't be refreshed") {
		t.Errorf("result %q, warnings %q", res.Run.Result, res.Run.Warnings)
	}
	snaps := env.snapshots()
	if len(snaps) != 1 || !slices.Contains(env.files(snaps[0].ID), filepath.Join(env.root, "home/alice/notes.txt")) {
		t.Error("the files weren't backed up")
	}
}

type brokenPM struct{ fakePM }

func (*brokenPM) Packages(context.Context) ([]pkgmgr.Package, error) {
	return nil, errors.New("the package database is locked")
}

func TestKeepArgs(t *testing.T) {
	got := keepArgs(config.Retention{KeepWithin: "2d", KeepDaily: 7, KeepMonthly: 6})
	want := []string{"--keep-within", "2d", "--keep-daily", "7", "--keep-monthly", "6"}
	if !slices.Equal(got, want) {
		t.Errorf("keepArgs = %q, want %q", got, want)
	}
	if got := keepArgs(config.Retention{}); got != nil {
		t.Errorf("no retention: %q", got)
	}
}

func TestPruneEveryFewDays(t *testing.T) {
	env := newEnv(t)
	opt := env.options()
	if _, err := Run(context.Background(), opt); err != nil {
		t.Fatal(err)
	}
	first, _ := state.LastPrune(env.dirs)
	env.now = env.now.Add(8 * 24 * time.Hour)
	if _, err := Run(context.Background(), env.options()); err != nil {
		t.Fatal(err)
	}
	if last, _ := state.LastPrune(env.dirs); !last.After(first) {
		t.Errorf("no prune after 8 days: %v then %v", first, last)
	}
	if !strings.Contains(env.log.String(), "Pruning") {
		t.Errorf("log doesn't mention pruning:\n%s", env.log)
	}
}

func TestInhibitorWrapsRestic(t *testing.T) {
	var got engine.Cmd
	r := inhibited(func(_ context.Context, c engine.Cmd) (int, error) { got = c; return 0, nil }, "/usr/bin/systemd-inhibit")
	if _, err := r(context.Background(), engine.Cmd{Path: "restic", Args: []string{"backup", "/etc"}}); err != nil {
		t.Fatal(err)
	}
	want := []string{"--what=sleep", "--who=Carryon", "--why=Backing up", "--mode=block", "--", "restic", "backup", "/etc"}
	if got.Path != "/usr/bin/systemd-inhibit" || !slices.Equal(got.Args, want) {
		t.Errorf("ran %s %q", got.Path, got.Args)
	}
}

func TestProgressWriter(t *testing.T) {
	var out bytes.Buffer
	log := &testLog{}
	p := newProgressWriter(&out, time.Minute, log)
	clock := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	p.now = func() time.Time { return clock }
	lines := []string{
		`{"message_type":"status","percent_done":0.5,"total_files":10,"files_done":5,"total_bytes":2048,"bytes_done":1024}`,
		`{"message_type":"status","percent_done":0.6,"total_files":10,"files_done":6,"total_bytes":2048,"bytes_done":1200}`,
		`{"message_type":"error","error":{"message":"permission denied"},"during":"archival","item":"/etc/secret"}`,
		`{"message_type":"summary","files_new":3,"snapshot_id":"abcdef0123456789"}`,
	}
	for _, l := range lines {
		// split writes, as a pipe may deliver them
		p.Write([]byte(l[:10]))
		p.Write([]byte(l[10:] + "\n"))
	}
	p.finish()
	if got := out.String(); got != " 50% done: 1.0 KiB of 2.0 KiB, 5 of 10 files\n" {
		t.Errorf("progress = %q", got)
	}
	o := p.output()
	if o.Summary == nil || o.Summary.SnapshotID != "abcdef0123456789" || len(o.Errors) != 1 {
		t.Errorf("output = %+v", o)
	}
	if !strings.Contains(log.String(), "/etc/secret: permission denied") {
		t.Errorf("log = %q", log)
	}
}

func TestStatusJSONIsValid(t *testing.T) {
	env := newEnv(t)
	if _, err := Run(context.Background(), env.options()); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(env.dirs.StatusFile())
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"format", "updated", "last_backup", "last_success", "recent", "inbox"} {
		if _, ok := m[k]; !ok {
			t.Errorf("status.json has no %q:\n%s", k, data)
		}
	}
}

// testLog collects log lines.
type testLog struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *testLog) Info(format string, a ...any) { l.add("info: ", format, a...) }
func (l *testLog) Warn(format string, a ...any) { l.add("warn: ", format, a...) }
func (l *testLog) add(prefix, format string, a ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.b.WriteString(prefix)
	l.b.WriteString(strings.TrimSpace(sprintf(format, a...)))
	l.b.WriteString("\n")
}
func (l *testLog) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

type lockedWriter struct {
	mu sync.Mutex
	w  *bytes.Buffer
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

func sprintf(format string, a ...any) string { return fmt.Sprintf(format, a...) }

func TestCaptureUsesBundleRules(t *testing.T) {
	env := newEnv(t)
	write(t, env.dirs.BundlesFile(), "put notes path /home/alice/notes.txt\ntitle notes Alice's notes\nnonsense line\n")
	opt := env.options()
	res, err := Capture(context.Background(), CaptureOptions{
		Dirs: env.dirs, Config: opt.Config, System: opt.System, Git: opt.Git, Log: env.log, Now: opt.Now,
	})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, it := range res.Result.Items {
		if it.ID == "/home/alice/notes.txt" {
			found = it.Bundle == "notes"
		}
	}
	if !found {
		t.Error("the put rule from bundles.conf wasn't applied")
	}
	if !strings.Contains(strings.Join(res.Warnings, "\n"), "bundles.conf") {
		t.Errorf("the unreadable line wasn't reported: %q", res.Warnings)
	}
}
