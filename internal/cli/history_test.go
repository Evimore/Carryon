package cli

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Evimore/carryon/internal/paths"
	"github.com/Evimore/carryon/internal/record"
	"github.com/Evimore/carryon/internal/sysexec"
)

func useHistoryGit(t *testing.T, r sysexec.Runner) {
	t.Helper()
	old := historyGit
	historyGit = func() sysexec.Runner { return r }
	t.Cleanup(func() { historyGit = old })
}

// publishTwice makes a record in dirs with two history entries: the first record, then vim added.
func publishTwice(t *testing.T, dirs paths.Dirs) {
	t.Helper()
	now := time.Date(2026, 10, 1, 9, 30, 0, 0, time.UTC)
	for _, extra := range []bool{false, true} {
		r, err := record.Load("../record/testdata/format2")
		if err != nil {
			t.Fatal(err)
		}
		if extra {
			r.Items = append(r.Items, record.Item{Type: record.TypePkg, ID: "vim", Repo: "extra", Bundle: "packages"})
		}
		stage, err := os.MkdirTemp(dirs.State, "stage-")
		if err != nil {
			t.Fatal(err)
		}
		if err := r.Save(stage); err != nil {
			t.Fatal(err)
		}
		now = now.Add(24 * time.Hour)
		opts := record.PublishOptions{Commit: true, Git: sysexec.New(), Now: func() time.Time { return now }}
		if _, err := record.Publish(context.Background(), stage, dirs.RecordDir(), opts); err != nil {
			t.Fatal(err)
		}
	}
}

func TestHistory(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git isn't installed")
	}
	useHistoryGit(t, sysexec.New())
	dirs := paths.Dirs{State: t.TempDir()}
	publishTwice(t, dirs)

	out, err := run(t, dirs, "history")
	if err != nil {
		t.Fatal(err)
	}
	want := "2026-10-03 09:30  1 change(s): + package vim [extra]\n2026-10-02 09:30  first record\n"
	if out != want {
		t.Errorf("history:\n%s\nwant:\n%s", out, want)
	}

	out, err = run(t, dirs, "history", "-n", "1", "--changes")
	if err != nil {
		t.Fatal(err)
	}
	want = "2026-10-03 09:30  1 change(s): + package vim [extra]\n                  + package    vim  [extra]\n"
	if out != want {
		t.Errorf("history --changes:\n%s\nwant:\n%s", out, want)
	}

	out, err = run(t, dirs, "history", "--json")
	if err != nil {
		t.Fatal(err)
	}
	var commits []map[string]any
	if err := json.Unmarshal([]byte(out), &commits); err != nil {
		t.Fatalf("%v:\n%s", err, out)
	}
	if len(commits) != 2 || commits[1]["subject"] != "first record" || commits[0]["date"] != "2026-10-03T09:30:00Z" {
		t.Errorf("json = %s", out)
	}
	if _, ok := commits[0]["changes"]; ok {
		t.Errorf("changes listed without --changes: %s", out)
	}
}

func TestHistoryWithoutHistory(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git isn't installed")
	}
	useHistoryGit(t, sysexec.New())
	dirs := paths.Dirs{State: t.TempDir()}
	out, err := run(t, dirs, "history")
	if err != nil || !strings.Contains(out, "No history yet") {
		t.Errorf("out = %q, err = %v", out, err)
	}
	out, err = run(t, dirs, "history", "--json")
	if err != nil || strings.TrimSpace(out) != "[]" {
		t.Errorf("out = %q, err = %v", out, err)
	}
}

func TestHistoryWithoutGit(t *testing.T) {
	useHistoryGit(t, &sysexec.Fake{})
	dirs := paths.Dirs{State: t.TempDir()}
	if err := os.MkdirAll(filepath.Join(dirs.RecordDir(), ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	_, err := run(t, dirs, "history")
	if err == nil || !strings.Contains(err.Error(), "git isn't installed") || !strings.Contains(err.Error(), "pacman -S git") {
		t.Fatalf("err = %v", err)
	}
}

func TestHistoryNeedsRootForTheRecord(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: every record is readable")
	}
	useHistoryGit(t, &sysexec.Fake{Programs: []string{"git"}})
	dirs := paths.Dirs{State: t.TempDir()}
	if err := os.MkdirAll(filepath.Join(dirs.RecordDir(), ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dirs.RecordDir(), 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dirs.RecordDir(), 0o700) })
	_, err := run(t, dirs, "history")
	if err == nil || !strings.Contains(err.Error(), "run it with sudo") {
		t.Fatalf("err = %v", err)
	}
}
