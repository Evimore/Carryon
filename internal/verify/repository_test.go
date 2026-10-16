package verify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Evimore/carryon/internal/backup"
	"github.com/Evimore/carryon/internal/config"
	"github.com/Evimore/carryon/internal/dest"
	"github.com/Evimore/carryon/internal/engine"
	"github.com/Evimore/carryon/internal/setup"
	"github.com/Evimore/carryon/internal/state"
	"github.com/Evimore/carryon/internal/sysexec"
)

// fakeRestic answers restic commands by their first argument and records every command.
type fakeRestic struct {
	mu      sync.Mutex
	answers map[string]fakeAnswer
	calls   [][]string
}

type fakeAnswer struct {
	code           int
	stdout, stderr string
}

func (f *fakeRestic) run(_ context.Context, c engine.Cmd) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, c.Args)
	sub := ""
	for i, a := range c.Args {
		if a == "--retry-lock" {
			continue
		}
		if i > 0 && c.Args[i-1] == "--retry-lock" {
			continue
		}
		sub = a
		break
	}
	a, ok := f.answers[sub]
	if !ok {
		return 1, nil
	}
	io.WriteString(c.Stdout, a.stdout)
	io.WriteString(c.Stderr, a.stderr)
	return a.code, nil
}

func snapshotsJSON(t *testing.T, snaps ...engine.Snapshot) string {
	t.Helper()
	b, err := json.Marshal(snaps)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func (env *testEnv) fakeEngine(f *fakeRestic) *engine.Engine {
	e := engine.New(config.Repository{URL: "/mnt/backup/carryon", PasswordFile: filepath.Join(env.tmp, "password")}, env.dirs, "desktop")
	e.Runner = f.run
	e.BaseEnv = []string{"HOME=" + env.tmp}
	e.Stderr = io.Discard
	return e
}

func (env *testEnv) repoOptions(f *fakeRestic, check dest.Result) (Options, *fakeChecker) {
	if err := state.WriteDestination(env.dirs, dest.Identity{MountPoint: "/mnt/backup", RepoID: "abc123"}); err != nil {
		env.t.Fatal(err)
	}
	opt := env.options()
	opt.Config.Repository.URL = "/mnt/backup/carryon"
	opt.Engine = env.fakeEngine(f)
	c := &fakeChecker{res: check}
	opt.Checker = c
	return opt, c
}

func TestMismatchedDestinationRunsNoRestic(t *testing.T) {
	for _, st := range []dest.Status{dest.RepoMismatch, dest.DifferentDisk} {
		t.Run(st.String(), func(t *testing.T) {
			env := newEnv(t)
			f := &fakeRestic{}
			opt, c := env.repoOptions(f, dest.Result{Status: st, Message: "A different repository is at /mnt/backup/carryon.", Fix: "Run carryon init."})
			v := env.withItems(opt)
			v.checkRepository(context.Background())
			if c.calls != 1 {
				t.Errorf("%d destination checks, want 1", c.calls)
			}
			if len(f.calls) != 0 {
				t.Errorf("restic ran: %q", f.calls)
			}
			l := want(t, v.rep, Problem, "A different repository is at /mnt/backup/carryon. Verify didn't run restic on it.")
			if l.Fix != "Run carryon init." {
				t.Errorf("fix = %q", l.Fix)
			}
		})
	}
}

func TestNotSetUp(t *testing.T) {
	env := newEnv(t)
	f := &fakeRestic{}
	opt := env.options()
	opt.Engine, opt.Checker = env.fakeEngine(f), &fakeChecker{}
	v := env.withItems(opt)
	v.checkRepository(context.Background())
	want(t, v.rep, Problem, "aren't set up")
	if len(f.calls) != 0 {
		t.Errorf("restic ran: %q", f.calls)
	}
}

func TestDestinationAwayFallsBackOnHistory(t *testing.T) {
	tests := []struct {
		name  string
		runs  []state.Run
		level Level
		text  string
	}{
		{name: "recent backup", runs: []state.Run{{Kind: state.KindBackup, Result: state.ResultOK, End: time.Date(2026, 9, 30, 3, 0, 0, 0, time.UTC)}},
			level: OK, text: "The last successful backup ended"},
		{name: "old backup", runs: []state.Run{
			{Kind: state.KindBackup, Result: state.ResultOK, End: time.Date(2026, 9, 20, 3, 0, 0, 0, time.UTC)},
			{Kind: state.KindBackup, Result: state.ResultFailed, End: time.Date(2026, 9, 30, 3, 0, 0, 0, time.UTC)},
			{Kind: state.KindVerify, Result: state.ResultOK, End: time.Date(2026, 9, 30, 4, 0, 0, 0, time.UTC)},
		}, level: Problem, text: "older than verify.max_age_hours"},
		{name: "no backup", level: Warning, text: "no successful backup"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := newEnv(t)
			for _, r := range tt.runs {
				if err := state.AppendRun(env.dirs, r); err != nil {
					t.Fatal(err)
				}
			}
			f := &fakeRestic{}
			opt, _ := env.repoOptions(f, dest.Result{Status: dest.NotMounted, Message: "No drive is mounted at /mnt/backup/carryon.", Fix: "Plug in the backup drive."})
			v := env.withItems(opt)
			v.checkRepository(context.Background())
			want(t, v.rep, Warning, "The repository couldn't be checked: No drive is mounted")
			want(t, v.rep, tt.level, tt.text)
			if len(f.calls) != 0 {
				t.Errorf("restic ran: %q", f.calls)
			}
		})
	}
}

func TestQuickRepositoryChecks(t *testing.T) {
	recent := engine.Snapshot{ID: "aaaa1111", Time: time.Date(2026, 10, 1, 2, 0, 0, 0, time.UTC), Hostname: "desktop"}
	old := engine.Snapshot{ID: "bbbb2222", Time: time.Date(2026, 9, 20, 2, 0, 0, 0, time.UTC), Hostname: "desktop"}
	laptop := engine.Snapshot{ID: "cccc3333", Time: time.Date(2026, 9, 30, 2, 0, 0, 0, time.UTC), Hostname: "laptop"}
	tests := []struct {
		name   string
		answer fakeAnswer
		kit    bool
		level  Level
		text   string
	}{
		{name: "opens", answer: fakeAnswer{stdout: snapshotsJSON(t, old, recent)}, level: OK, text: "opens with the saved password"},
		{name: "recent", answer: fakeAnswer{stdout: snapshotsJSON(t, old, recent)}, level: OK, text: "2 backups kept"},
		{name: "too old", answer: fakeAnswer{stdout: snapshotsJSON(t, old, laptop)}, level: Problem, text: "older than verify.max_age_hours"},
		{name: "other machines", answer: fakeAnswer{stdout: snapshotsJSON(t, recent, laptop)}, level: Info, text: "Other machines in this repository: laptop."},
		{name: "none of this machine", answer: fakeAnswer{stdout: snapshotsJSON(t, laptop)}, level: Problem, text: "no backups of desktop yet"},
		{name: "wrong password", answer: fakeAnswer{code: 12, stderr: "Fatal: wrong password or no key found\n"}, level: Problem, text: "can't be opened"},
		{name: "kit not confirmed", answer: fakeAnswer{stdout: snapshotsJSON(t, recent)}, level: Warning, text: "recovery kit isn't confirmed"},
		{name: "kit confirmed", answer: fakeAnswer{stdout: snapshotsJSON(t, recent)}, kit: true, level: OK, text: "recovery kit is saved outside this machine (2026-09-01)"},
		{name: "quick", answer: fakeAnswer{stdout: snapshotsJSON(t, recent)}, level: Info, text: "This was the quick check"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := newEnv(t)
			if tt.kit {
				write(t, setup.KitConfirmedFile(env.dirs), "2026-09-01T10:00:00Z\n")
			}
			f := &fakeRestic{answers: map[string]fakeAnswer{"snapshots": tt.answer}}
			opt, _ := env.repoOptions(f, dest.Result{Status: dest.OK})
			v := env.withItems(opt)
			v.checkRepository(context.Background())
			want(t, v.rep, tt.level, tt.text)
			for _, c := range f.calls {
				if !slices.Contains(c, "--no-lock") {
					t.Errorf("restic %q runs without --no-lock", c)
				}
			}
		})
	}
}

// realEnv is env with a local restic repository and one backup of the fake machine in it.
func (env *testEnv) realRepository() (*engine.Engine, *bytes.Buffer) {
	t := env.t
	if _, err := exec.LookPath("restic"); err != nil {
		t.Skip("restic isn't installed; the deep check tests need it")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git isn't installed; the backup the deep check reads needs it")
	}
	pw := filepath.Join(env.tmp, "password")
	write(t, pw, "correct horse battery staple\n")
	e := engine.New(config.Repository{URL: filepath.Join(env.tmp, "repo"), PasswordFile: pw}, env.dirs, "desktop")
	e.BaseEnv = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + env.tmp}
	stderr := &bytes.Buffer{}
	e.Stderr = &lockedWriter{w: stderr}
	ctx := context.Background()
	if r, err := e.Run(ctx, "init"); err != nil || r.Err() != nil {
		t.Fatalf("restic init: %v %v\n%s", err, r.Err(), r.Stderr)
	}
	id, err := e.RepositoryID(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := state.WriteDestination(env.dirs, dest.Identity{MountPoint: "/", RepoID: id}); err != nil {
		t.Fatal(err)
	}
	cfg := env.config()
	cfg.Repository = e.Repository()
	_, err = backup.Run(ctx, backup.Options{
		Dirs: env.dirs, Config: cfg, System: env.system(), Engine: e,
		Checker: &fakeChecker{res: dest.Result{Status: dest.OK}}, Machine: "desktop",
		Trigger: state.TriggerManual, Git: sysexec.New(), Log: env.log, Now: func() time.Time { return env.now },
	})
	if err != nil {
		t.Fatalf("backup: %v\n%s\n%s", err, env.log.b.String(), stderr)
	}
	return e, stderr
}

// recordingRunner runs restic for real and records each command.
type recordingRunner struct {
	mu    sync.Mutex
	calls [][]string
}

func (r *recordingRunner) run(ctx context.Context, c engine.Cmd) (int, error) {
	r.mu.Lock()
	r.calls = append(r.calls, c.Args)
	r.mu.Unlock()
	return engine.ExecRunner(ctx, c)
}

func TestDeepCheckOnARealRepository(t *testing.T) {
	for _, full := range []bool{false, true} {
		t.Run(fmt.Sprintf("full=%v", full), func(t *testing.T) {
			env := newEnv(t)
			e, stderr := env.realRepository()
			write(t, setup.KitConfirmedFile(env.dirs), "2026-09-01T10:00:00Z\n")
			rec := &recordingRunner{}
			e.Runner = rec.run
			opt := env.options()
			opt.Config.Repository = e.Repository()
			opt.Engine = e
			checker := &fakeChecker{res: dest.Result{Status: dest.OK}}
			opt.Checker = checker
			opt.Deep, opt.Full = true, full
			rep, err := Run(context.Background(), opt)
			if err != nil {
				t.Fatal(err)
			}
			if !rep.OK() || rep.Warnings != 0 {
				t.Fatalf("report:\n%s\nrestic:\n%s", levels(rep), stderr)
			}
			want(t, rep, OK, "The newest backup holds the record")
			if full {
				want(t, rep, OK, "restic re-read all of the data")
			} else {
				want(t, rep, OK, "restic re-read 2% of the data")
			}
			want(t, rep, OK, "the restored record matches the one on disk")
			if checker.calls != 1 {
				t.Errorf("%d destination checks", checker.calls)
			}
			for _, c := range rec.calls {
				if slices.Contains(c, "check") {
					continue // restic check takes the lock it needs
				}
				if !slices.Contains(c, "--no-lock") {
					t.Errorf("restic %q runs without --no-lock", c)
				}
			}

			runs, err := state.ReadRuns(env.dirs)
			if err != nil {
				t.Fatal(err)
			}
			last := runs[len(runs)-1]
			if last.Kind != state.KindVerify || last.Result != state.ResultOK || last.Trigger != state.TriggerManual {
				t.Errorf("last run = %+v", last)
			}
			st, err := state.ReadStatus(env.dirs)
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, r := range st.Recent {
				found = found || r.Kind == state.KindVerify
			}
			if st.LastCapture == nil || !st.LastCapture.Equal(env.now) {
				t.Errorf("last capture = %v, want the backup's %v", st.LastCapture, env.now)
			}
			if !found {
				t.Errorf("status.json has no verify run: %+v", st.Recent)
			}
		})
	}
}

func TestDeepCheckFindsMissingPaths(t *testing.T) {
	env := newEnv(t)
	e, _ := env.realRepository()
	// A folder added to backup.extra_paths after the newest backup isn't in it.
	write(t, filepath.Join(env.root, "srv/www/index.html"), "<p>hello</p>\n")
	opt := env.options()
	opt.Config.Repository = e.Repository()
	opt.Engine = e
	opt.Checker = &fakeChecker{res: dest.Result{Status: dest.OK}}
	opt.Deep = true
	v := env.verifier(opt)
	snaps, err := e.Snapshots(context.Background(), "desktop")
	if err != nil || len(snaps) != 1 {
		t.Fatalf("snapshots = %v, %v", snaps, err)
	}
	for _, tt := range []struct {
		captured time.Time
		level    Level
	}{
		{snaps[0].Time.Add(-time.Minute), Problem},
		// The record changed after the backup: the next backup picks up what it added.
		{snaps[0].Time.Add(time.Minute), Warning},
	} {
		env.setCaptured(tt.captured)
		v.rep = &Report{}
		v.checkContents(context.Background(), snaps[0])
		l := want(t, v.rep, tt.level, "missing some of what it should hold")
		if strings.Join(l.Details, ",") != filepath.Join(env.root, "srv") {
			t.Errorf("details = %q", l.Details)
		}
	}
}

func TestFailedRunIsRecorded(t *testing.T) {
	env := newEnv(t)
	env.capture()
	f := &fakeRestic{answers: map[string]fakeAnswer{"snapshots": {code: 12, stderr: "Fatal: wrong password or no key found\n"}}}
	opt, _ := env.repoOptions(f, dest.Result{Status: dest.OK})
	opt.Unit, opt.Trigger = "carryon-verify.service", state.TriggerTimer
	rep, err := Run(context.Background(), opt)
	if err != nil {
		t.Fatal(err)
	}
	if rep.OK() {
		t.Fatalf("report:\n%s", levels(rep))
	}
	runs, _ := state.ReadRuns(env.dirs)
	last := runs[len(runs)-1]
	if last.Kind != state.KindVerify || last.Result != state.ResultFailed || last.Trigger != state.TriggerTimer ||
		last.Unit != "carryon-verify.service" || !strings.Contains(last.Error, "can't be opened") {
		t.Errorf("last run = %+v", last)
	}
	if len(last.Warnings) == 0 {
		t.Errorf("no warnings recorded")
	}
}

// lockedWriter serializes writes from restic's output goroutines.
type lockedWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}
