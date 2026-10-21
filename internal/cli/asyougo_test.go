package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Evimore/carryon/internal/backup"
	"github.com/Evimore/carryon/internal/config"
	"github.com/Evimore/carryon/internal/engine"
	"github.com/Evimore/carryon/internal/paths"
	"github.com/Evimore/carryon/internal/pkgmgr"
	"github.com/Evimore/carryon/internal/restore"
	"github.com/Evimore/carryon/internal/state"
	"github.com/Evimore/carryon/internal/sysexec"
	"github.com/Evimore/carryon/internal/ui"
)

var keptSnapshot = engine.Snapshot{ID: "5e1f0c0ffee5", ShortID: "5e1f0c0f", Hostname: "desktop",
	Time: time.Date(2026, 10, 9, 22, 0, 0, 0, time.UTC), Paths: []string{"/"}}

// keepOBS keeps a record of desktop holding OBS's saved settings, for restoring as you go.
func keepOBS(t *testing.T, dirs paths.Dirs) {
	t.Helper()
	dir := t.TempDir()
	for name, body := range map[string]string{
		"meta.json": `{"format": 2, "host": "desktop", "machine_name": "desktop", "user": "alice", "home": "/home/alice",
			"users": [{"name": "alice", "home": "/home/alice"}], "chassis": "desktop", "complete": true, "kernel": "6.17", "machine": ""}`,
		"bundles.json": `{"packages": {"title": "Packages", "when": "always"}, "app-obs-studio": {"title": "OBS Studio", "when": "always"}}`,
		"items.jsonl": `{"bundle": "packages", "id": "vim", "repo": "extra", "type": "pkg"}
{"bundle": "app-obs-studio", "id": "obs-studio", "repo": "extra", "type": "pkg"}
{"bundle": "app-obs-studio", "id": "/etc/obs.conf", "kind": "unowned", "mode": "644", "type": "file"}
{"bundle": "app-obs-studio", "home": ".config/obs-studio", "id": "/home/alice/.config/obs-studio", "type": "path", "user": "alice"}
`,
		"files/etc/obs.conf": "from the backup\n",
	} {
		writeFile(t, filepath.Join(dir, name), body)
	}
	src, err := restore.OpenRecord(dir)
	if err != nil {
		t.Fatal(err)
	}
	snap := keptSnapshot
	src.Snapshot = &snap
	if _, err := restore.Keep(dirs.State, src, nil, time.Now()); err != nil {
		t.Fatal(err)
	}
}

// installedPM is a package manager with these packages installed.
type installedPM struct {
	tinyPM
	installed []string
}

func (p installedPM) Installed(context.Context) ([]string, error) { return p.installed, nil }
func (p installedPM) Packages(context.Context) ([]pkgmgr.Package, error) {
	var out []pkgmgr.Package
	for _, n := range p.installed {
		out = append(out, pkgmgr.Package{Name: n, Repo: "extra"})
	}
	return out, nil
}

// withPackages makes the fake machine's package manager have these packages installed.
func withPackages(t *testing.T, installed ...string) {
	t.Helper()
	prev := backupSystem
	backupSystem = func(c config.Config) (backup.System, error) {
		s, err := prev(c)
		s.Packages = installedPM{installed: installed}
		return s, err
	}
	t.Cleanup(func() { backupSystem = prev })
}

// hookWith runs the pacman hook with pacman's list of packages on its standard input.
func hookWith(t *testing.T, dirs paths.Dirs, targets string) string {
	t.Helper()
	var out bytes.Buffer
	root := NewRoot(&out, dirs)
	root.SetArgs([]string{"hook"})
	root.SetErr(&out)
	root.SetIn(strings.NewReader(targets))
	if err := root.Execute(); err != nil {
		t.Fatalf("the hook failed: %v\n%s", err, out.String())
	}
	return out.String()
}

const obsHint = "carryon: saved settings for obs-studio ("

func TestHookOffersSavedSettingsOnce(t *testing.T) {
	dirs := fakeMachine(t, true, nil)
	withPackages(t, "base", "obs-studio", "vim")
	keepOBS(t, dirs)
	out := hookWith(t, dirs, "obs-studio\nvim\n")
	if !strings.Contains(out, obsHint) || !strings.Contains(out, ") — restore with: carryon restore --bundle app-obs-studio") {
		t.Errorf("hints:\n%s", out)
	}
	in, err := state.ReadInbox(dirs)
	if err != nil || len(in.Entries) != 1 || in.Entries[0].Bundle != "app-obs-studio" ||
		!slices.Equal(in.Entries[0].Changes, []string{"adds /etc/obs.conf", "restores ~/.config/obs-studio (alice)"}) {
		t.Errorf("inbox = %+v, %v", in, err)
	}
	if out := hookWith(t, dirs, "obs-studio\n"); strings.Contains(out, obsHint) {
		t.Errorf("offered twice:\n%s", out)
	}
}

// An upgrade isn't an install: the record already held the package.
func TestHookDoesntOfferUpgrades(t *testing.T) {
	dirs := fakeMachine(t, true, nil)
	withPackages(t, "base", "obs-studio")
	hookWith(t, dirs, "obs-studio\n") // the record now holds obs-studio
	keepOBS(t, dirs)
	if out := hookWith(t, dirs, "obs-studio\n"); strings.Contains(out, obsHint) {
		t.Errorf("an upgrade was offered:\n%s", out)
	}
	if in, _ := state.ReadInbox(dirs); len(in.Entries) != 0 {
		t.Errorf("inbox = %+v", in.Entries)
	}
}

// Restoring everything now pauses the hook, offers included.
func TestHookOffersNothingWhileRestoring(t *testing.T) {
	dirs := fakeMachine(t, true, nil)
	withPackages(t, "base", "obs-studio")
	keepOBS(t, dirs)
	writeFile(t, dirs.RestoringFlag(), "")
	if out := hookWith(t, dirs, "obs-studio\n"); out != "" {
		t.Errorf("hints while restoring:\n%s", out)
	}
}

// asYouGoDirs are Carryon's folders for restoring onto the fake laptop, with OBS's settings kept
// and offered.
func asYouGoDirs(t *testing.T, offer bool) (paths.Dirs, string) {
	t.Helper()
	dirs := paths.Dirs{State: t.TempDir(), Conf: t.TempDir(), Share: "../.."}
	cfg := config.Default()
	cfg.MachineName = "laptop"
	cfg.Repository.URL = "/mnt/backup/carryon"
	if err := config.Write(dirs.ConfigFile(), cfg); err != nil {
		t.Fatal(err)
	}
	keepOBS(t, dirs)
	root := restoreEnv(dirs).Root
	if offer {
		_, err := restore.Offer(context.Background(), restore.OfferOptions{
			Dirs: dirs, Here: &restore.Machine{Root: root, Run: &sysexec.Fake{}},
			Installed: []string{"obs-studio"}, Wait: time.Second, Now: time.Now(),
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	return dirs, root
}

func TestRestoreBundleUsesTheKeptRecord(t *testing.T) {
	r := applyEnv(t, nil)
	dirs, root := asYouGoDirs(t, true)
	out, err := run(t, dirs, "restore", "--yes", "--bundle", "app-obs-studio")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if !strings.Contains(out, "Restore plan: desktop, backup of 2026-10-09 22:00 -> laptop") {
		t.Errorf("output:\n%s", out)
	}
	if b, _ := os.ReadFile(filepath.Join(root, "etc/obs.conf")); string(b) != "from the backup\n" {
		t.Errorf("obs.conf = %q", b)
	}
	if !slices.Contains(r.calls, "pacman -Ql obs-studio") {
		t.Errorf("didn't look for OBS running: %q", r.calls)
	}
	// Restored: the offer is settled and isn't made again.
	if k, _ := restore.ReadKept(dirs.State); !slices.Contains(k.Restored, "app-obs-studio") {
		t.Errorf("kept %+v", k)
	}
	if in, _ := state.ReadInbox(dirs); len(in.Entries) != 0 {
		t.Errorf("inbox = %+v", in.Entries)
	}
}

func TestRestoreOffered(t *testing.T) {
	r := applyEnv(t, nil)
	dirs, root := asYouGoDirs(t, true)
	out, err := run(t, dirs, "restore", "--offered", "--bundle", "app-obs-studio")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if b, _ := os.ReadFile(filepath.Join(root, "etc/obs.conf")); string(b) != "from the backup\n" {
		t.Errorf("obs.conf = %q", b)
	}
	for _, c := range r.calls {
		if strings.HasPrefix(c, "pacman -S") || strings.HasPrefix(c, "paru") {
			t.Errorf("an offer installed software: %s", c)
		}
	}
	if in, _ := state.ReadInbox(dirs); len(in.Entries) != 0 {
		t.Errorf("inbox = %+v", in.Entries)
	}
	// Restored, so it can't be applied again without a new offer.
	if _, err = run(t, dirs, "restore", "--offered", "--bundle", "app-obs-studio"); err == nil || !strings.Contains(err.Error(), "no restore was offered") {
		t.Errorf("applied twice: %v", err)
	}
}

func TestRestoreOfferedRefusesAChangedPlan(t *testing.T) {
	applyEnv(t, nil)
	dirs, root := asYouGoDirs(t, true)
	writeFile(t, filepath.Join(root, "etc/obs.conf"), "mine\n")
	_, err := run(t, dirs, "restore", "--offered", "--bundle", "app-obs-studio")
	if err == nil || !strings.Contains(err.Error(), "nothing was restored. The offer was renewed") {
		t.Fatalf("err = %v", err)
	}
	if b, _ := os.ReadFile(filepath.Join(root, "etc/obs.conf")); string(b) != "mine\n" {
		t.Errorf("a changed plan was applied: obs.conf = %q", b)
	}
	in, _ := state.ReadInbox(dirs)
	if len(in.Entries) != 1 || in.Entries[0].Changes[0] != "replaces /etc/obs.conf" {
		t.Errorf("offer not renewed: %+v", in.Entries)
	}
	// The renewed offer is what the user has seen now: it applies.
	if out, err := run(t, dirs, "restore", "--offered", "--bundle", "app-obs-studio"); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if b, _ := os.ReadFile(filepath.Join(root, "etc/obs.conf")); string(b) != "from the backup\n" {
		t.Errorf("obs.conf = %q", b)
	}
}

// carryon-restore@.service passes its instance name: only a bundle offered from the kept record
// goes through.
func TestRestoreOfferedRefusesAnythingElse(t *testing.T) {
	r := applyEnv(t, nil)
	dirs, root := asYouGoDirs(t, false)
	for arg, want := range map[string]string{
		"../../etc/shadow":         "isn't a bundle ID",
		"-rf":                      "isn't a bundle ID",
		"app\\x2fobs":              "isn't a bundle ID", // an escaped "/"
		"app-nothing":              "has no bundle app-nothing",
		"app-obs-studio":           "no restore was offered",
		"packages --yes":           "isn't a bundle ID",
		strings.Repeat("a", 300):   "isn't a bundle ID",
		"app-obs-studio\\x0a--yes": "isn't a bundle ID",
	} {
		_, err := run(t, dirs, "restore", "--offered", "--bundle="+arg)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: err = %v", arg, err)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "etc/obs.conf")); err == nil {
		t.Error("a refused bundle was restored")
	}
	onlyLooked(t, r)

	geteuid = func() int { return 1000 }
	if _, err := run(t, dirs, "restore", "--offered", "--bundle", "app-obs-studio"); err == nil || !strings.Contains(err.Error(), "needs root") {
		t.Errorf("without root: %v", err)
	}
	geteuid = func() int { return 0 }
	if _, err := run(t, paths.Dirs{State: t.TempDir()}, "restore", "--offered", "--bundle", "app-obs-studio"); err == nil || !strings.Contains(err.Error(), "nothing is offered") {
		t.Errorf("without a kept record: %v", err)
	}
}

func TestUnescapeInstance(t *testing.T) {
	for in, want := range map[string]string{
		"app-obs-studio": "app-obs-studio",
		`app-gtk\x2b`:    "app-gtk+",
		`app\x2f`:        "app/",
		`app\x2`:         `app\x2`,
		`app\xzz`:        `app\xzz`,
	} {
		if got := unescapeInstance(in); got != want {
			t.Errorf("unescapeInstance(%q) = %q, want %q", in, got, want)
		}
	}
}

// tagRepo is a backup whose snapshot restic re-IDs when tagged.
type tagRepo struct{ calls []string }

func (r *tagRepo) Snapshots(context.Context, string) ([]engine.Snapshot, error) {
	s := keptSnapshot
	s.ID, s.ShortID, s.Tags = "7a99ed0000", "7a99ed00", []string{restore.SourceTag}
	return []engine.Snapshot{s}, nil
}
func (r *tagRepo) Run(_ context.Context, args ...string) (engine.Result, error) {
	r.calls = append(r.calls, strings.Join(args, " "))
	return engine.Result{}, nil
}

// A restore from the backup keeps its source; under the source machine's name, the snapshot is
// tagged so retention keeps it, after checking the destination.
func TestKeepSourceAfterARestore(t *testing.T) {
	asRoot(t, true)
	dirs := fakeMachine(t, true, nil) // machine name: desktop, as the record's
	repo := &tagRepo{}
	guarded := 0
	env := restoreSystem{Now: time.Now, Guard: func(context.Context, config.Config) (restore.Repo, error) {
		guarded++
		return repo, nil
	}}
	other := paths.Dirs{State: t.TempDir()}
	keepOBS(t, other)
	src, _, err := restore.OpenKept(other.State)
	if err != nil {
		t.Fatal(err)
	}
	log := ui.NewLogger(&bytes.Buffer{}, false, false)
	keepSource(context.Background(), log, dirs, env, src, false, []string{"app-obs-studio"}, true)
	k, err := restore.ReadKept(dirs.State)
	if err != nil || !k.Tagged || k.Snapshot.ID != "7a99ed0000" || !slices.Equal(k.Restored, []string{"app-obs-studio"}) {
		t.Errorf("kept %+v: %v", k, err)
	}
	if guarded != 1 || !slices.Equal(repo.calls, []string{"tag --add " + restore.SourceTag + " 5e1f0c0ffee5"}) {
		t.Errorf("guarded %d times, ran %q", guarded, repo.calls)
	}

	// --stop-offers takes the tag off and drops the record and the offers.
	if _, err := state.UpdateInbox(dirs, time.Second, func(in *state.Inbox) error {
		in.Put(state.NewEntry(state.KindRestore, "app-obs-studio"))
		in.Put(state.NewEntry(state.KindPath, "/var/lib/jellyfin"))
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	old := restoreEnv
	restoreEnv = func(paths.Dirs) restoreSystem { return env }
	t.Cleanup(func() { restoreEnv = old })
	out, err := run(t, dirs, "restore", "--stop-offers")
	if err != nil || !strings.Contains(out, "won't offer saved settings from desktop any more") {
		t.Fatalf("%v\n%s", err, out)
	}
	if restore.HasKept(dirs.State) || repo.calls[1] != "tag --remove "+restore.SourceTag+" 7a99ed0000" {
		t.Errorf("kept %v, ran %q", restore.HasKept(dirs.State), repo.calls)
	}
	if in, _ := state.ReadInbox(dirs); len(in.Entries) != 1 || in.Entries[0].Kind != state.KindPath {
		t.Errorf("inbox = %+v", in.Entries)
	}
}
