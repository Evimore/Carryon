package cli

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Evimore/carryon/internal/paths"
)

func asRoot(t *testing.T, root bool) {
	t.Helper()
	old := geteuid
	geteuid = func() int {
		if root {
			return 0
		}
		return 1000
	}
	t.Cleanup(func() { geteuid = old })
}

func TestResticNeedsRoot(t *testing.T) {
	asRoot(t, false)
	_, err := run(t, paths.Dirs{Conf: t.TempDir()}, "restic", "snapshots")
	if err == nil || !strings.Contains(err.Error(), "sudo") {
		t.Fatalf("err = %v", err)
	}
}

func TestResticNeedsSetup(t *testing.T) {
	asRoot(t, true)
	_, err := run(t, paths.Dirs{Conf: t.TempDir()}, "restic", "snapshots")
	if err == nil || !strings.Contains(err.Error(), "carryon init") {
		t.Fatalf("err = %v", err)
	}
}

// resticSetup is a config for a local repository in a temp folder, not created yet. It returns the
// folders and the repository's path.
func resticSetup(t *testing.T) (paths.Dirs, string) {
	t.Helper()
	if _, err := exec.LookPath("restic"); err != nil {
		t.Skip("restic isn't installed")
	}
	asRoot(t, true)
	tmp := t.TempDir()
	dirs := paths.Dirs{Conf: filepath.Join(tmp, "conf"), Cache: filepath.Join(tmp, "cache")}
	if err := os.MkdirAll(dirs.Conf, 0o700); err != nil {
		t.Fatal(err)
	}
	repo := filepath.Join(tmp, "repo")
	conf := "machine_name = \"desktop\"\n[repository]\nurl = \"" + repo + "\"\n"
	if err := os.WriteFile(dirs.ConfigFile(), []byte(conf), 0o600); err != nil {
		t.Fatal(err)
	}
	// the password comes from the default place
	if err := os.WriteFile(dirs.PasswordFile(), []byte("correct horse\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return dirs, repo
}

func TestResticPassthrough(t *testing.T) {
	dirs, _ := resticSetup(t)
	if out, err := run(t, dirs, "restic", "init"); err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}
	out, err := run(t, dirs, "restic", "snapshots", "--json")
	if err != nil || strings.TrimSpace(out) != "[]" {
		t.Fatalf("snapshots: %v\n%s", err, out)
	}
	// flags reach restic, not cobra
	if out, err := run(t, dirs, "restic", "--help"); err != nil || !strings.Contains(out, "restic is a backup program") {
		t.Errorf("--help: %v\n%s", err, out)
	}

	if err := os.WriteFile(dirs.PasswordFile(), []byte("wrong\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err = run(t, dirs, "restic", "snapshots")
	var exit exitError
	if !errors.As(err, &exit) || exit.code != 12 {
		t.Errorf("wrong password: err = %v\n%s", err, out)
	}
}

func TestRcloneShimIsHidden(t *testing.T) {
	out, err := run(t, paths.Dirs{}, "--help")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "rclone-shim") || !strings.Contains(out, "restic") {
		t.Errorf("help:\n%s", out)
	}
}
