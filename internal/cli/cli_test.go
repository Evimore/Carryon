package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Evimore/carryon/internal/paths"
)

func run(t *testing.T, dirs paths.Dirs, args ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	root := NewRoot(&out, dirs)
	root.SetArgs(args)
	root.SetErr(&out)
	err := root.Execute()
	return out.String(), err
}

func TestBundlesReadsAPrototypeRecord(t *testing.T) {
	out, err := run(t, paths.Dirs{}, "bundles", "--record", "../record/testdata/format1")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"System record of desktop (18 items in 14 bundles, format 1)",
		"Machine identity: hostname, SSH host keys, Tailscale node   identity · when samehost",
		"      package    fuse3  [extra]  (as dependency)",
		"      backup     ~/.config/kitty",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
}

func TestBundlesExplainsAMissingRecord(t *testing.T) {
	_, err := run(t, paths.Dirs{State: t.TempDir()}, "bundles")
	if err == nil || !strings.Contains(err.Error(), "can't read the system record") {
		t.Fatalf("err = %v", err)
	}
}

func TestConfigCheck(t *testing.T) {
	dir := t.TempDir()
	dirs := paths.Dirs{Conf: dir}
	if err := os.WriteFile(dirs.ConfigFile(), []byte("[repository]\nurl = \"/mnt/backup\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := run(t, dirs, "config", "check")
	if err != nil || !strings.Contains(out, "is valid") {
		t.Fatalf("out = %q, err = %v", out, err)
	}

	bad := filepath.Join(dir, "bad.toml")
	if err := os.WriteFile(bad, []byte("[detect]\nnew_paths = \"maybe\"\ncolour = 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err = run(t, dirs, "config", "check", "--file", bad)
	if err == nil {
		t.Fatal("want a failing exit")
	}
	if !strings.Contains(out, `detect.new_paths "maybe"`) || !strings.Contains(out, `unknown setting "detect.colour"`) {
		t.Errorf("out = %q", out)
	}
}

func TestPlannedCommandsSayWhenTheyArrive(t *testing.T) {
	_, err := run(t, paths.Dirs{}, "inbox")
	if err == nil || !strings.Contains(err.Error(), "not implemented yet (roadmap: v0.2)") {
		t.Fatalf("err = %v", err)
	}
}

func TestVersion(t *testing.T) {
	out, err := run(t, paths.Dirs{}, "version")
	if err != nil || !strings.HasPrefix(out, "carryon ") {
		t.Fatalf("out = %q, err = %v", out, err)
	}
}
