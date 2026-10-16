package setup

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/Evimore/carryon/internal/config"
	"github.com/Evimore/carryon/internal/ui"
)

// desktopSettings backs up a Carryon machine named desktop with its settings, as T11 lays a
// backup out, and removes them from this machine.
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
		d.ConfigFile():     string(text),
		d.ExcludesFile():   "# mine\n/home/*/Videos\n",
		d.BundlesFile():    "put gaming pkg steam\n",
		d.PasswordFile():   "the other machine's password\n",
		DestinationFile(d): `{"mount_point":"/mnt/other","repo_id":"other"}`,
		filepath.Join(d.RecordDir(), "meta.json"): meta(fingerprint),
	})
}

func TestConnectOnAReinstalledMachine(t *testing.T) {
	t.Parallel()
	h := newHarness(t,
		testPassword,  // repository password
		ui.UseDefault, // reuse settings of: this machine
		ui.UseDefault, // machine name: the one it had
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
	if len(h.fake.Commands()) != 4 {
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
	h := newHarness(t, testPassword, "desktop", ui.UseDefault)
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

func TestConnectReusesThePrototypesSettings(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.env.Prompt = ui.Defaults{}
	e := h.makeRepo(h.repo, testPassword)
	old := filepath.Join(h.env.SysbackupRoot, "etc/sysbackup")
	h.writeFile(h.env.Dirs.DefaultExcludes(), "/home/*/.cache\n", 0o644)
	h.backup(e, "desktop", []string{"nightly"}, map[string]string{
		filepath.Join(old, "sysbackup.conf"):                                       "SYSBACKUP_USER=alice\nRESTIC_REPOSITORY=" + h.repo + "\nEXTRA_PATHS=\"/root /srv\"\n",
		filepath.Join(old, "excludes.txt"):                                         "# caches\n/home/*/.cache\n/home/*/Downloads\n",
		filepath.Join(old, "bundles.local"):                                        "# examples\nput gaming pkg steam\n",
		filepath.Join(h.env.SysbackupRoot, "var/lib/sysbackup/manifest/meta.json"): meta(testFingerprint),
	})
	pwFile := filepath.Join(h.tmp, "pw")
	h.writeFile(pwFile, testPassword+"\n", 0o600)
	if err := h.env.Connect(context.Background(), h.repo, Options{PasswordFile: pwFile}); err != nil {
		t.Fatalf("Connect: %v\n%s", err, h.log)
	}
	cfg := h.config()
	if cfg.MachineName != "desktop" || !reflect.DeepEqual(cfg.Backup.Users, []string{"alice"}) || !reflect.DeepEqual(cfg.Backup.ExtraPaths, []string{"/root", "/srv"}) {
		t.Errorf("config = %+v", cfg)
	}
	ex, _ := config.ReadExcludes(h.env.Dirs.ExcludesFile())
	if !reflect.DeepEqual(ex, []string{"/home/*/Downloads"}) {
		t.Errorf("excludes = %q, want only the user's own", ex)
	}
	if b := h.readFile(h.env.Dirs.BundlesFile()); !strings.Contains(b, "put gaming pkg steam") {
		t.Errorf("bundles = %q", b)
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
	h := newHarness(t, testPassword, ui.UseDefault, ui.UseDefault)
	h.desktopSettings(testFingerprint)
	h.writeFile(h.env.Dirs.ExcludesFile(), "/home/*/mine\n", 0o644)
	if err := h.env.Connect(context.Background(), h.repo, Options{}); err != nil {
		t.Fatalf("Connect: %v\n%s", err, h.log)
	}
	if got := h.readFile(h.env.Dirs.ExcludesFile() + ".before-desktop"); got != "/home/*/mine\n" {
		t.Errorf("kept %q", got)
	}
}

func removeAll(p string) error { return os.RemoveAll(p) }
