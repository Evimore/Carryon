package setup

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/Evimore/carryon/internal/dest"
	"github.com/Evimore/carryon/internal/state"
	"github.com/Evimore/carryon/internal/ui"
)

func TestInitCreatesARepository(t *testing.T) {
	t.Parallel()
	h := newHarness(t,
		"generate",        // password for the new repository
		ui.UseDefault,     // machine name: the hostname
		"alice, bob",      // users
		"Mon *-*-* 03:00", // backup schedule
		ui.UseDefault,     // verify schedule
		true,              // recovery kit saved
	)
	if err := h.env.Init(context.Background(), Options{Repo: h.repo}); err != nil {
		t.Fatalf("Init: %v\n%s", err, h.log)
	}
	h.wantNoAnswersLeft()
	d := h.env.Dirs

	cfg := h.config()
	if cfg.MachineName != "desktop" || cfg.Repository.URL != h.repo || cfg.Repository.PasswordFile != d.PasswordFile() {
		t.Errorf("config: machine %q, repository %+v", cfg.MachineName, cfg.Repository)
	}
	if !reflect.DeepEqual(cfg.Backup.Users, []string{"alice", "bob"}) || cfg.Backup.Schedule != "Mon *-*-* 03:00" || cfg.Verify.Schedule != "weekly" {
		t.Errorf("config: users %q, schedules %q %q", cfg.Backup.Users, cfg.Backup.Schedule, cfg.Verify.Schedule)
	}

	pw, err := ReadPasswordFile(d.PasswordFile())
	if err != nil || len(pw) != PasswordLength {
		t.Fatalf("password file: %q, %v", pw, err)
	}
	if m := h.mode(d.PasswordFile()); m != 0o600 {
		t.Errorf("password file mode %o, want 0600", m)
	}
	if h.exists(filepath.Join(d.Conf, ".repository-password.staging")) {
		t.Error("the staging password file was left behind")
	}

	// The repository exists, the saved password opens it, and its identity is recorded.
	repoID, err := h.engineFor(h.repo, pw, "desktop").RepositoryID(context.Background())
	if err != nil {
		t.Fatalf("the saved password doesn't open the new repository: %v", err)
	}
	want := dest.Identity{FSUUID: testUUID, MountPoint: h.drive, RepoID: repoID}
	if got := h.identity(); got != want {
		t.Errorf("identity = %+v, want %+v", got, want)
	}
	if m := h.mode(state.DestinationPath(d)); m != 0o600 {
		t.Errorf("destination.json mode %o, want 0600", m)
	}

	// Schedules go into drop-ins, then the timers are switched on.
	dropIn := h.readFile(filepath.Join(h.env.SystemdDir, "carryon-backup.timer.d/schedule.conf"))
	if !strings.Contains(dropIn, "[Timer]\nOnCalendar=\nOnCalendar=Mon *-*-* 03:00\n") {
		t.Errorf("backup drop-in:\n%s", dropIn)
	}
	if v := h.readFile(filepath.Join(h.env.SystemdDir, "carryon-verify.timer.d/schedule.conf")); !strings.Contains(v, "OnCalendar=weekly\n") {
		t.Errorf("verify drop-in:\n%s", v)
	}
	wantCmds := []string{"systemctl daemon-reload", "systemctl enable --now carryon-backup.timer",
		"systemctl enable --now carryon-verify.timer", "systemctl enable --now carryon-settle.timer",
		"systemctl enable --now carryon-flatpak.path"}
	if got := h.fake.Commands(); !reflect.DeepEqual(got, wantCmds) {
		t.Errorf("commands = %q", got)
	}

	out := h.out.String()
	for _, s := range []string{"Carryon recovery kit", "Repository:    " + h.repo, "Machine name:  desktop", "Password:      " + pw,
		"sudo carryon init --connect " + h.repo, "restic -r " + h.repo + " snapshots --host desktop"} {
		if !strings.Contains(out, s) {
			t.Errorf("output lacks %q:\n%s", s, out)
		}
	}
	if got := strings.TrimSpace(h.readFile(KitConfirmedFile(d))); got != "2026-10-15T09:30:00Z" {
		t.Errorf("kit confirmed at %q", got)
	}
}

func TestInitWithoutATerminal(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.env.Prompt = ui.Defaults{}
	h.timers(1) // the units aren't installed
	if err := h.env.Init(context.Background(), Options{Repo: h.repo}); err != nil {
		t.Fatalf("Init: %v\n%s", err, h.log)
	}
	cfg := h.config()
	if cfg.MachineName != "desktop" || cfg.Backup.Schedule != "daily" || !reflect.DeepEqual(cfg.Backup.Users, []string{"*"}) {
		t.Errorf("config = %+v", cfg)
	}
	log := h.log.String()
	for _, s := range []string{
		"carryon-backup.timer, carryon-verify.timer, carryon-settle.timer, carryon-flatpak.path are not installed",
		"sudo systemctl enable --now carryon-backup.timer carryon-verify.timer carryon-settle.timer carryon-flatpak.path",
		`"sudo carryon recovery-kit" shows it again`,
	} {
		if !strings.Contains(log, s) {
			t.Errorf("log lacks %q:\n%s", s, log)
		}
	}
	if KitConfirmed(h.env.Dirs) {
		t.Error("the kit was marked saved without the user saying so")
	}
}

func TestInitAgainKeepsThePasswordAndSettings(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.env.Prompt = ui.Defaults{}
	ctx := context.Background()
	if err := h.env.Init(ctx, Options{Repo: h.repo, Users: []string{"alice"}, KitSaved: true}); err != nil {
		t.Fatal(err)
	}
	pw := h.readFile(h.env.Dirs.PasswordFile())
	id := h.identity()

	// Run again with no flags: everything comes from the saved settings, nothing is asked.
	h.script.Answers = nil
	if err := h.env.Init(ctx, Options{}); err != nil {
		t.Fatalf("second Init: %v\n%s", err, h.log)
	}
	if got := h.readFile(h.env.Dirs.PasswordFile()); got != pw {
		t.Error("init replaced the password of the existing repository")
	}
	if got := h.identity(); got != id {
		t.Errorf("identity changed: %+v, was %+v", got, id)
	}
	if cfg := h.config(); !reflect.DeepEqual(cfg.Backup.Users, []string{"alice"}) || cfg.Repository.URL != h.repo {
		t.Errorf("settings weren't kept: %+v", cfg)
	}
	if !strings.Contains(h.log.String(), "exists and the password opens it") {
		t.Errorf("log:\n%s", h.log)
	}
}

func TestInitOpensAnExistingRepository(t *testing.T) {
	t.Parallel()
	h := newHarness(t, testPassword, ui.UseDefault, ui.UseDefault, ui.UseDefault, ui.UseDefault, false)
	e := h.makeRepo(h.repo, testPassword)
	h.backup(e, "desktop", []string{"carryon"}, map[string]string{
		filepath.Join(h.env.Dirs.RecordDir(), "meta.json"): meta(testFingerprint),
	})
	if err := h.env.Init(context.Background(), Options{Repo: h.repo}); err != nil {
		t.Fatalf("Init: %v\n%s", err, h.log)
	}
	h.wantNoAnswersLeft()
	if !slices.Contains(h.asked(), "Repository password") {
		t.Errorf("asked %q, want the existing repository's password", h.asked())
	}
	if pw, _ := ReadPasswordFile(h.env.Dirs.PasswordFile()); pw != testPassword {
		t.Errorf("saved password %q", pw)
	}
	if !strings.Contains(h.log.String(), "already has 1 backups of desktop from this machine") {
		t.Errorf("log:\n%s", h.log)
	}
}

func TestInitRefusesAWrongPassword(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "not the password")
	h.makeRepo(h.repo, testPassword)
	err := h.env.Init(context.Background(), Options{Repo: h.repo})
	if err == nil || !strings.Contains(err.Error(), "doesn't open the repository") {
		t.Fatalf("err = %v", err)
	}
	if h.exists(h.env.Dirs.ConfigFile()) || h.exists(h.env.Dirs.PasswordFile()) {
		t.Error("init wrote settings although the password was wrong")
	}

	pwFile := filepath.Join(h.tmp, "wrong")
	h.writeFile(pwFile, "not the password\n", 0o600)
	err = h.env.Init(context.Background(), Options{Repo: h.repo, PasswordFile: pwFile})
	if err == nil || !strings.Contains(err.Error(), "that password doesn't open it") {
		t.Fatalf("with --password-file: err = %v", err)
	}
}

func TestInitRefusesTheNameOfAnotherMachine(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.env.Prompt = ui.Defaults{}
	pwFile := filepath.Join(h.tmp, "pw")
	h.writeFile(pwFile, testPassword+"\n", 0o600)
	e := h.makeRepo(h.repo, testPassword)
	h.backup(e, "desktop", []string{"carryon"}, map[string]string{
		filepath.Join(h.env.Dirs.RecordDir(), "meta.json"): meta("fedcba9876543210"),
	})
	err := h.env.Init(context.Background(), Options{Repo: h.repo, PasswordFile: pwFile})
	var na *ui.NeedsAnswerError
	if !errors.As(err, &na) || na.Flag != "--machine-name" {
		t.Fatalf("with the hostname as default: err = %v", err)
	}
	err = h.env.Init(context.Background(), Options{Repo: h.repo, PasswordFile: pwFile, MachineName: "desktop"})
	if err == nil || !strings.Contains(err.Error(), "another machine already keeps its backups as \"desktop\"") {
		t.Fatalf("err = %v", err)
	}
	if err := h.env.Init(context.Background(), Options{Repo: h.repo, PasswordFile: pwFile, MachineName: "desktop-2"}); err != nil {
		t.Fatalf("with another name: %v", err)
	}
}

func TestInitWithAPasswordCommand(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.env.Prompt = ui.Defaults{}
	pwFile := filepath.Join(h.tmp, "secret")
	h.writeFile(pwFile, testPassword+"\n", 0o600)
	cmd := "cat " + pwFile
	if err := h.env.Init(context.Background(), Options{Repo: h.repo, PasswordCommand: cmd}); err != nil {
		t.Fatalf("Init: %v\n%s", err, h.log)
	}
	cfg := h.config()
	if cfg.Repository.PasswordCommand != cmd || cfg.Repository.PasswordFile != "" {
		t.Errorf("repository = %+v", cfg.Repository)
	}
	if h.exists(h.env.Dirs.PasswordFile()) {
		t.Error("a password file was written although a command gives the password")
	}
	if _, err := h.engineFor(h.repo, testPassword, "desktop").RepositoryID(context.Background()); err != nil {
		t.Errorf("the command's password doesn't open the repository: %v", err)
	}
	if !strings.Contains(h.out.String(), "Carryon reads it with: "+cmd) {
		t.Errorf("kit:\n%s", h.out)
	}
}

func TestInitAsksBeforeUsingADifferentRepository(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.env.Prompt = ui.Defaults{}
	ctx := context.Background()
	if err := h.env.Init(ctx, Options{Repo: h.repo}); err != nil {
		t.Fatal(err)
	}
	// Another drive with its own repository gets mounted at the same place.
	pw, _ := ReadPasswordFile(h.env.Dirs.PasswordFile())
	if err := removeAll(h.repo); err != nil {
		t.Fatal(err)
	}
	h.makeRepo(h.repo, pw)
	err := h.env.Init(ctx, Options{})
	var na *ui.NeedsAnswerError
	if !errors.As(err, &na) || na.Flag != "--yes" {
		t.Fatalf("err = %v", err)
	}
	if err := h.env.Init(ctx, Options{Yes: true}); err != nil {
		t.Fatalf("with --yes: %v", err)
	}
}

func TestGuardDestination(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.mounts(
		"20 1 259:2 / / rw,relatime shared:1 - ext4 /dev/nvme0n1p2 rw",
		"50 20 0:61 / /home/alice/cloud rw,nosuid,nodev shared:30 - fuse.rclone my-remote:backups rw,user_id=1000",
		"51 20 0:62 / /home/alice/nas rw shared:31 - cifs //nas.example.com/share rw",
	)
	cases := []struct {
		name, url string
		yes       bool
		answers   []any
		want      string
		wantErr   string
	}{
		{name: "rclone mount, direct accepted", url: "/home/alice/cloud/carryon", answers: []any{true}, want: "rclone:my-remote:backups/carryon"},
		{name: "rclone mount, direct by default", url: "/home/alice/cloud/carryon", answers: []any{ui.UseDefault}, want: "rclone:my-remote:backups/carryon"},
		{name: "rclone mount, refused", url: "/home/alice/cloud/carryon", answers: []any{false, false}, wantErr: "isn't on the FUSE mount at /home/alice/cloud"},
		{name: "rclone mount kept", url: "/home/alice/cloud/carryon", answers: []any{false, true}, want: "/home/alice/cloud/carryon"},
		{name: "network mount", url: "/home/alice/nas/carryon", answers: []any{ui.UseDefault}, wantErr: "use --yes"},
		{name: "network mount with --yes", url: "/home/alice/nas/carryon", yes: true, want: "/home/alice/nas/carryon"},
		{name: "empty mount point", url: "/mnt/backup/carryon", answers: []any{ui.UseDefault}, wantErr: "use --yes"},
		{name: "empty mount point refused", url: "/run/media/alice/usb", answers: []any{false}, wantErr: "mount the backup drive at /run/media/alice/usb"},
		{name: "system disk elsewhere", url: "/var/backups/carryon", want: "/var/backups/carryon"},
		{name: "remote URL", url: "sftp:alice@example.com:/backups", want: "sftp:alice@example.com:/backups"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h.script.Answers, h.script.Asked = c.answers, nil
			got, err := h.env.guardDestination(c.url, c.yes)
			if c.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), c.wantErr) {
					t.Fatalf("err = %v, want %q", err, c.wantErr)
				}
				return
			}
			if err != nil || got != c.want {
				t.Fatalf("got %q, %v; want %q", got, err, c.want)
			}
			h.wantNoAnswersLeft()
		})
	}
	if !strings.Contains(h.log.String(), "/var/backups/carryon is on the system disk") {
		t.Errorf("log:\n%s", h.log)
	}
}

func TestNormalizeURL(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]string{
		"/mnt/backup/carryon/":        "/mnt/backup/carryon",
		"local:/mnt/backup":           "/mnt/backup",
		" sftp:alice@example.com:/b ": "sftp:alice@example.com:/b",
		"rclone:my-remote:backups":    "rclone:my-remote:backups",
		"backups":                     "error: full path",
		"~/backup":                    "error: ~ would be root's home",
		"":                            "error: empty",
	} {
		got, err := normalizeURL(in)
		if w, isErr := strings.CutPrefix(want, "error: "); isErr {
			if err == nil || !strings.Contains(err.Error(), w) {
				t.Errorf("normalizeURL(%q) = %q, %v; want an error with %q", in, got, err, w)
			}
			continue
		}
		if err != nil || got != want {
			t.Errorf("normalizeURL(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
}
