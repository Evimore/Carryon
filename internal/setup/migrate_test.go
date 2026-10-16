package setup

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Evimore/carryon/internal/config"
	"github.com/Evimore/carryon/internal/engine"
	"github.com/Evimore/carryon/internal/ui"
)

// prototype sets up the prototype's files under SysbackupRoot, and its repository with one
// backup of desktop.
func (h *harness) prototype() *engine.Engine {
	h.t.Helper()
	old := h.env.SysbackupRoot
	h.writeFile(filepath.Join(old, "etc/sysbackup/sysbackup.conf"), `# /etc/sysbackup/sysbackup.conf
SYSBACKUP_USER=alice
RESTIC_REPOSITORY=`+h.repo+`
RESTIC_PASSWORD_FILE=/etc/sysbackup/restic-password
TOOLKIT_MIRROR=rclone:my-remote:Toolkit
EXTRA_PATHS="/root /srv /usr/local /opt/games"
KEEP="--keep-within 3d --keep-daily 10 --keep-weekly 4 --keep-monthly 12 --keep-last 5"
PRUNE_EVERY_DAYS=14
AFTER_INSTALL_DELAY=30min
MAX_AGE_HOURS=72
VERIFY_READ_SUBSET=5%
`, 0o644)
	h.writeFile(filepath.Join(old, "etc/sysbackup/restic-password"), testPassword+"\n", 0o600)
	h.writeFile(filepath.Join(old, "etc/sysbackup/excludes.txt"), "# caches & trash\n/home/*/.cache\n/root/.cache\n\n# mine\n/home/*/Videos/raw\n", 0o644)
	h.writeFile(filepath.Join(old, "etc/sysbackup/bundles.local"), "# my rules\nwhen myapp samehost\nput gaming pkg steam\n", 0o644)
	h.writeFile(filepath.Join(old, "var/lib/sysbackup/password-confirmed"), "2026-01-02T03:04:05+00:00\n", 0o644)
	h.writeFile(filepath.Join(old, "etc/pacman.d/hooks/zz-sysbackup.hook"), "[Trigger]\n", 0o644)
	h.writeFile(h.env.Dirs.DefaultExcludes(), "/home/*/.cache\n/root/.cache\n", 0o644)
	e := h.makeRepo(h.repo, testPassword)
	h.backup(e, "desktop", []string{"nightly"}, map[string]string{
		filepath.Join(h.tmp, "data/notes.txt"):                     "hello\n",
		filepath.Join(old, "var/lib/sysbackup/manifest/meta.json"): meta(testFingerprint),
	})
	return e
}

func TestFromSysbackup(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.env.Prompt = ui.Defaults{}
	e := h.prototype()
	ctx := context.Background()
	if err := h.env.FromSysbackup(ctx, Options{}); err != nil {
		t.Fatalf("FromSysbackup: %v\n%s", err, h.log)
	}
	d := h.env.Dirs

	cfg := h.config()
	want := config.Default()
	want.MachineName = "desktop"
	want.Repository = config.Repository{URL: h.repo, PasswordFile: d.PasswordFile()}
	want.Backup.Users = []string{"alice"}
	want.Backup.ExtraPaths = []string{"/root", "/srv", "/usr/local", "/opt/games"}
	want.Backup.AfterChangeDelay = "30min"
	want.Retention = config.Retention{KeepWithin: "3d", KeepDaily: 10, KeepWeekly: 4, KeepMonthly: 12, PruneEveryDays: 14}
	want.Verify.MaxAgeHours = 72
	want.Verify.ReadSubset = "5%"
	if !reflect.DeepEqual(cfg, want) {
		t.Errorf("config =\n%+v\nwant\n%+v", cfg, want)
	}
	if pw, _ := ReadPasswordFile(d.PasswordFile()); pw != testPassword {
		t.Errorf("password = %q, want the prototype's", pw)
	}
	if ex, _ := config.ReadExcludes(d.ExcludesFile()); !reflect.DeepEqual(ex, []string{"/home/*/Videos/raw"}) {
		t.Errorf("excludes = %q", ex)
	}
	if b := h.readFile(d.BundlesFile()); !strings.Contains(b, "when myapp samehost\nput gaming pkg steam\n") {
		t.Errorf("bundles = %q", b)
	}
	if id := h.identity(); id.FSUUID != testUUID {
		t.Errorf("identity = %+v", id)
	}
	if !KitConfirmed(d) {
		t.Error("the prototype's password confirmation wasn't carried over")
	}
	log := h.log.String()
	for _, s := range []string{"Backing up alice's home", "TOOLKIT_MIRROR isn't needed", "--keep-last 5", "already has 1 backups of desktop from this machine"} {
		if !strings.Contains(log, s) {
			t.Errorf("log lacks %q:\n%s", s, log)
		}
	}

	// No Carryon backup yet: the prototype keeps running, and the user learns how to stop it.
	hook := filepath.Join(h.env.SysbackupRoot, "etc/pacman.d/hooks/zz-sysbackup.hook")
	if !h.exists(hook) || slices.Contains(h.fake.Commands(), "systemctl disable --now sysbackup.timer sysbackup-verify.timer") {
		t.Fatal("the prototype was switched off before Carryon made a backup")
	}
	if out := h.out.String(); !strings.Contains(out, "sudo systemctl disable --now sysbackup.timer sysbackup-verify.timer && sudo mv /etc/pacman.d/hooks/zz-sysbackup.hook") {
		t.Errorf("output:\n%s", out)
	}

	// After Carryon's first backup, running it again offers to switch the prototype off.
	h.backup(e, "desktop", []string{"carryon"}, map[string]string{filepath.Join(h.tmp, "data/notes.txt"): "hello again\n"})
	h.fake.On("systemctl disable --now sysbackup.timer sysbackup-verify.timer", sysexecOK)
	if err := h.env.FromSysbackup(ctx, Options{}); err != nil {
		t.Fatalf("second FromSysbackup: %v\n%s", err, h.log)
	}
	if !slices.Contains(h.fake.Commands(), "systemctl disable --now sysbackup.timer sysbackup-verify.timer") {
		t.Errorf("commands = %q", h.fake.Commands())
	}
	if h.exists(hook) || !h.exists(hook+".disabled") {
		t.Error("the prototype's hook is still active")
	}
	if got := h.readFile(d.ExcludesFile()); strings.Count(got, "/home/*/Videos/raw") != 1 {
		t.Errorf("excludes repeated on the second run:\n%s", got)
	}
}

func TestFromSysbackupNeedsThePrototype(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.env.Prompt = ui.Defaults{}
	err := h.env.FromSysbackup(context.Background(), Options{})
	if err == nil || !strings.Contains(err.Error(), "no prototype settings") {
		t.Fatalf("err = %v", err)
	}

	h.prototype()
	h.writeFile(filepath.Join(h.env.SysbackupRoot, "etc/sysbackup/restic-password"), "not it\n", 0o600)
	err = h.env.FromSysbackup(context.Background(), Options{})
	if err == nil || !strings.Contains(err.Error(), "doesn't open the repository") {
		t.Fatalf("err = %v", err)
	}
	if h.exists(h.env.Dirs.ConfigFile()) {
		t.Error("settings were written with a wrong password")
	}
}

func TestConvertSysbackupDefaults(t *testing.T) {
	// The prototype's own template, with only the repository filled in, keeps Carryon's defaults.
	tmpl, err := os.ReadFile("../../legacy/sysbackup.conf")
	if err != nil {
		t.Fatal(err)
	}
	tmpl = bytes.Replace(tmpl, []byte("RESTIC_REPOSITORY=\n"), []byte("RESTIC_REPOSITORY=sftp:alice@example.com:/backups\n"), 1)
	cfg, notes := convertSysbackup(config.Default(), tmpl)
	want := config.Default()
	want.Repository.URL = "sftp:alice@example.com:/backups"
	if !reflect.DeepEqual(cfg, want) || len(notes) != 0 {
		t.Errorf("cfg = %+v, notes %q", cfg, notes)
	}

	cfg, _ = convertSysbackup(config.Default(), []byte("SYSBACKUP_USER=alice\nRESTIC_REPOSITORY='rclone:my-remote:backups'\n"))
	if cfg.Repository.RcloneUser != "alice" || cfg.Repository.URL != "rclone:my-remote:backups" {
		t.Errorf("repository = %+v", cfg.Repository)
	}
}

func TestConvertKeep(t *testing.T) {
	rt, skipped := convertKeep(config.Default().Retention, "--keep-within=1y --keep-yearly 3 --keep-tag important --keep-daily x")
	if rt.KeepWithin != "1y" || rt.KeepYearly != 3 || rt.KeepDaily != 0 || rt.PruneEveryDays != 7 {
		t.Errorf("retention = %+v", rt)
	}
	if !reflect.DeepEqual(skipped, []string{"--keep-tag important", "--keep-daily x"}) {
		t.Errorf("skipped = %q", skipped)
	}
}

func TestOwnExcludes(t *testing.T) {
	got := ownExcludes([]byte("# x\n/a\n  /b  \n/a\n\n/c\n"), []string{"/b"})
	if !reflect.DeepEqual(got, []string{"/a", "/c"}) {
		t.Errorf("got %q", got)
	}
}

func TestConvertBundlesSkipsTheExample(t *testing.T) {
	ex, err := os.ReadFile("../../legacy/bundles.local.example")
	if err != nil {
		t.Fatal(err)
	}
	if out, problems, err := convertBundles(ex, "bundles.local"); out != nil || problems != nil || err != nil {
		t.Errorf("an example-only file gave %q, %q, %v", out, problems, err)
	}
	out, problems, err := convertBundles([]byte("when x sometimes\n"), "bundles.local")
	if err != nil || out == nil || len(problems) != 1 || !strings.HasPrefix(problems[0], "bundles.local:1:") {
		t.Errorf("got %q, %q, %v", out, problems, err)
	}
}

func TestGeneratePassword(t *testing.T) {
	a, err := GeneratePassword(randReader())
	if err != nil {
		t.Fatal(err)
	}
	b, _ := GeneratePassword(randReader())
	if len(a) != PasswordLength || a == b {
		t.Errorf("passwords %q and %q", a, b)
	}
	for _, r := range a {
		if !strings.ContainsRune(passwordChars, r) {
			t.Errorf("%q has %q", a, r)
		}
	}
	// Bytes past the last whole multiple of 62 are skipped, so every character is equally likely.
	pw, err := GeneratePassword(bytes.NewReader(append(bytes.Repeat([]byte{255}, 64), bytes.Repeat([]byte{63}, 64)...)))
	if err != nil || pw != strings.Repeat("B", PasswordLength) {
		t.Errorf("got %q, %v", pw, err)
	}
}

func TestMachines(t *testing.T) {
	at := func(h int) time.Time { return time.Date(2026, 10, 1, h, 0, 0, 0, time.UTC) }
	got := machines([]engine.Snapshot{
		{ID: "a1", Hostname: "desktop", Time: at(1)},
		{ID: "b1", Hostname: "laptop", Time: at(5)},
		{ID: "a2", Hostname: "desktop", Time: at(3)},
	})
	want := []Machine{
		{Name: "laptop", Newest: at(5), Snapshot: "b1", Count: 1},
		{Name: "desktop", Newest: at(3), Snapshot: "a2", Count: 2},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v", got)
	}
}

func TestKitForAnRcloneRepository(t *testing.T) {
	var b bytes.Buffer
	Kit{URL: "rclone:my-remote:backups/carryon", MachineName: "desktop", Password: "pw", RcloneUser: "alice"}.Write(&b)
	for _, s := range []string{"rclone login:  alice's rclone remote my-remote:", "set up the same rclone remote: rclone config",
		"restic -r rclone:my-remote:backups/carryon restore latest --host desktop"} {
		if !strings.Contains(b.String(), s) {
			t.Errorf("kit lacks %q:\n%s", s, b.String())
		}
	}
}
