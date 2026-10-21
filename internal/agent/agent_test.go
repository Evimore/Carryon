package agent

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Evimore/carryon/internal/config"
	"github.com/Evimore/carryon/internal/state"
	"github.com/Evimore/carryon/internal/sysexec"
)

func newAgent(t *testing.T, desk Desktop) (*Agent, *sysexec.Fake) {
	t.Helper()
	u := testUser(t)
	f := &sysexec.Fake{}
	return &Agent{
		Dirs: testDirs(t), User: u, Desktop: desk, Runner: f,
		StateDir: StateDir(u.Home, ""), Settings: settingsOn,
	}, f
}

// Each kind of event shows with its own text and buttons.
func TestEvents(t *testing.T) {
	jelly := pathEntry("/var/lib/jellyfin", "jellyfin-server", 12<<30, true)
	tests := []struct {
		name    string
		status  *state.Status
		entries []state.Entry
		want    []string // title | buttons
	}{
		{"all is well", ptr(okStatus(now)), nil, nil},
		{
			"a failed backup", ptr(failedStatus(now)), nil,
			[]string{"Backup failed | backup=Back up now"},
		},
		{
			"a failed check", ptr(checkFailed(now)), nil,
			[]string{"Backup check found problems | "},
		},
		{
			"the newest backup is too old", ptr(okStatus(now.Add(-72 * time.Hour))), nil,
			[]string{"Your backup is getting old | backup=Back up now"},
		},
		{
			"a suggestion", ptr(okStatus(now)), []state.Entry{jelly},
			[]string{"Back up /var/lib/jellyfin? | add=Back it up ignore=Ignore"},
		},
		{
			"a restore offer", ptr(okStatus(now)), []state.Entry{restoreEntry("app-obs-studio", "obs-studio")},
			[]string{"Saved settings for obs-studio | restore=Restore later=Not now"},
		},
		{
			"no status yet, but a suggestion", nil, []state.Entry{jelly},
			[]string{"Back up /var/lib/jellyfin? | add=Back it up ignore=Ignore"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			desk := newFakeDesktop("actions", "body")
			a, _ := newAgent(t, desk)
			if tt.status != nil {
				writeStatus(t, a.Dirs, *tt.status)
			}
			putEntries(t, a.Dirs, tt.entries...)
			r := start(t, a)
			r.waitEval(t, 3)
			var got []string
			for _, n := range desk.shown() {
				got = append(got, n.Title+" | "+strings.Join(n.buttons(), " "))
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("shown:\n%q\nwant:\n%q", got, tt.want)
			}
		})
	}
}

func ptr[T any](v T) *T { return &v }

// checkFailed is a status whose last check, at end, found problems.
func checkFailed(end time.Time) state.Status {
	s := okStatus(end)
	check := state.Run{Kind: state.KindVerify, Trigger: state.TriggerTimer, Start: end.Add(-time.Minute), End: end, Result: state.ResultFailed, Error: "2 packages can't be reinstalled from any repository."}
	s.Recent = append([]state.Run{check}, s.Recent...)
	return s
}

func TestEventText(t *testing.T) {
	desk := newFakeDesktop("actions", "body", "body-markup")
	a, _ := newAgent(t, desk)
	writeStatus(t, a.Dirs, failedStatus(now))
	putEntries(t, a.Dirs, pathEntry("/srv/a&b", "jellyfin-server", 12<<30, true))
	start(t, a).waitEval(t, 2)
	notes := desk.shown()
	if len(notes) != 2 {
		t.Fatalf("shown %d notifications", len(notes))
	}
	wantBody := "The last backup failed: unable to open the repository at sftp:alice@example.com:/backups\n" +
		"Check that example.com is reachable, then run: sudo carryon backup"
	if notes[0].Body != wantBody || notes[0].Urgency != urgencyCritical {
		t.Errorf("failure: %q (urgency %d)", notes[0].Body, notes[0].Urgency)
	}
	// The service reads markup, so the path's "&" is escaped.
	if want := "/srv/a&amp;b holds data that isn't backed up yet. Probably jellyfin-server, 12.0 GiB (large)."; notes[1].Body != want {
		t.Errorf("suggestion: %q\nwant %q", notes[1].Body, want)
	}
}

// A service without buttons gets the commands to run instead.
func TestNoButtons(t *testing.T) {
	desk := newFakeDesktop("body")
	a, _ := newAgent(t, desk)
	e := pathEntry("/var/lib/jellyfin", "", 0, false)
	putEntries(t, a.Dirs, e)
	start(t, a).waitEval(t, 2)
	notes := desk.shown()
	if len(notes) != 1 || len(notes[0].Actions) != 0 {
		t.Fatalf("shown %+v", notes)
	}
	for _, want := range []string{"Back it up: sudo carryon add " + e.ID, "Ignore: sudo carryon ignore " + e.ID} {
		if !strings.Contains(notes[0].Body, want) {
			t.Errorf("body %q lacks %q", notes[0].Body, want)
		}
	}
}

// Each event shows once, while the agent runs and after it starts again; a new one still shows.
func TestShowsEachEventOnce(t *testing.T) {
	desk := newFakeDesktop("actions")
	a, _ := newAgent(t, desk)
	writeStatus(t, a.Dirs, failedStatus(now))
	putEntries(t, a.Dirs, pathEntry("/var/lib/jellyfin", "", 0, false))
	r := start(t, a)
	r.waitEval(t, 5)
	if n := len(desk.shown()); n != 2 {
		t.Fatalf("shown %d notifications, want 2", n)
	}
	r.stop()

	again := &Agent{Dirs: a.Dirs, User: a.User, Desktop: desk, Runner: a.Runner, StateDir: a.StateDir, Settings: settingsOn}
	r = start(t, again)
	r.waitEval(t, 5)
	if n := len(desk.shown()); n != 2 {
		t.Fatalf("after a restart: shown %d notifications, want still 2", n)
	}

	// The next night's failure is a new event; so is a new suggestion.
	writeStatus(t, a.Dirs, failedStatus(now.Add(time.Minute)))
	putEntries(t, a.Dirs, pathEntry("/srv/media", "", 0, false))
	r.waitEval(t, 5)
	notes := desk.shown()
	if len(notes) != 4 || notes[2].Title != "Backup failed" || notes[3].Title != "Back up /srv/media?" {
		t.Fatalf("shown %+v", notes)
	}

	data, err := os.ReadFile(filepath.Join(a.StateDir, StateFile))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "backup-failed:"+stamp(now)+`"`) || !strings.Contains(string(data), "backup-failed:"+stamp(now.Add(time.Minute))) {
		t.Errorf("the state file should remember only the newest failure:\n%s", data)
	}
	if fi, _ := os.Stat(filepath.Join(a.StateDir, StateFile)); fi.Mode().Perm() != 0o600 {
		t.Errorf("%s is %v, want 0600", StateFile, fi.Mode().Perm())
	}
}

// When the service doesn't answer, nothing counts as shown, and it shows once it answers.
func TestRetriesWhenTheServiceIsAway(t *testing.T) {
	desk := newFakeDesktop("actions")
	desk.setFail(errNoService)
	a, _ := newAgent(t, desk)
	writeStatus(t, a.Dirs, failedStatus(now))
	r := start(t, a)
	r.waitEval(t, 3)
	desk.setFail(nil)
	eventually(t, "the failure to show", func() bool { return len(desk.shown()) == 1 })
	r.waitEval(t, 3)
	if n := len(desk.shown()); n != 1 {
		t.Errorf("shown %d times", n)
	}
}

// The user's own settings and backup.users decide whether anything shows.
func TestFollowsSettings(t *testing.T) {
	t.Run("desktop notifications off", func(t *testing.T) {
		desk := newFakeDesktop("actions")
		a, _ := newAgent(t, desk)
		off := false
		a.Settings = func(sysexec.User) (config.UserNotify, error) { return config.UserNotify{Desktop: !off}, nil }
		off = true
		writeStatus(t, a.Dirs, failedStatus(now))
		r := start(t, a)
		r.waitEval(t, 3)
		if n := len(desk.shown()); n != 0 {
			t.Fatalf("shown %d notifications while they are off", n)
		}
		r.stop()
		off = false // switched back on: what is still current shows
		a.shown = shown{}
		start(t, a).waitEval(t, 2)
		if n := len(desk.shown()); n != 1 {
			t.Errorf("shown %d notifications once back on, want 1", n)
		}
	})
	t.Run("not in backup.users", func(t *testing.T) {
		desk := newFakeDesktop("actions")
		a, _ := newAgent(t, desk)
		if err := os.WriteFile(a.Dirs.ConfigFile(), []byte("[backup]\nusers = [\"bob\"]\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		writeStatus(t, a.Dirs, failedStatus(now))
		start(t, a).waitEval(t, 3)
		if n := len(desk.shown()); n != 0 {
			t.Errorf("shown %d notifications to a user Carryon doesn't back up", n)
		}
	})
}

// Each button starts its unit; Not now starts nothing.
func TestButtonsStartUnits(t *testing.T) {
	desk := newFakeDesktop("actions")
	a, f := newAgent(t, desk)
	add, ignore := pathEntry("/var/lib/jellyfin", "", 0, false), pathEntry("/srv/media", "", 0, false)
	offer, later := restoreEntry("app-obs-studio", "obs-studio"), restoreEntry("app-gimp", "gimp")
	writeStatus(t, a.Dirs, failedStatus(now))
	putEntries(t, a.Dirs, add, ignore, offer, later)
	want := []string{
		"systemctl start --no-block carryon-backup.service",
		"systemctl start --no-block carryon-add@" + add.ID + ".service",
		"systemctl start --no-block carryon-ignore@" + ignore.ID + ".service",
		"systemctl start --no-block carryon-restore@app-obs-studio.service",
	}
	for _, c := range want {
		f.On(c, sysexec.Response{})
	}
	r := start(t, a)
	eventually(t, "five notifications", func() bool { return len(desk.shown()) == 5 })
	press := map[string]string{
		"Backup failed":                 keyBackup,
		"Back up /var/lib/jellyfin?":    keyAdd,
		"Back up /srv/media?":           keyIgnore,
		"Saved settings for obs-studio": keyRestore,
		"Saved settings for gimp":       keyLater,
	}
	for _, n := range desk.shown() {
		desk.signals <- Signal{ID: n.ID, Action: press[n.Title]}
	}
	eventually(t, "the units to start", func() bool { return len(f.Commands()) == len(want) })
	r.waitEval(t, 3)
	got := f.Commands()
	if !sameSet(got, want) {
		t.Errorf("ran %q\nwant %q", got, want)
	}
	// A button pressed again after it was answered does nothing.
	desk.signals <- Signal{ID: desk.shown()[0].ID, Action: keyBackup}
	r.waitEval(t, 3)
	if n := len(f.Commands()); n != len(want) {
		t.Errorf("ran %d commands, want %d", n, len(want))
	}
}

func sameSet(a, b []string) bool {
	m := map[string]int{}
	for _, s := range a {
		m[s]++
	}
	for _, s := range b {
		m[s]--
	}
	for _, n := range m {
		if n != 0 {
			return false
		}
	}
	return len(a) == len(b)
}

// Restore warns first while the app runs, and its button checks again.
func TestRestoreWarnsWhileTheAppRuns(t *testing.T) {
	desk := newFakeDesktop("actions")
	a, f := newAgent(t, desk)
	f.On("systemctl start --no-block carryon-restore@app-obs-studio.service", sysexec.Response{})
	var runs = make(chan bool, 1)
	runs <- true
	a.Running = func(_ context.Context, app string) bool {
		if app != "obs-studio" {
			t.Errorf("asked about %q", app)
		}
		v := <-runs
		runs <- v
		return v
	}
	putEntries(t, a.Dirs, restoreEntry("app-obs-studio", "obs-studio"))
	r := start(t, a)
	eventually(t, "the offer", func() bool { return len(desk.shown()) == 1 })
	offer := desk.shown()[0]
	if !strings.Contains(offer.Body, "obs-studio is running: close it before you restore.") {
		t.Errorf("offer: %q", offer.Body)
	}
	desk.signals <- Signal{ID: offer.ID, Action: keyRestore}
	eventually(t, "the warning", func() bool { return len(desk.shown()) == 2 })
	warning := desk.shown()[1]
	if warning.Title != "Close obs-studio first" || !reflect.DeepEqual(warning.buttons(), []string{"restore=Restore", "later=Not now"}) {
		t.Errorf("warning: %q %q", warning.Title, warning.buttons())
	}
	if n := len(f.Commands()); n != 0 {
		t.Fatalf("restored while the app runs: %q", f.Commands())
	}
	<-runs
	runs <- false // closed
	desk.signals <- Signal{ID: warning.ID, Action: keyRestore}
	eventually(t, "the restore", func() bool { return len(f.Commands()) == 1 })
	r.waitEval(t, 1)
}

// A unit that can't start says so, with the command to run by hand.
func TestStartFails(t *testing.T) {
	desk := newFakeDesktop("actions")
	a, f := newAgent(t, desk)
	e := pathEntry("/var/lib/jellyfin", "", 0, false)
	f.On("systemctl start --no-block carryon-add@"+e.ID+".service", sysexec.Response{
		Code: 5, Stderr: "Failed to start carryon-add@" + e.ID + ".service: Unit carryon-add@" + e.ID + ".service not found.",
	})
	putEntries(t, a.Dirs, e)
	start(t, a)
	eventually(t, "the suggestion", func() bool { return len(desk.shown()) == 1 })
	desk.signals <- Signal{ID: desk.shown()[0].ID, Action: keyAdd}
	eventually(t, "the error", func() bool { return len(desk.shown()) == 2 })
	n := desk.shown()[1]
	want := "carryon-add@" + e.ID + ".service isn't installed. Update Carryon to get it.\nRun instead: sudo carryon add " + e.ID
	if n.Title != "Couldn't back it up" || n.Body != want {
		t.Errorf("shown %q: %q", n.Title, n.Body)
	}
}

// Entries the agent can't offer safely aren't shown.
func TestUnsafeEntries(t *testing.T) {
	desk := newFakeDesktop("actions")
	a, _ := newAgent(t, desk)
	bad := restoreEntry("app-x", "x")
	bad.Bundle = "../../etc/passwd"
	odd := pathEntry("/var/lib/odd", "", 0, false)
	odd.ID = "path-/var/lib/odd"
	putEntries(t, a.Dirs, bad, odd)
	start(t, a).waitEval(t, 3)
	if n := len(desk.shown()); n != 0 {
		t.Errorf("shown %+v", desk.shown())
	}
}

func TestStateDir(t *testing.T) {
	for _, tc := range []struct{ xdg, want string }{
		{"", "/home/alice/.local/state/carryon"},
		{"/home/alice/.state", "/home/alice/.state/carryon"},
		{"/var/tmp/state", "/home/alice/.local/state/carryon"}, // outside the home
		{"/home/alice/../bob/state", "/home/alice/.local/state/carryon"},
		{"relative", "/home/alice/.local/state/carryon"},
	} {
		if got := StateDir("/home/alice", tc.xdg); got != tc.want {
			t.Errorf("StateDir(%q) = %q, want %q", tc.xdg, got, tc.want)
		}
	}
}

func TestDamagedStateFile(t *testing.T) {
	desk := newFakeDesktop("actions")
	a, _ := newAgent(t, desk)
	if err := os.MkdirAll(a.StateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(a.StateDir, StateFile), []byte("{oops"), 0o600); err != nil {
		t.Fatal(err)
	}
	writeStatus(t, a.Dirs, failedStatus(now))
	start(t, a).waitEval(t, 2)
	if n := len(desk.shown()); n != 1 {
		t.Errorf("shown %d notifications", n)
	}
	data, _ := os.ReadFile(filepath.Join(a.StateDir, StateFile))
	if !strings.Contains(string(data), "backup-failed:") {
		t.Errorf("the state file wasn't written again: %s", data)
	}
}

// A restore offer says what it would change; when the plan changes, the renewed offer shows again.
func TestRestoreOfferChanges(t *testing.T) {
	desk := newFakeDesktop("actions")
	a, _ := newAgent(t, desk)
	offer := restoreEntry("app-obs-studio", "obs-studio")
	offer.Changes = []string{"3 files in ~/.config/obs-studio", "enables obs-virtualcam.service"}
	putEntries(t, a.Dirs, offer)
	r := start(t, a)
	eventually(t, "the offer", func() bool { return len(desk.shown()) == 1 })
	want := "Your backup has saved settings for obs-studio (app-obs-studio).\n" +
		"Restoring them would change:\n• 3 files in ~/.config/obs-studio\n• enables obs-virtualcam.service\n" +
		"Restore them here?"
	if got := desk.shown()[0].Body; got != want {
		t.Errorf("body:\n%s\nwant:\n%s", got, want)
	}
	r.waitEval(t, 3)
	if n := len(desk.shown()); n != 1 {
		t.Fatalf("shown %d times", n)
	}

	offer.Changes = []string{"4 files in ~/.config/obs-studio"} // the plan changed: the offer is renewed
	putEntries(t, a.Dirs, offer)
	eventually(t, "the renewed offer", func() bool { return len(desk.shown()) == 2 })
	if !strings.Contains(desk.shown()[1].Body, "• 4 files in ~/.config/obs-studio") {
		t.Errorf("renewed offer: %q", desk.shown()[1].Body)
	}
	r.waitEval(t, 3)
	if n := len(desk.shown()); n != 2 {
		t.Errorf("shown %d times, want 2", n)
	}
}

// A bundle ID with characters a unit name can't hold is escaped as systemd-escape does.
func TestRestoreUnitEscapes(t *testing.T) {
	for bundle, want := range map[string]string{
		"app-obs-studio":   "carryon-restore@app-obs-studio.service",
		"app-gtk+":         `carryon-restore@app-gtk\x2b.service`,
		"cfg-foo@bar_1.2":  `carryon-restore@cfg-foo\x40bar_1.2.service`,
		"app-libreoffice7": "carryon-restore@app-libreoffice7.service",
	} {
		if got := RestoreUnit(bundle); got != want {
			t.Errorf("RestoreUnit(%q) = %q, want %q", bundle, got, want)
		}
	}
	desk := newFakeDesktop("actions")
	a, f := newAgent(t, desk)
	f.On(`systemctl start --no-block 'carryon-restore@app-gtk\x2b.service'`, sysexec.Response{})
	putEntries(t, a.Dirs, restoreEntry("app-gtk+", ""))
	start(t, a)
	eventually(t, "the offer", func() bool { return len(desk.shown()) == 1 })
	desk.signals <- Signal{ID: desk.shown()[0].ID, Action: keyRestore}
	eventually(t, "the restore", func() bool { return len(f.Commands()) == 1 })
	if got := f.Calls()[0].Args; got[len(got)-1] != `carryon-restore@app-gtk\x2b.service` {
		t.Errorf("started %q", got)
	}
}
