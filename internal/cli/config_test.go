package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Evimore/carryon/internal/config"
	"github.com/Evimore/carryon/internal/paths"
)

// asUser runs the rest of the test with the given effective user ID.
func asUser(t *testing.T, uid int) {
	t.Helper()
	old := geteuid
	geteuid = func() int { return uid }
	t.Cleanup(func() { geteuid = old })
}

// confDir makes a settings folder with a config file.
func confDir(t *testing.T, text string) paths.Dirs {
	t.Helper()
	dirs := paths.Dirs{Conf: t.TempDir(), Share: t.TempDir()}
	if err := os.WriteFile(dirs.ConfigFile(), []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	return dirs
}

func TestConfigGet(t *testing.T) {
	dirs := confDir(t, "[retention]\nkeep_daily = 10\n")
	out, err := run(t, dirs, "config", "get", "retention.keep_daily")
	if err != nil || out != "10\n" {
		t.Errorf("out = %q, err = %v", out, err)
	}
	out, err = run(t, dirs, "config", "get", "backup.extra_paths")
	if err != nil || out != "[\"/root\", \"/srv\", \"/usr/local\"]\n" {
		t.Errorf("out = %q, err = %v", out, err)
	}
	out, err = run(t, dirs, "config", "get")
	if err != nil || !strings.Contains(out, "retention.keep_daily = 10\n") || !strings.Contains(out, "backup.schedule = daily\n") {
		t.Errorf("out = %q, err = %v", out, err)
	}
	_, err = run(t, dirs, "config", "get", "retention.keep_dialy")
	if err == nil || !strings.Contains(err.Error(), `unknown setting "retention.keep_dialy"`) || !strings.Contains(err.Error(), "carryon config get") {
		t.Errorf("err = %v", err)
	}

	// Without a file, the defaults show.
	out, err = run(t, paths.Dirs{Conf: t.TempDir()}, "config", "get", "backup.after_change_delay")
	if err != nil || out != "1h\n" {
		t.Errorf("out = %q, err = %v", out, err)
	}
}

func TestConfigSet(t *testing.T) {
	asUser(t, 0)
	dirs := confDir(t, "machine_name = \"desktop\"\n")

	out, err := run(t, dirs, "config", "set", "backup.after_change_delay", "30min")
	if err != nil || !strings.Contains(out, "backup.after_change_delay: 1h -> 30min") {
		t.Fatalf("out = %q, err = %v", out, err)
	}
	out, err = run(t, dirs, "config", "set", "backup.extra_paths", "/srv, /var/lib/jellyfin")
	if err != nil || !strings.Contains(out, `-> ["/srv", "/var/lib/jellyfin"]`) {
		t.Fatalf("out = %q, err = %v", out, err)
	}
	out, err = run(t, dirs, "config", "set", "backup.after_change_delay", "30min")
	if err != nil || !strings.Contains(out, "is already 30min") {
		t.Fatalf("out = %q, err = %v", out, err)
	}

	cfg, _, err := config.Load(dirs.ConfigFile())
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MachineName != "desktop" || cfg.Backup.AfterChangeDelay != "30min" || len(cfg.Backup.ExtraPaths) != 2 {
		t.Errorf("cfg = %+v", cfg)
	}
	data, _ := os.ReadFile(dirs.ConfigFile())
	if !strings.Contains(string(data), "# Back up this long after the last package change") {
		t.Errorf("the file has no comments:\n%s", data)
	}

	for _, tt := range []struct{ key, value, want string }{
		{"backup.after_change_delay", "soon", "backup.after_change_delay"},
		{"backup.extra_paths", "/proc/self", "only exists while the system runs"},
		{"retention.keep_daily", "seven", "needs a whole number"},
		{"retention.keep_dialy", "7", "unknown setting"},
	} {
		before, _ := os.ReadFile(dirs.ConfigFile())
		_, err := run(t, dirs, "config", "set", tt.key, tt.value)
		if err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("set %s %q: err = %v, want %q", tt.key, tt.value, err, tt.want)
		}
		if after, _ := os.ReadFile(dirs.ConfigFile()); string(after) != string(before) {
			t.Errorf("set %s %q changed the file", tt.key, tt.value)
		}
	}
}

func TestChangesNeedRoot(t *testing.T) {
	asUser(t, 1000)
	dirs := confDir(t, "")
	for _, args := range [][]string{
		{"config", "set", "retention.keep_daily", "3"},
		{"add", "/srv"},
		{"exclude", "/srv/tmp"},
	} {
		_, err := run(t, dirs, args...)
		if err == nil || !strings.Contains(err.Error(), "needs root: run it with sudo") {
			t.Errorf("%q: err = %v", args, err)
		}
	}
	if _, err := os.Stat(dirs.ExcludesFile()); err == nil {
		t.Error("excludes.txt was created without root")
	}
}

func TestAdd(t *testing.T) {
	asUser(t, 0)
	dirs := confDir(t, "[backup]\nextra_paths = [\"/srv\"]\n")
	const target = "/var/lib/carryon-test-app" // never created: add only changes the config

	out, err := run(t, dirs, "add", target)
	if err != nil || !strings.Contains(out, "Added "+target+" to backup.extra_paths") {
		t.Fatalf("out = %q, err = %v", out, err)
	}
	out, err = run(t, dirs, "add", filepath.Join(target, "sub"))
	if err != nil || !strings.Contains(out, "is already backed up (backup.extra_paths has "+target+")") {
		t.Fatalf("out = %q, err = %v", out, err)
	}
	if !strings.Contains(out, "doesn't exist yet; it is backed up once it does") {
		t.Errorf("no note about a missing folder: %q", out)
	}
	cfg, _, _ := config.Load(dirs.ConfigFile())
	if got := cfg.Backup.ExtraPaths; len(got) != 2 || got[1] != target {
		t.Errorf("extra_paths = %q", got)
	}

	for _, bad := range []struct{ path, want string }{
		{"/", "would back up the whole system"},
		{"/proc/1", "only exists while the system runs"},
	} {
		if _, err := run(t, dirs, "add", bad.path); err == nil || !strings.Contains(err.Error(), bad.want) {
			t.Errorf("add %s: err = %v", bad.path, err)
		}
	}

	if _, err := run(t, paths.Dirs{Conf: t.TempDir()}, "add", "/srv"); err == nil || !strings.Contains(err.Error(), "carryon init") {
		t.Errorf("without a config: err = %v", err)
	}
}

func TestExclude(t *testing.T) {
	asUser(t, 0)
	dirs := confDir(t, "")
	if err := os.WriteFile(dirs.DefaultExcludes(), []byte("# defaults\n/home/*/.cache\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	out, err := run(t, dirs, "exclude", "/var/lib/docker")
	if err != nil || !strings.Contains(out, "Added /var/lib/docker to "+dirs.ExcludesFile()) {
		t.Fatalf("out = %q, err = %v", out, err)
	}
	out, err = run(t, dirs, "exclude", "/var/lib/docker")
	if err != nil || !strings.Contains(out, "is already in") {
		t.Fatalf("out = %q, err = %v", out, err)
	}
	out, err = run(t, dirs, "exclude", "node_modules")
	if err != nil || !strings.Contains(out, "matching names anywhere") {
		t.Fatalf("out = %q, err = %v", out, err)
	}
	out, err = run(t, dirs, "exclude", "/home/*/.cache")
	if err != nil || !strings.Contains(out, "already left out by the default excludes") {
		t.Fatalf("out = %q, err = %v", out, err)
	}

	// A relative path that exists becomes a full one.
	wd, _ := os.Getwd()
	t.Cleanup(func() { os.Chdir(wd) })
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "big"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	if out, err = run(t, dirs, "exclude", "big"); err != nil || !strings.Contains(out, "Added "+filepath.Join(dir, "big")) {
		t.Fatalf("out = %q, err = %v", out, err)
	}

	got, _ := config.ReadExcludes(dirs.ExcludesFile())
	want := []string{"/var/lib/docker", "node_modules", filepath.Join(dir, "big")}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("excludes = %q, want %q", got, want)
	}

	if _, err := run(t, dirs, "exclude", "#x"); err == nil {
		t.Error("a comment was accepted as a pattern")
	}
}
