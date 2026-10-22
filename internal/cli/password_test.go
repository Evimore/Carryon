package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Evimore/carryon/internal/setup"
)

func TestPassword(t *testing.T) {
	if _, err := exec.LookPath("restic"); err != nil {
		t.Skip("restic isn't installed")
	}
	asUser(t, 0)
	dirs := fakeSetup(t)
	tmp := filepath.Dir(dirs.Conf)
	repo := filepath.Join(tmp, "backup")
	// The destination check looks for the repository's folder in fakeSetup's system root.
	if err := os.MkdirAll(filepath.Join(tmp, "root", repo), 0o755); err != nil {
		t.Fatal(err)
	}
	if out, err := run(t, dirs, "init", "--defaults", "--repo", repo, "--kit-saved"); err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}
	old, err := setup.ReadPasswordFile(dirs.PasswordFile())
	if err != nil {
		t.Fatal(err)
	}

	_, err = run(t, dirs, "password")
	if err == nil || !strings.Contains(err.Error(), "--yes") {
		t.Fatalf("without a terminal or --yes: err = %v", err)
	}

	out, err := run(t, dirs, "password", "--yes")
	if err != nil {
		t.Fatalf("password --yes: %v\n%s", err, out)
	}
	pw, err := setup.ReadPasswordFile(dirs.PasswordFile())
	if err != nil || pw == old {
		t.Fatalf("password file holds %q (%v), was %q", pw, err, old)
	}
	for _, s := range []string{"Password:      " + pw, "The repository password is changed", "Save the recovery kit soon"} {
		if !strings.Contains(out, s) {
			t.Errorf("output lacks %q:\n%s", s, out)
		}
	}
	if setup.KitConfirmed(dirs) {
		t.Error("the recovery kit still counts as saved after the change")
	}

	// recovery-kit shows the new password.
	out, err = run(t, dirs, "recovery-kit", "--saved")
	if err != nil || !strings.Contains(out, "Password:      "+pw) || !setup.KitConfirmed(dirs) {
		t.Fatalf("recovery-kit: %v\n%s", err, out)
	}

	// --kit-saved answers the kit's question.
	if out, err := run(t, dirs, "password", "--yes", "--kit-saved"); err != nil || !setup.KitConfirmed(dirs) {
		t.Fatalf("password --yes --kit-saved: %v\n%s", err, out)
	}
}

func TestPasswordNeedsRootAndSettings(t *testing.T) {
	asUser(t, 1000)
	dirs := fakeSetup(t)
	if _, err := run(t, dirs, "password"); err == nil || !strings.Contains(err.Error(), "run it with sudo") {
		t.Errorf("as a user: err = %v", err)
	}
	asUser(t, 0)
	if _, err := run(t, dirs, "password"); err == nil || !strings.Contains(err.Error(), "sudo carryon init") {
		t.Errorf("before init: err = %v", err)
	}
}
