package backup

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Evimore/carryon/internal/config"
	"github.com/Evimore/carryon/internal/dest"
	"github.com/Evimore/carryon/internal/paths"
	"github.com/Evimore/carryon/internal/state"
)

func TestRecordWritesAndRemovesTheLoginMessage(t *testing.T) {
	tmp := t.TempDir()
	d := paths.Dirs{
		Conf: filepath.Join(tmp, "conf"), State: filepath.Join(tmp, "state"),
		Run: filepath.Join(tmp, "run"), Motd: filepath.Join(tmp, "motd.d"),
	}
	if err := state.WriteDestination(d, dest.Identity{MountPoint: "/mnt/backup", RepoID: "abc123"}); err != nil {
		t.Fatal(err)
	}
	start := time.Date(2026, 10, 18, 14, 0, 0, 0, time.UTC)
	backup := func(at time.Time, result string) *testLog {
		t.Helper()
		log := &testLog{}
		r := state.Run{Kind: state.KindBackup, Trigger: "timer", Start: at, End: at.Add(2 * time.Minute), Result: result}
		if result == state.ResultFailed {
			r.Error = "The backup drive isn't plugged in."
		}
		Record(context.Background(), d, r, "desktop", nil, log)
		if _, err := os.Stat(d.StatusFile()); err != nil {
			t.Fatalf("status.json wasn't written: %v", err)
		}
		return log
	}
	line := func() string {
		t.Helper()
		data, err := os.ReadFile(d.LoginMessageFile())
		if os.IsNotExist(err) {
			return ""
		}
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}
	setLoginMessage := func(on bool) {
		t.Helper()
		cfg := config.Default()
		cfg.Repository.URL = "/mnt/backup/carryon"
		cfg.Notify.LoginMessage = on
		data, err := config.Format(cfg)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(d.Conf, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(d.ConfigFile(), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	backup(start, state.ResultOK)
	if got := line(); got != "" {
		t.Fatalf("after a good backup the login message is %q, want none", got)
	}

	backup(start.Add(24*time.Hour), state.ResultFailed)
	want := "Carryon: the last backup failed (Oct 19 14:02). See: carryon status\n"
	if got := line(); got != want {
		t.Fatalf("after a failed backup the login message is %q, want %q", got, want)
	}
	if fi, err := os.Stat(d.LoginMessageFile()); err != nil || fi.Mode().Perm() != 0o644 {
		t.Errorf("the login message's mode: %v %v, want readable by everyone", fi, err)
	}

	backup(start.Add(25*time.Hour), state.ResultOK)
	if got := line(); got != "" {
		t.Fatalf("after the next good backup the login message is %q, want none", got)
	}

	backup(start.Add(26*time.Hour), state.ResultFailed)
	if line() == "" {
		t.Fatal("no login message after a failed backup")
	}
	setLoginMessage(false)
	backup(start.Add(27*time.Hour), state.ResultFailed)
	if got := line(); got != "" {
		t.Fatalf("with notify.login_message off the login message is %q, want none", got)
	}
	setLoginMessage(true)
	backup(start.Add(28*time.Hour), state.ResultFailed)
	if line() == "" {
		t.Fatal("no login message once notify.login_message is back on")
	}

	// A motd folder that can't be written only warns: the status is still written.
	if err := os.RemoveAll(d.Motd); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(d.Motd, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	log := backup(start.Add(29*time.Hour), state.ResultFailed)
	if !strings.Contains(log.String(), "warn: The login message wasn't updated") {
		t.Errorf("no warning about the login message:\n%s", log)
	}
}

func TestRecordWithoutMotdFolderWritesNoLoginMessage(t *testing.T) {
	tmp := t.TempDir()
	d := paths.Dirs{State: filepath.Join(tmp, "state"), Run: filepath.Join(tmp, "run")}
	at := time.Date(2026, 10, 18, 14, 0, 0, 0, time.UTC)
	log := &testLog{}
	Record(context.Background(), d, state.Run{Kind: state.KindBackup, Start: at, End: at, Result: state.ResultFailed}, "desktop", nil, log)
	if log.String() != "" {
		t.Errorf("warnings: %s", log)
	}
	entries, err := os.ReadDir(tmp)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() != "state" && e.Name() != "run" {
			t.Errorf("unexpected %s in %s", e.Name(), tmp)
		}
	}
}
