package config

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// userHome makes a home folder for alice with the given settings file, next to an "outside"
// folder standing in for the rest of the system.
func userHome(t *testing.T, settings string) (home, outside string) {
	t.Helper()
	base := t.TempDir()
	home = filepath.Join(base, "home", "alice")
	outside = filepath.Join(base, "etc")
	for _, d := range []string{filepath.Join(home, ".config", "carryon"), outside} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if settings != "" {
		if err := os.WriteFile(filepath.Join(home, UserConfigPath), []byte(settings), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return home, outside
}

func TestLoadUserWithoutAFile(t *testing.T) {
	home, _ := userHome(t, "")
	cfg, warnings, err := LoadUser(home, os.Getuid())
	if err != nil || len(warnings) != 0 {
		t.Fatalf("warnings = %q, err = %v", warnings, err)
	}
	if !cfg.Notify.Desktop || len(cfg.Excludes) != 0 {
		t.Errorf("cfg = %+v", cfg)
	}
}

func TestLoadUserKeepsExcludesInsideTheHome(t *testing.T) {
	home, outside := userHome(t, "")
	if err := os.MkdirAll(filepath.Join(home, "Games", "inner"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Symlinks: one leading out, one staying in.
	if err := os.Symlink(outside, filepath.Join(home, "sneaky")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(home, "Games"), filepath.Join(home, "games-link")); err != nil {
		t.Fatal(err)
	}
	settings := strings.ReplaceAll(`excludes = [
  "~/Videos",
  "Downloads/*.iso",
  "HOME/.local/share/Steam",
  "~/**/node_modules",
  "~/games-link/inner",
  "/etc/shadow",
  "/home/bob/Documents",
  "~/../bob",
  "Videos/../../bob",
  "~bob/Documents",
  "~/sneaky",
  "~/sneaky/passwd",
  "~/sneaky/*",
  "OTHER/x",
  "",
]

[notify]
desktop = false
`, "HOME", home)
	settings = strings.ReplaceAll(settings, "OTHER", home+"2")
	if err := os.WriteFile(filepath.Join(home, UserConfigPath), []byte(settings), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, warnings, err := LoadUser(home, os.Getuid())
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		home + "/Videos",
		home + "/Downloads/*.iso",
		home + "/.local/share/Steam",
		home + "/**/node_modules",
		home + "/games-link/inner",
	}
	if !slices.Equal(cfg.Excludes, want) {
		t.Errorf("excludes = %q\nwant %q", cfg.Excludes, want)
	}
	if cfg.Notify.Desktop {
		t.Error("notify.desktop should be false")
	}
	joined := strings.Join(warnings, "\n")
	for _, w := range []string{
		`"/etc/shadow" ignored: it is outside`,
		`"/home/bob/Documents" ignored: it is outside`,
		`"~/../bob" ignored: ".." isn't allowed`,
		`"Videos/../../bob" ignored: ".." isn't allowed`,
		`"~bob/Documents" ignored: only your own home folder`,
		`"~/sneaky" ignored: it leads through a symlink`,
		`"~/sneaky/passwd" ignored: it leads through a symlink`,
		`"~/sneaky/*" ignored: it leads through a symlink`,
		`2/x" ignored: it is outside`,
		`"" ignored: it is empty`,
	} {
		if !strings.Contains(joined, w) {
			t.Errorf("warnings lack %q:\n%s", w, joined)
		}
	}
	if len(warnings) != 10 {
		t.Errorf("got %d warnings, want 10:\n%s", len(warnings), joined)
	}
}

func TestLoadUserRefusesFilesLeadingOut(t *testing.T) {
	t.Run("settings file is a symlink out", func(t *testing.T) {
		home, outside := userHome(t, "")
		secret := filepath.Join(outside, "shadow")
		if err := os.WriteFile(secret, []byte("excludes = [\"~/x\"]\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(secret, filepath.Join(home, UserConfigPath)); err != nil {
			t.Fatal(err)
		}
		cfg, _, err := LoadUser(home, os.Getuid())
		if err == nil || !strings.Contains(err.Error(), "outside the home folder") {
			t.Errorf("err = %v", err)
		}
		if len(cfg.Excludes) != 0 || !cfg.Notify.Desktop {
			t.Errorf("cfg = %+v, want the defaults", cfg)
		}
	})
	t.Run("settings folder is a symlink out", func(t *testing.T) {
		home, outside := userHome(t, "")
		if err := os.RemoveAll(filepath.Join(home, ".config")); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(outside, "carryon"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(outside, "carryon", "config.toml"), []byte("[notify]\ndesktop = false\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, filepath.Join(home, ".config")); err != nil {
			t.Fatal(err)
		}
		if _, _, err := LoadUser(home, os.Getuid()); err == nil || !strings.Contains(err.Error(), "outside the home folder") {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("not a regular file", func(t *testing.T) {
		home, _ := userHome(t, "")
		if err := os.Mkdir(filepath.Join(home, UserConfigPath), 0o755); err != nil {
			t.Fatal(err)
		}
		if _, _, err := LoadUser(home, os.Getuid()); err == nil || !strings.Contains(err.Error(), "isn't a regular file") {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("someone else's file", func(t *testing.T) {
		home, _ := userHome(t, "[notify]\ndesktop = false\n")
		uid := os.Getuid() + 1
		if os.Getuid() == 0 { // root's own files are accepted, so give the file to a third user
			if err := os.Lchown(filepath.Join(home, UserConfigPath), 4242, 4242); err != nil {
				t.Fatal(err)
			}
			uid = 4343
		}
		if _, _, err := LoadUser(home, uid); err == nil || !strings.Contains(err.Error(), "belongs to another user") {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("too big", func(t *testing.T) {
		home, _ := userHome(t, "# "+strings.Repeat("x", maxUserConfigSize)+"\n")
		if _, _, err := LoadUser(home, os.Getuid()); err == nil || !strings.Contains(err.Error(), "bigger than") {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("relative home", func(t *testing.T) {
		if _, _, err := LoadUser("home/alice", os.Getuid()); err == nil {
			t.Error("want an error")
		}
	})
}

func TestLoadUserReportsMistakes(t *testing.T) {
	home, _ := userHome(t, "excludes = [\"~/x\"]\ncolour = 1\n")
	cfg, warnings, err := LoadUser(home, os.Getuid())
	if err != nil {
		t.Fatal(err)
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], `unknown setting "colour"`) || len(cfg.Excludes) != 1 {
		t.Errorf("cfg = %+v, warnings = %q", cfg, warnings)
	}

	home, _ = userHome(t, "excludes = \"~/x\"\n")
	cfg, _, err = LoadUser(home, os.Getuid())
	if err == nil || len(cfg.Excludes) != 0 {
		t.Errorf("cfg = %+v, err = %v", cfg, err)
	}
}
