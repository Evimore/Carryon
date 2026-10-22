package setup

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/Evimore/carryon/internal/config"
	"github.com/Evimore/carryon/internal/restore"
	"github.com/Evimore/carryon/internal/state"
	"github.com/Evimore/carryon/internal/ui"
)

// desktopSettings backs up a Carryon machine named desktop with its settings, as a backup lays
// it out, and removes them from this machine.
func (h *harness) desktopSettings(fingerprint string) {
	h.t.Helper()
	e := h.makeRepo(h.repo, testPassword)
	cfg := config.Default()
	cfg.MachineName = "desktop"
	cfg.Repository = config.Repository{URL: "sftp:alice@example.com:/old", PasswordCommand: "systemd-creds decrypt /etc/carryon/repository-password.cred"}
	cfg.Backup.Users = []string{"alice"}
	cfg.Backup.ExtraPaths = []string{"/srv", "/opt/games"}
	text, err := config.Format(cfg)
	if err != nil {
		h.t.Fatal(err)
	}
	d := h.env.Dirs
	h.backup(e, "desktop", []string{"carryon"}, map[string]string{
		d.ConfigFile():                               string(text),
		d.ExcludesFile():                             "# mine\n/home/*/Videos\n",
		d.BundlesFile():                              "put gaming pkg steam\n",
		d.PasswordFile():                             "the other machine's password\n",
		state.DestinationPath(d):                     `{"mount_point":"/mnt/other","repo_id":"other"}`,
		filepath.Join(d.RecordDir(), "meta.json"):    meta(fingerprint),
		filepath.Join(d.RecordDir(), "items.jsonl"):  `{"bundle": "app-obs-studio", "id": "obs-studio", "type": "pkg"}` + "\n",
		filepath.Join(d.RecordDir(), "bundles.json"): `{"app-obs-studio": {"title": "OBS Studio", "when": "always"}}`,
	})
}

func TestConnectOnAReinstalledMachine(t *testing.T) {
	t.Parallel()
	h := newHarness(t,
		testPassword,  // repository password
		ui.UseDefault, // reuse settings of: this machine
		ui.UseDefault, // machine name: the one it had
		ui.UseDefault, // restore: everything now
	)
	h.desktopSettings(testFingerprint)
	h.env.Hostname = func() (string, error) { return "archlinux", nil } // a fresh install's default
	if err := h.env.Connect(context.Background(), h.repo, Options{}); err != nil {
		t.Fatalf("Connect: %v\n%s", err, h.log)
	}
	h.wantNoAnswersLeft()
	d := h.env.Dirs

	cfg := h.config()
	if cfg.MachineName != "desktop" {
		t.Errorf("machine name %q, want the reused machine's", cfg.MachineName)
	}
	if !reflect.DeepEqual(cfg.Backup.Users, []string{"alice"}) || !reflect.DeepEqual(cfg.Backup.ExtraPaths, []string{"/srv", "/opt/games"}) {
		t.Errorf("settings weren't reused: %+v", cfg.Backup)
	}
	if cfg.Repository.URL != h.repo || cfg.Repository.PasswordFile != d.PasswordFile() || cfg.Repository.PasswordCommand != "" {
		t.Errorf("repository = %+v", cfg.Repository)
	}
	if pw, _ := ReadPasswordFile(d.PasswordFile()); pw != testPassword {
		t.Errorf("password file holds %q", pw)
	}
	if got := h.readFile(d.ExcludesFile()); got != "# mine\n/home/*/Videos\n" {
		t.Errorf("excludes = %q", got)
	}
	if got := h.readFile(d.BundlesFile()); got != "put gaming pkg steam\n" {
		t.Errorf("bundles = %q", got)
	}
	if id := h.identity(); id.RepoID == "other" || id.MountPoint != h.drive || id.FSUUID != testUUID {
		t.Errorf("identity = %+v, want this machine's own", id)
	}
	if !h.exists(d.RestoringFlag()) {
		t.Error("backups aren't paused for the restore")
	}
	if len(h.fake.Commands()) != 2+len(Timers) { // daemon-reload, one enable per unit, and the agent
		t.Errorf("commands = %q", h.fake.Commands())
	}
	out := h.out.String()
	for _, s := range []string{"Machines in this repository:", "desktop", "1 in all", "sudo carryon restore"} {
		if !strings.Contains(out, s) {
			t.Errorf("output lacks %q:\n%s", s, out)
		}
	}
	if !KitConfirmed(d) {
		t.Error("typing the password in should count as having the kit")
	}
}

func TestConnectAnotherMachine(t *testing.T) {
	t.Parallel()
	h := newHarness(t, testPassword, "desktop", ui.UseDefault, ui.UseDefault)
	h.desktopSettings("fedcba9876543210")
	h.env.Hostname = func() (string, error) { return "laptop", nil }
	if err := h.env.Connect(context.Background(), h.repo, Options{}); err != nil {
		t.Fatalf("Connect: %v\n%s", err, h.log)
	}
	cfg := h.config()
	if cfg.MachineName != "laptop" || !reflect.DeepEqual(cfg.Backup.Users, []string{"alice"}) {
		t.Errorf("config = %+v", cfg)
	}
}

func TestConnectWithFlagsAndNoSettings(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.env.Prompt = ui.Defaults{}
	h.desktopSettings("fedcba9876543210")
	pwFile := filepath.Join(h.tmp, "pw")
	h.writeFile(pwFile, testPassword+"\n", 0o600)
	err := h.env.Connect(context.Background(), h.repo, Options{PasswordFile: pwFile, SettingsFrom: "none", MachineName: "laptop"})
	if err != nil {
		t.Fatalf("Connect: %v\n%s", err, h.log)
	}
	cfg := h.config()
	if cfg.MachineName != "laptop" || !reflect.DeepEqual(cfg.Backup.Users, []string{"*"}) {
		t.Errorf("config = %+v", cfg)
	}
	if h.exists(h.env.Dirs.ExcludesFile()) || h.exists(h.env.Dirs.BundlesFile()) {
		t.Error("settings files were copied although none were wanted")
	}
}

func TestConnectNeedsARepository(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.env.Prompt = ui.Defaults{}
	err := h.env.Connect(context.Background(), h.repo, Options{})
	if err == nil || !strings.Contains(err.Error(), "there's no backup repository at "+h.repo) {
		t.Fatalf("err = %v", err)
	}
	if h.exists(h.env.Dirs.ConfigFile()) || h.exists(h.env.Dirs.RestoringFlag()) {
		t.Error("connect wrote something without a repository")
	}
}

func TestConnectKeepsReplacedFiles(t *testing.T) {
	t.Parallel()
	h := newHarness(t, testPassword, ui.UseDefault, ui.UseDefault, ui.UseDefault)
	h.desktopSettings(testFingerprint)
	h.writeFile(h.env.Dirs.ExcludesFile(), "/home/*/mine\n", 0o644)
	if err := h.env.Connect(context.Background(), h.repo, Options{}); err != nil {
		t.Fatalf("Connect: %v\n%s", err, h.log)
	}
	if got := h.readFile(h.env.Dirs.ExcludesFile() + ".before-desktop"); got != "/home/*/mine\n" {
		t.Errorf("kept %q", got)
	}
}

func TestConnectAsYouGo(t *testing.T) {
	t.Parallel()
	h := newHarness(t,
		testPassword,   // repository password
		ui.UseDefault,  // reuse settings of: this machine
		ui.UseDefault,  // machine name: the one it had, so its snapshot gets tagged
		RestoreAsYouGo, // restore as you go
	)
	h.desktopSettings(testFingerprint)
	if err := h.env.Connect(context.Background(), h.repo, Options{}); err != nil {
		t.Fatalf("Connect: %v\n%s", err, h.log)
	}
	h.wantNoAnswersLeft()
	d := h.env.Dirs
	if h.exists(d.RestoringFlag()) {
		t.Error("restoring as you go paused backups")
	}
	src, kept, err := restore.OpenKept(d.State)
	if err != nil {
		t.Fatalf("no kept source: %v\n%s", err, h.log)
	}
	if _, ok := src.Record.Bundles.Get("app-obs-studio"); !ok || kept.Machine != "desktop" || !kept.Tagged {
		t.Errorf("kept %+v with bundles %+v", kept, src.Record.Bundles)
	}
	if mode := h.mode(restore.KeptDir(d.State)); mode.Perm() != 0o700 {
		t.Errorf("the kept record's folder has mode %v", mode)
	}
	// The kept snapshot is the tagged one: tagging gave it a new ID.
	e := h.engineFor(h.repo, testPassword, "desktop")
	snaps, err := e.Snapshots(context.Background(), "desktop")
	if err != nil || len(snaps) != 1 || snaps[0].ID != kept.Snapshot.ID || !slices.Contains(snaps[0].Tags, restore.SourceTag) {
		t.Errorf("snapshots %+v (%v), kept %s", snaps, err, kept.Snapshot.ID)
	}
	if out := h.out.String(); !strings.Contains(out, "Automatic backups start now.") || !strings.Contains(out, "offers their saved settings from desktop") {
		t.Errorf("output:\n%s", out)
	}
}

// Under another name, this machine's retention never touches the source's snapshots: no tag.
func TestConnectAsYouGoFromAnotherMachine(t *testing.T) {
	t.Parallel()
	h := newHarness(t, testPassword, "desktop", ui.UseDefault)
	h.desktopSettings("fedcba9876543210")
	h.env.Hostname = func() (string, error) { return "laptop", nil }
	if err := h.env.Connect(context.Background(), h.repo, Options{Restore: RestoreAsYouGo}); err != nil {
		t.Fatalf("Connect: %v\n%s", err, h.log)
	}
	h.wantNoAnswersLeft()
	if _, kept, err := restore.OpenKept(h.env.Dirs.State); err != nil || kept.Tagged || kept.Machine != "desktop" {
		t.Errorf("kept %+v: %v", kept, err)
	}
	if h.exists(h.env.Dirs.RestoringFlag()) {
		t.Error("restoring as you go paused backups")
	}
}

// Without a terminal, connecting restores everything now, as before.
func TestConnectWithoutATerminalRestoresNow(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.env.Prompt = ui.Defaults{}
	h.desktopSettings(testFingerprint)
	pwFile := filepath.Join(h.tmp, "pw")
	h.writeFile(pwFile, testPassword+"\n", 0o600)
	if err := h.env.Connect(context.Background(), h.repo, Options{PasswordFile: pwFile}); err != nil {
		t.Fatalf("Connect: %v\n%s", err, h.log)
	}
	if !h.exists(h.env.Dirs.RestoringFlag()) || restore.HasKept(h.env.Dirs.State) {
		t.Error("connecting without a terminal didn't pause backups for a full restore")
	}
}

func TestConnectAsYouGoNeedsASourceMachine(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.env.Prompt = ui.Defaults{}
	h.desktopSettings(testFingerprint)
	pwFile := filepath.Join(h.tmp, "pw")
	h.writeFile(pwFile, testPassword+"\n", 0o600)
	for _, c := range []struct {
		opt  Options
		want string
	}{
		{Options{PasswordFile: pwFile, SettingsFrom: "none", Restore: RestoreAsYouGo}, "--settings-from"},
		{Options{PasswordFile: pwFile, Restore: "later"}, "say now or as-you-go"},
	} {
		err := h.env.Connect(context.Background(), h.repo, c.opt)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%+v: err = %v", c.opt, err)
		}
	}
	if h.exists(h.env.Dirs.ConfigFile()) {
		t.Error("a refused connect wrote the settings")
	}
}

func removeAll(p string) error { return os.RemoveAll(p) }
