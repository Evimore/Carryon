package setup

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/Evimore/carryon/internal/config"
	"github.com/Evimore/carryon/internal/engine"
	"github.com/Evimore/carryon/internal/paths"
	"github.com/Evimore/carryon/internal/state"
	"github.com/Evimore/carryon/internal/ui"
)

// setUp sets up backups on h's machine with a generated password and the kit saved, and returns
// the password. Output and log start empty again afterwards.
func (h *harness) setUp() string {
	h.t.Helper()
	prompt := h.env.Prompt
	h.env.Prompt = ui.Defaults{}
	if err := h.env.Init(context.Background(), Options{Repo: h.repo, KitSaved: true}); err != nil {
		h.t.Fatalf("Init: %v\n%s", err, h.log)
	}
	h.env.Prompt = prompt
	h.out, h.log = &syncBuffer{}, &syncBuffer{}
	h.env.Out, h.env.Log = h.out, ui.NewLogger(h.log, false, false)
	pw, err := ReadPasswordFile(h.env.Dirs.PasswordFile())
	if err != nil {
		h.t.Fatal(err)
	}
	return pw
}

// opens tells whether password opens h's repository.
func (h *harness) opens(password string) bool {
	h.t.Helper()
	_, err := h.engineFor(h.repo, password, "check").RepositoryID(context.Background())
	var re *engine.Error
	if errors.As(err, &re) && re.Code == engine.ExitBadPassword {
		return false
	}
	if err != nil {
		h.t.Fatalf("opening the repository: %v", err)
	}
	return true
}

// failKeyPasswd makes restic key passwd fail, before it changes the key or after.
func (h *harness) failKeyPasswd(afterChange bool) {
	base := h.env.Engine
	h.env.Engine = func(repo config.Repository, machine string) *engine.Engine {
		e := base(repo, machine)
		e.Runner = func(ctx context.Context, c engine.Cmd) (int, error) {
			if !slices.Contains(c.Args, "passwd") {
				return engine.ExecRunner(ctx, c)
			}
			if afterChange {
				if code, err := engine.ExecRunner(ctx, c); code != 0 || err != nil {
					return code, err
				}
			}
			fmt.Fprintln(c.Stderr, "Fatal: unable to remove key: permission denied")
			return 1, nil
		}
		return e
	}
}

// staged is where the new password waits while it's changed.
func (h *harness) staged() string { return h.env.Dirs.PasswordFile() + ".new" }

func TestChangePassword(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	old := h.setUp()
	h.script.Answers = []any{true, true} // change it; the new kit is saved
	if err := h.env.ChangePassword(context.Background(), h.config(), PasswordOptions{}); err != nil {
		t.Fatalf("ChangePassword: %v\n%s", err, h.log)
	}
	h.wantNoAnswersLeft()
	if got, want := h.asked(), []string{"Change the repository password?", "Have you saved the recovery kit?"}; !reflect.DeepEqual(got, want) {
		t.Errorf("asked %q, want %q", got, want)
	}
	file := h.env.Dirs.PasswordFile()
	pw, err := ReadPasswordFile(file)
	if err != nil || len(pw) != PasswordLength || pw == old {
		t.Fatalf("password file holds %q (%v), was %q", pw, err, old)
	}
	if m := h.mode(file); m != 0o600 {
		t.Errorf("password file mode %o, want 0600", m)
	}
	if got := h.readFile(file + ".old"); got != old+"\n" {
		t.Errorf(".old holds %q, want the old password", got)
	}
	if h.exists(h.staged()) {
		t.Error("the staged password was left behind")
	}
	if h.opens(old) {
		t.Error("the old password still opens the repository")
	}
	if !h.opens(pw) {
		t.Error("the new password doesn't open the repository")
	}
	out := h.out.String()
	for _, s := range []string{"Carryon recovery kit", "Password:      " + pw, "The repository password is changed, and saved in " + file} {
		if !strings.Contains(out, s) {
			t.Errorf("output lacks %q:\n%s", s, out)
		}
	}
	if strings.Contains(out, old) || strings.Contains(h.log.String(), "also holds") {
		t.Errorf("output names the old password or other machines:\n%s\n%s", out, h.log)
	}
	if !KitConfirmed(h.env.Dirs) {
		t.Error("the new kit wasn't noted as saved")
	}
}

func TestChangePasswordWithoutATerminal(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	old := h.setUp()
	h.env.Prompt = ui.Defaults{}
	ctx := context.Background()

	err := h.env.ChangePassword(ctx, h.config(), PasswordOptions{})
	var na *ui.NeedsAnswerError
	if !errors.As(err, &na) || na.Flag != "--yes" {
		t.Fatalf("without --yes: err = %v", err)
	}
	if pw, _ := ReadPasswordFile(h.env.Dirs.PasswordFile()); pw != old || h.exists(h.staged()) {
		t.Fatal("the password changed without --yes")
	}

	if err := h.env.ChangePassword(ctx, h.config(), PasswordOptions{Yes: true}); err != nil {
		t.Fatalf("with --yes: %v\n%s", err, h.log)
	}
	if pw, _ := ReadPasswordFile(h.env.Dirs.PasswordFile()); pw == old || !h.opens(pw) {
		t.Errorf("the password file holds %q", pw)
	}
	// The kit was saved before, but not the new one.
	if KitConfirmed(h.env.Dirs) {
		t.Error("the recovery kit still counts as saved after the change")
	}
	if !strings.Contains(h.log.String(), saveKitSoon) {
		t.Errorf("log:\n%s", h.log)
	}
}

func TestChangePasswordFromAFile(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	old := h.setUp()
	ctx := context.Background()
	from := filepath.Join(h.tmp, "new-password")
	h.writeFile(from, "a new password of my own\n", 0o600)
	o := PasswordOptions{PasswordFile: from, Yes: true, KitSaved: true}
	if err := h.env.ChangePassword(ctx, h.config(), o); err != nil {
		t.Fatalf("ChangePassword: %v\n%s", err, h.log)
	}
	if pw, _ := ReadPasswordFile(h.env.Dirs.PasswordFile()); pw != "a new password of my own" {
		t.Errorf("password file holds %q", pw)
	}
	if h.opens(old) || !h.opens("a new password of my own") {
		t.Error("the repository didn't get the password from the file")
	}
	if !KitConfirmed(h.env.Dirs) {
		t.Error("--kit-saved didn't note the new kit as saved")
	}

	err := h.env.ChangePassword(ctx, h.config(), o)
	if err == nil || !strings.Contains(err.Error(), "holds the current password") {
		t.Fatalf("the same password again: err = %v", err)
	}
}

func TestChangePasswordWarnsAboutOtherMachines(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	old := h.setUp()
	for _, name := range []string{"laptop", "server", "desktop"} {
		h.backup(h.engineFor(h.repo, old, name), name, []string{"carryon"}, map[string]string{filepath.Join(h.tmp, "file-"+name): name})
	}
	h.env.Prompt = ui.Defaults{}
	ctx := context.Background()

	// Without a terminal, the warning comes before anything changes.
	err := h.env.ChangePassword(ctx, h.config(), PasswordOptions{})
	var na *ui.NeedsAnswerError
	if !errors.As(err, &na) || na.Flag != "--yes" {
		t.Fatalf("err = %v", err)
	}
	if !h.opens(old) {
		t.Fatal("the password changed without --yes")
	}
	log := h.log.String()
	for _, s := range []string{"The repository also holds the backups of server and laptop.", `run "sudo carryon init" there`} {
		if !strings.Contains(log, s) {
			t.Errorf("log lacks %q:\n%s", s, log)
		}
	}

	if err := h.env.ChangePassword(ctx, h.config(), PasswordOptions{Yes: true}); err != nil {
		t.Fatalf("with --yes: %v\n%s", err, h.log)
	}
	if want := `Save the new password on server and laptop too: run "sudo carryon init" there`; !strings.Contains(h.out.String(), want) {
		t.Errorf("output lacks %q:\n%s", want, h.out)
	}
}

// The way the warning tells to save the new password on another machine works.
func TestAnotherMachineTakesTheNewPasswordWithInit(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	old := h.setUp()
	ctx := context.Background()

	dir := filepath.Join(h.tmp, "laptop")
	dirs := paths.Dirs{
		Conf: filepath.Join(dir, "etc/carryon"), State: filepath.Join(dir, "var/lib/carryon"),
		Cache: filepath.Join(dir, "var/cache/carryon"), Run: filepath.Join(dir, "run/carryon"),
		Share: filepath.Join(dir, "usr/share/carryon"),
	}
	laptopLog := &syncBuffer{}
	laptop := *h.env
	laptop.Dirs = dirs
	laptop.Prompt = ui.Defaults{}
	laptop.Log, laptop.Out = ui.NewLogger(laptopLog, false, false), io.Discard
	laptop.SystemdDir = filepath.Join(dir, "etc/systemd/system")
	laptop.Hostname = func() (string, error) { return "laptop", nil }
	laptop.Fingerprint = func() string { return "fedcba9876543210" }
	laptop.Engine = func(repo config.Repository, machine string) *engine.Engine {
		e := engine.New(repo, dirs, machine)
		e.BaseEnv = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + dir}
		e.Stderr = io.Discard
		return e
	}
	oldFile := filepath.Join(h.tmp, "old-password")
	h.writeFile(oldFile, old+"\n", 0o600)
	if err := laptop.Init(ctx, Options{Repo: h.repo, PasswordFile: oldFile, KitSaved: true}); err != nil {
		t.Fatalf("setting up the laptop: %v\n%s", err, laptopLog)
	}

	if err := h.env.ChangePassword(ctx, h.config(), PasswordOptions{Yes: true, KitSaved: true}); err != nil {
		t.Fatalf("ChangePassword: %v\n%s", err, h.log)
	}
	pw, _ := ReadPasswordFile(h.env.Dirs.PasswordFile())

	// sudo carryon init on the laptop, entering the new password and keeping the rest.
	script := &ui.Script{Answers: []any{
		ui.UseDefault, // where backups go
		pw,            // the repository's password, since the saved one doesn't open it
		ui.UseDefault, // machine name
		ui.UseDefault, // users
		ui.UseDefault, // backup schedule
		ui.UseDefault, // verify schedule
	}}
	laptop.Prompt = script
	if err := laptop.Init(ctx, Options{}); err != nil {
		t.Fatalf("init on the laptop: %v\n%s", err, laptopLog)
	}
	if len(script.Answers) > 0 {
		t.Errorf("answers left over: %v", script.Answers)
	}
	if got, _ := ReadPasswordFile(dirs.PasswordFile()); got != pw {
		t.Errorf("the laptop saved %q, want the new password", got)
	}
	if !strings.Contains(laptopLog.String(), "The saved password doesn't open the repository") {
		t.Errorf("laptop log:\n%s", laptopLog)
	}
}

func TestChangePasswordWithAPasswordCommand(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.env.Prompt = ui.Defaults{}
	ctx := context.Background()
	secret := filepath.Join(h.tmp, "secret")
	h.writeFile(secret, testPassword+"\n", 0o600)
	cmd := "cat " + secret
	if err := h.env.Init(ctx, Options{Repo: h.repo, PasswordCommand: cmd, KitSaved: true}); err != nil {
		t.Fatalf("Init: %v\n%s", err, h.log)
	}

	if err := h.env.ChangePassword(ctx, h.config(), PasswordOptions{Yes: true}); err != nil {
		t.Fatalf("ChangePassword: %v\n%s", err, h.log)
	}
	if h.opens(testPassword) {
		t.Error("the command's password still opens the repository")
	}
	// The staged file keeps the new password until the user has changed what the command reads.
	pw, err := ReadPasswordFile(h.staged())
	if err != nil || !h.opens(pw) {
		t.Fatalf("staged password %q (%v) doesn't open the repository", pw, err)
	}
	if m := h.mode(h.staged()); m != 0o600 {
		t.Errorf("staged password mode %o, want 0600", m)
	}
	if h.exists(h.env.Dirs.PasswordFile()) {
		t.Error("a password file was written although a command gives the password")
	}
	if cfg := h.config(); cfg.Repository.PasswordCommand != cmd {
		t.Errorf("repository = %+v", cfg.Repository)
	}
	out := h.out.String()
	for _, s := range []string{"Password:      " + pw, "Carryon reads it with: " + cmd, "That still gives the old password", h.staged()} {
		if !strings.Contains(out, s) {
			t.Errorf("output lacks %q:\n%s", s, out)
		}
	}
	if KitConfirmed(h.env.Dirs) {
		t.Error("the recovery kit still counts as saved after the change")
	}

	// Run again before the command gives the new password: it says the same, and changes nothing.
	if err := h.env.ChangePassword(ctx, h.config(), PasswordOptions{}); err != nil {
		t.Fatalf("again: %v\n%s", err, h.log)
	}
	if !strings.Contains(h.log.String(), "from a change that didn't finish") || !h.opens(pw) {
		t.Errorf("log:\n%s", h.log)
	}
}

func TestChangePasswordKeepsThePasswordWhenResticFails(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	old := h.setUp()
	file := h.env.Dirs.PasswordFile()
	before := h.readFile(file)
	h.failKeyPasswd(false)
	err := h.env.ChangePassword(context.Background(), h.config(), PasswordOptions{Yes: true, KitSaved: true})
	if err == nil || !strings.Contains(err.Error(), "restic couldn't change the password: unable to remove key: permission denied") ||
		!strings.Contains(err.Error(), "The password is still the one in "+file) {
		t.Fatalf("err = %v", err)
	}
	if got := h.readFile(file); got != before {
		t.Errorf("the password file changed to %q", got)
	}
	if h.exists(file+".old") || h.exists(h.staged()) {
		t.Error("a failed change left .old or the staged password behind")
	}
	if !h.opens(old) {
		t.Error("the old password no longer opens the repository")
	}
	if !KitConfirmed(h.env.Dirs) {
		t.Error("a failed change made the saved kit count as unsaved")
	}
}

func TestChangePasswordWhenResticFailsAfterTheChange(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	old := h.setUp()
	h.failKeyPasswd(true)
	if err := h.env.ChangePassword(context.Background(), h.config(), PasswordOptions{Yes: true, KitSaved: true}); err != nil {
		t.Fatalf("ChangePassword: %v\n%s", err, h.log)
	}
	pw, _ := ReadPasswordFile(h.env.Dirs.PasswordFile())
	if pw == old || !h.opens(pw) || h.opens(old) {
		t.Errorf("the password file holds %q, which doesn't open the repository", pw)
	}
	if !strings.Contains(h.log.String(), "so the change went through") {
		t.Errorf("log:\n%s", h.log)
	}
}

func TestChangePasswordFinishesAnInterruptedChange(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	old := h.setUp()
	// restic changed the key, then Carryon stopped before saving the new password.
	h.writeFile(h.staged(), "the interrupted new password\n", 0o600)
	h.restic(h.engineFor(h.repo, old, "desktop"), "key", "passwd", "--new-password-file", h.staged())

	h.env.Prompt = ui.Defaults{} // nothing to confirm: the repository changed already
	if err := h.env.ChangePassword(context.Background(), h.config(), PasswordOptions{}); err != nil {
		t.Fatalf("ChangePassword: %v\n%s", err, h.log)
	}
	if pw, _ := ReadPasswordFile(h.env.Dirs.PasswordFile()); pw != "the interrupted new password" {
		t.Errorf("password file holds %q", pw)
	}
	if h.exists(h.staged()) || KitConfirmed(h.env.Dirs) {
		t.Error("the staged password was left behind, or the kit counts as saved")
	}
	if !strings.Contains(h.log.String(), "from a change that didn't finish") {
		t.Errorf("log:\n%s", h.log)
	}
}

func TestChangePasswordRefuses(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name    string
		prepare func(h *harness) (undo func())
		want    string
	}{
		{"drive not mounted", func(h *harness) func() {
			h.mounts("20 1 259:2 / / rw,relatime shared:1 - ext4 /dev/nvme0n1p2 rw")
			return func() {}
		}, "The backup drive isn't mounted"},
		{"a backup is running", func(h *harness) func() {
			unlock, err := state.Lock(h.env.Dirs)
			if err != nil {
				h.t.Fatal(err)
			}
			return unlock
		}, "a backup is running"},
		{"wrong password", func(h *harness) func() {
			h.writeFile(h.env.Dirs.PasswordFile(), "not the password\n", 0o600)
			return func() {}
		}, "wrong password"},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t)
			h.setUp()
			undo := c.prepare(h)
			defer undo()
			before := h.readFile(h.env.Dirs.PasswordFile())
			err := h.env.ChangePassword(context.Background(), h.config(), PasswordOptions{Yes: true})
			if err == nil || !strings.Contains(err.Error(), c.want) || !strings.Contains(err.Error(), "The password wasn't changed") {
				t.Fatalf("err = %v, want %q", err, c.want)
			}
			if h.readFile(h.env.Dirs.PasswordFile()) != before || h.exists(h.staged()) {
				t.Error("the password file changed, or a staged password was left behind")
			}
		})
	}
}

// unplugOnConfirm says yes, after the backup drive was unplugged while the question waited.
type unplugOnConfirm struct {
	ui.Defaults
	h *harness
}

func (u unplugOnConfirm) Confirm(ui.Confirm) (bool, error) {
	u.h.mounts("20 1 259:2 / / rw,relatime shared:1 - ext4 /dev/nvme0n1p2 rw")
	return true, nil
}

func TestChangePasswordChecksTheDriveAgainAfterAsking(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	old := h.setUp()
	h.env.Prompt = unplugOnConfirm{h: h}
	err := h.env.ChangePassword(context.Background(), h.config(), PasswordOptions{})
	if err == nil || !strings.Contains(err.Error(), "The backup drive isn't mounted") {
		t.Fatalf("err = %v", err)
	}
	if pw, _ := ReadPasswordFile(h.env.Dirs.PasswordFile()); pw != old || h.exists(h.staged()) || !h.opens(old) {
		t.Error("the password changed although the drive was gone")
	}
}

func TestAndList(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		in   []string
		want string
	}{
		{nil, ""},
		{[]string{"laptop"}, "laptop"},
		{[]string{"laptop", "server"}, "laptop and server"},
		{[]string{"laptop", "server", "nas"}, "laptop, server and nas"},
	} {
		if got := andList(c.in); got != c.want {
			t.Errorf("andList(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
