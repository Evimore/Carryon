package state

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Evimore/carryon/internal/dest"
	"github.com/Evimore/carryon/internal/paths"
)

func testDirs(t *testing.T) paths.Dirs {
	tmp := t.TempDir()
	return paths.Dirs{State: filepath.Join(tmp, "state"), Run: filepath.Join(tmp, "run")}
}

func TestDestination(t *testing.T) {
	d := testDirs(t)
	if _, err := ReadDestination(d); !errors.Is(err, ErrNotSetUp) {
		t.Fatalf("err = %v, want ErrNotSetUp", err)
	}
	want := dest.Identity{FSUUID: "0f3c9a52-6e1b-4d8a-9c27-1a2b3c4d5e6f", MountPoint: "/mnt/backup", RepoID: "abc123"}
	if err := WriteDestination(d, want); err != nil {
		t.Fatal(err)
	}
	got, err := ReadDestination(d)
	if err != nil || got != want {
		t.Fatalf("got %+v, %v", got, err)
	}
	if err := os.WriteFile(DestinationPath(d), []byte("{"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadDestination(d); err == nil || !strings.Contains(err.Error(), "carryon init") {
		t.Errorf("damaged file: %v", err)
	}
}

func TestPending(t *testing.T) {
	d := testDirs(t)
	when := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	if err := MarkPending(d, ReasonWaiting, when); err != nil {
		t.Fatal(err)
	}
	p, ok, err := ReadPending(d)
	if err != nil || !ok || p.Reason != ReasonWaiting || !p.Since.Equal(when) {
		t.Fatalf("got %+v %v %v", p, ok, err)
	}
	// a change made after the backup started stays
	if cleared, _ := ClearPending(d, when.Add(-time.Minute)); cleared {
		t.Error("cleared a newer marker")
	}
	if cleared, _ := ClearPending(d, when); !cleared {
		t.Error("didn't clear an older marker")
	}
	if _, ok, _ := ReadPending(d); ok {
		t.Error("the marker is still there")
	}
}

func TestLock(t *testing.T) {
	d := testDirs(t)
	unlock, err := Lock(d)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Lock(d); !errors.Is(err, ErrBusy) {
		t.Errorf("second lock: %v", err)
	}
	unlock()
	unlock2, err := Lock(d)
	if err != nil {
		t.Fatalf("lock after unlock: %v", err)
	}
	unlock2()
}

func TestRunsAndStatus(t *testing.T) {
	d := testDirs(t)
	t0 := time.Date(2026, 10, 1, 3, 0, 0, 0, time.UTC)
	runs := []Run{
		{Kind: KindBackup, Trigger: TriggerTimer, Start: t0, End: t0.Add(time.Minute), Result: ResultOK, Snapshot: "aaaa"},
		{Kind: KindCapture, Trigger: TriggerHook, Start: t0.Add(time.Hour), End: t0.Add(time.Hour), Result: ResultOK},
		{Kind: KindBackup, Trigger: TriggerSettle, Unit: "carryon-settle.service", Invocation: "inv1",
			Start: t0.Add(2 * time.Hour), End: t0.Add(2 * time.Hour), Result: ResultFailed, Error: strings.Repeat("e", 2000)},
	}
	for _, r := range runs {
		if err := AppendRun(d, r); err != nil {
			t.Fatal(err)
		}
	}
	// A damaged line doesn't hide the others.
	f, _ := os.OpenFile(d.RunsFile(), os.O_APPEND|os.O_WRONLY, 0)
	f.WriteString("not json\n")
	f.Close()
	got, err := ReadRuns(d)
	if err != nil || len(got) != 3 {
		t.Fatalf("%d runs, %v", len(got), err)
	}
	if n := len([]rune(got[2].Error)); n > maxErrorText {
		t.Errorf("error kept at %d characters", n)
	}

	timers := func(context.Context) ([]NextRun, error) {
		return []NextRun{{Unit: "carryon-verify.timer", At: t0.Add(72 * time.Hour)}, {Unit: "carryon-backup.timer", At: t0.Add(24 * time.Hour)}}, nil
	}
	if err := os.WriteFile(d.InboxFile(), []byte(`[{"path":"/var/lib/jellyfin"},{"path":"/srv/media"}]`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Refresh(context.Background(), d, "desktop", timers, t0.Add(3*time.Hour)); err != nil {
		t.Fatal(err)
	}
	s, err := ReadStatus(d)
	if err != nil {
		t.Fatal(err)
	}
	if s.LastBackup == nil || s.LastBackup.Result != ResultFailed || s.LastBackup.Invocation != "" {
		t.Errorf("last backup = %+v", s.LastBackup)
	}
	if s.LastSuccess == nil || s.LastSuccess.Snapshot != "aaaa" {
		t.Errorf("last success = %+v", s.LastSuccess)
	}
	if len(s.Recent) != 3 || s.Recent[0].Kind != KindBackup || s.Inbox != 2 || s.Machine != "desktop" {
		t.Errorf("status = %+v", s)
	}
	if len(s.Next) != 2 || s.Next[0].Unit != "carryon-backup.timer" {
		t.Errorf("next = %+v", s.Next)
	}
	// After a reboot /run is empty; the mirror in the state folder answers.
	os.RemoveAll(d.Run)
	if _, err := ReadStatus(d); err != nil {
		t.Errorf("after a reboot: %v", err)
	}
}

func TestTrimRuns(t *testing.T) {
	d := testDirs(t)
	big := Run{Kind: KindBackup, Result: ResultOK, Error: strings.Repeat("x", 400)}
	for i := 0; i < 2600; i++ {
		if err := AppendRun(d, big); err != nil {
			t.Fatal(err)
		}
	}
	runs, _ := ReadRuns(d)
	if len(runs) > keepRunsLines+maxRunsSize/400 {
		t.Errorf("history kept %d runs", len(runs))
	}
}

func TestParseTimers(t *testing.T) {
	out := []byte(`[{"next":1791248400000000,"left":1,"last":null,"passed":null,"unit":"carryon-backup.timer","activates":"carryon-backup.service"},
{"next":null,"left":null,"last":null,"passed":null,"unit":"carryon-settle.timer","activates":"carryon-settle.service"}]`)
	got, err := ParseTimers(out)
	if err != nil || len(got) != 1 || got[0].Unit != "carryon-backup.timer" || got[0].At.Unix() != 1791248400 {
		t.Fatalf("got %+v, %v", got, err)
	}
}

func TestSystemdUnit(t *testing.T) {
	dir := t.TempDir()
	for content, want := range map[string]string{
		"0::/system.slice/carryon-backup.service\n":            "carryon-backup.service",
		"0::/user.slice/user-1000.slice/session-2.scope\n":     "",
		"12:pids:/\n0::/system.slice/carryon-settle.service\n": "carryon-settle.service",
	} {
		f := filepath.Join(dir, "cgroup")
		os.WriteFile(f, []byte(content), 0o644)
		if got := SystemdUnit(f); got != want {
			t.Errorf("%q: got %q, want %q", content, got, want)
		}
	}
	if got := SystemdUnit(filepath.Join(dir, "missing")); got != "" {
		t.Errorf("missing file: %q", got)
	}
}
