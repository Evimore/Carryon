package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Evimore/carryon/internal/dest"
	"github.com/Evimore/carryon/internal/engine"
	"github.com/Evimore/carryon/internal/paths"
	"github.com/Evimore/carryon/internal/state"
	"github.com/Evimore/carryon/internal/sysexec"
)

var updateStatus = flag.Bool("update-status", false, "rewrite the status golden files in testdata/status")

// statusTime is "now" in the status tests.
var statusTime = time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)

// useStatusClock fixes the time, the time zone and the colours for carryon status.
func useStatusClock(t *testing.T) {
	t.Helper()
	oldNow, oldLoc, oldColor := statusNow, statusLoc, statusColor
	statusNow = func() time.Time { return statusTime }
	statusLoc = time.UTC
	statusColor = func(io.Writer) bool { return false }
	t.Cleanup(func() { statusNow, statusLoc, statusColor = oldNow, oldLoc, oldColor })
}

// statusDirs is an empty machine: state, runtime and settings folders in a temp folder.
func statusDirs(t *testing.T) paths.Dirs {
	t.Helper()
	dir := t.TempDir()
	d := paths.Dirs{State: filepath.Join(dir, "state"), Run: filepath.Join(dir, "run"), Conf: filepath.Join(dir, "conf")}
	for _, p := range []string{d.State, d.Run, d.Conf} {
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return d
}

func setUpDestination(t *testing.T, d paths.Dirs, id dest.Identity) {
	t.Helper()
	if err := state.WriteDestination(d, id); err != nil {
		t.Fatal(err)
	}
}

// putStatus copies a status.json fixture to file.
func putStatus(t *testing.T, fixture, file string) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "status", fixture+".json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

// statusGolden compares got with testdata/status/name, or rewrites it with -update.
func statusGolden(t *testing.T, name, got string) {
	t.Helper()
	file := filepath.Join("testdata", "status", name)
	if *updateStatus {
		if err := os.WriteFile(file, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("%v (run go test -update-status to write it)", err)
	}
	if got != string(want) {
		t.Errorf("output differs from %s:\n%s\nwant:\n%s", file, got, want)
	}
}

func TestStatus(t *testing.T) {
	useStatusClock(t)
	asUser(t, 1000)
	tests := []struct {
		name     string
		fixture  string // status.json, or "" for none
		reboot   bool   // only the state folder's copy is there
		notSetUp bool
		setup    func(t *testing.T, d paths.Dirs)
	}{
		{name: "not-set-up", notSetUp: true},
		{name: "no-status"},
		{name: "never-run", fixture: "never-run"},
		{name: "all-fine", fixture: "all-fine"},
		{name: "after-reboot", fixture: "all-fine", reboot: true},
		{name: "failed", fixture: "failed"},
		{name: "warnings", fixture: "warnings"},
		{name: "waiting", fixture: "waiting", setup: func(t *testing.T, d paths.Dirs) {
			statusMust(t, state.MarkPending(d, state.ReasonWaiting, time.Date(2026, 10, 16, 11, 50, 1, 0, time.UTC)))
		}},
		{name: "waiting-recent", fixture: "waiting-recent", setup: func(t *testing.T, d paths.Dirs) {
			statusMust(t, state.MarkPending(d, state.ReasonWaiting, time.Date(2026, 10, 16, 9, 0, 1, 0, time.UTC)))
		}},
		{name: "paused", fixture: "paused", setup: func(t *testing.T, d paths.Dirs) {
			statusMust(t, os.WriteFile(d.RestoringFlag(), nil, 0o644))
		}},
		{name: "pending", fixture: "pending", setup: func(t *testing.T, d paths.Dirs) {
			statusMust(t, state.MarkPending(d, state.ReasonChange, time.Date(2026, 10, 16, 11, 20, 0, 0, time.UTC)))
		}},
		{name: "stale", fixture: "stale"},
		{name: "connected", setup: func(t *testing.T, d paths.Dirs) {
			// init --connect sets the restoring flag before any backup wrote status.json.
			statusMust(t, os.WriteFile(d.RestoringFlag(), nil, 0o644))
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := statusDirs(t)
			if !tt.notSetUp {
				setUpDestination(t, d, dest.Identity{RepoID: "0123abcd", MountPoint: "/mnt/backup"})
			}
			if tt.fixture != "" {
				putStatus(t, tt.fixture, d.StatusFile())
				if !tt.reboot {
					putStatus(t, tt.fixture, state.RunStatusFile(d))
				}
			}
			if tt.setup != nil {
				tt.setup(t, d)
			}
			out, err := run(t, d, "status")
			if err != nil {
				t.Fatalf("status: %v\n%s", err, out)
			}
			statusGolden(t, tt.name+".golden", strings.ReplaceAll(out, d.State, "/var/lib/carryon"))
		})
	}
}

func statusMust(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func TestStatusPrefersTheRuntimeCopy(t *testing.T) {
	useStatusClock(t)
	asUser(t, 1000)
	d := statusDirs(t)
	setUpDestination(t, d, dest.Identity{RepoID: "0123abcd", MountPoint: "/mnt/backup"})
	putStatus(t, "failed", d.StatusFile())
	putStatus(t, "all-fine", state.RunStatusFile(d))
	out, err := run(t, d, "status")
	if err != nil || !strings.HasPrefix(out, "All good") {
		t.Errorf("out = %q, err = %v", out, err)
	}
}

func TestStatusFindsAnOlderCheckInTheHistory(t *testing.T) {
	useStatusClock(t)
	asUser(t, 1000)
	d := statusDirs(t)
	setUpDestination(t, d, dest.Identity{RepoID: "0123abcd", MountPoint: "/mnt/backup"})
	putStatus(t, "stale", state.RunStatusFile(d))
	check := state.Run{Kind: "verify", Trigger: state.TriggerManual, Result: state.ResultOK,
		Start: time.Date(2026, 10, 9, 8, 0, 0, 0, time.UTC), End: time.Date(2026, 10, 9, 8, 30, 0, 0, time.UTC)}
	statusMust(t, state.AppendRun(d, check))
	out, err := run(t, d, "status")
	if err != nil || !strings.Contains(out, "Last check      ok, 2026-10-09 08:30 (7 days ago), by hand\n") {
		t.Errorf("err = %v, out:\n%s", err, out)
	}
}

func TestStatusMaxAgeFromTheConfig(t *testing.T) {
	useStatusClock(t)
	asUser(t, 1000)
	d := statusDirs(t)
	setUpDestination(t, d, dest.Identity{RepoID: "0123abcd", MountPoint: "/mnt/backup"})
	putStatus(t, "waiting", state.RunStatusFile(d))
	statusMust(t, os.WriteFile(d.ConfigFile(), []byte("[verify]\nmax_age_hours = 96\n"), 0o644))
	out, err := run(t, d, "status")
	if err != nil || !strings.HasPrefix(out, "All good: backed up 3 days ago.") {
		t.Errorf("err = %v, out:\n%s", err, out)
	}
}

func TestStatusJSON(t *testing.T) {
	useStatusClock(t)
	asUser(t, 1000)
	tests := []struct {
		name, fixture string
		notSetUp      bool
		code          int
	}{
		{name: "all-fine", fixture: "all-fine"},
		{name: "failed", fixture: "failed", code: exitAttention},
		{name: "not-set-up", notSetUp: true, code: exitAttention},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := statusDirs(t)
			if !tt.notSetUp {
				setUpDestination(t, d, dest.Identity{RepoID: "0123abcd", MountPoint: "/mnt/backup"})
			}
			if tt.fixture != "" {
				putStatus(t, tt.fixture, d.StatusFile())
			}
			var out bytes.Buffer
			root := NewRoot(&out, d)
			root.SetArgs([]string{"status", "--json"})
			root.SetErr(io.Discard)
			err := root.Execute()
			code := 0
			var exit exitError
			if errors.As(err, &exit) {
				code = exit.code
			} else if err != nil {
				t.Fatal(err)
			}
			if code != tt.code {
				t.Errorf("exit code %d, want %d", code, tt.code)
			}
			var parsed map[string]any
			if err := json.Unmarshal(out.Bytes(), &parsed); err != nil {
				t.Fatalf("not JSON: %v\n%s", err, out.String())
			}
			statusGolden(t, tt.name+".json.golden", out.String())
		})
	}
}

// statusRestic plays restic: `cat config` answers with id and `snapshots --json` with snaps. It
// records every command line.
type statusRestic struct {
	id    string
	snaps string
	calls [][]string
}

func (f *statusRestic) run(_ context.Context, c engine.Cmd) (int, error) {
	f.calls = append(f.calls, c.Args)
	switch {
	case slices.Contains(c.Args, "cat"):
		io.WriteString(c.Stdout, `{"version":2,"id":"`+f.id+`","chunker_polynomial":"3dea92648f6e83"}`)
	case slices.Contains(c.Args, "snapshots"):
		io.WriteString(c.Stdout, f.snaps)
	default:
		return 1, nil
	}
	return 0, nil
}

const statusSnapshotList = `[
 {"id":"aaaa1111","time":"2026-09-01T09:00:00Z","hostname":"desktop","paths":["/etc"]},
 {"id":"bbbb2222","time":"2026-10-02T09:00:00Z","hostname":"laptop","paths":["/etc"]},
 {"id":"cccc3333","time":"2026-10-16T09:04:00Z","hostname":"desktop","paths":["/etc"]}
]`

func useStatusRestic(t *testing.T, f *statusRestic) {
	t.Helper()
	oldRunner, oldRclone := statusRunner, statusRclone
	statusRunner = f.run
	statusRclone = &sysexec.Fake{}
	t.Cleanup(func() { statusRunner, statusRclone = oldRunner, oldRclone })
}

func TestStatusSnapshots(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git isn't installed")
	}
	useStatusClock(t)
	useHistoryGit(t, sysexec.New())
	asUser(t, 0)
	tests := []struct {
		name, repoID string
		wantCalls    int
	}{
		{name: "snapshots", repoID: "0123abcd", wantCalls: 2},
		// A different repository at the destination: the check stops before listing anything.
		{name: "snapshots-mismatch", repoID: "ffff9999", wantCalls: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := statusDirs(t)
			publishTwice(t, d)
			statusMust(t, os.WriteFile(d.ConfigFile(), []byte("machine_name = \"desktop\"\n[repository]\nurl = \"sftp:alice@example.com:/backups\"\n"), 0o644))
			setUpDestination(t, d, dest.Identity{RepoID: "0123abcd"})
			putStatus(t, "all-fine", state.RunStatusFile(d))
			f := &statusRestic{id: tt.repoID, snaps: statusSnapshotList}
			useStatusRestic(t, f)
			out, err := run(t, d, "status", "--snapshots")
			if err != nil {
				t.Fatalf("status: %v\n%s", err, out)
			}
			statusGolden(t, tt.name+".golden", out)
			if len(f.calls) != tt.wantCalls {
				t.Fatalf("restic ran %d times, want %d: %q", len(f.calls), tt.wantCalls, f.calls)
			}
			if !slices.Contains(f.calls[0], "cat") {
				t.Errorf("the destination wasn't checked first: %q", f.calls)
			}
			for _, c := range f.calls {
				if !slices.Contains(c, "--no-lock") || slices.Contains(c, "--retry-lock") {
					t.Errorf("restic ran without --no-lock: %q", c)
				}
			}
		})
	}
}

func TestStatusSnapshotsNeedsRoot(t *testing.T) {
	asUser(t, 1000)
	_, err := run(t, statusDirs(t), "status", "--snapshots")
	if err == nil || !strings.Contains(err.Error(), "needs root") {
		t.Errorf("err = %v", err)
	}
}

func TestStatusDamaged(t *testing.T) {
	asUser(t, 1000)
	d := statusDirs(t)
	statusMust(t, os.WriteFile(state.RunStatusFile(d), []byte("{not json"), 0o644))
	_, err := run(t, d, "status")
	if err == nil || !strings.Contains(err.Error(), "the next backup writes it again") {
		t.Errorf("err = %v", err)
	}
}

func TestColorFor(t *testing.T) {
	t.Setenv("NO_COLOR", "")
	if colorFor(&bytes.Buffer{}) {
		t.Error("a buffer isn't a terminal")
	}
	f, err := os.CreateTemp(t.TempDir(), "out")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if colorFor(f) {
		t.Error("a file isn't a terminal")
	}
	if tty, err := os.OpenFile("/dev/tty", os.O_WRONLY, 0); err == nil {
		defer tty.Close()
		if !colorFor(tty) {
			t.Error("a terminal gets colours")
		}
		t.Setenv("NO_COLOR", "1")
		if colorFor(tty) {
			t.Error("NO_COLOR switches colours off")
		}
	}
}
