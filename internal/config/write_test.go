package config

import (
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func TestWriteRoundTrips(t *testing.T) {
	full := Default()
	full.MachineName = "desktop"
	full.Repository = Repository{URL: "rclone:remote:Backups/carryon", PasswordCommand: `pass show "carryon\backup"`, RcloneUser: "alice"}
	full.Backup.ExtraPaths = []string{"/var/lib/jellyfin", "/srv/my files"}
	full.Backup.SkipDefaultExcludes = []string{"/home/*/.cache", "/home/*/.config/**/Code Cache"}
	full.Backup.AfterChangeDelay = "off"
	full.Retention.KeepWithin = ""
	full.Retention.KeepYearly = 2
	full.Detect.LargePathGB = 2.5
	full.Notify = Notify{Desktop: false, NtfyURL: "https://ntfy.sh/my-topic", WebhookURL: "https://example.com/hook?a=1&b=\t2"}
	full.Pacman.ExtraStandardRepos = []string{"myrepo"}

	empty := Default()
	empty.Backup.ExtraPaths = []string{}

	for name, cfg := range map[string]Config{"defaults": Default(), "everything set": full, "empty list": empty} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.toml")
			if err := Write(path, cfg); err != nil {
				t.Fatal(err)
			}
			got, warnings, err := Load(path)
			if err != nil {
				t.Fatal(err)
			}
			if len(warnings) != 0 {
				t.Errorf("warnings = %q", warnings)
			}
			if !equalConfig(got, cfg) {
				t.Errorf("round trip changed the config:\n got %+v\nwant %+v", got, cfg)
			}
		})
	}
}

// equalConfig compares configs, treating nil and empty lists alike.
func equalConfig(a, b Config) bool {
	norm := func(c Config) Config {
		for _, l := range []*[]string{&c.Backup.ExtraPaths, &c.Backup.Users, &c.Backup.SkipDefaultExcludes, &c.Pacman.ExtraStandardRepos} {
			if len(*l) == 0 {
				*l = nil
			}
		}
		return c
	}
	return reflect.DeepEqual(norm(a), norm(b))
}

func TestFormatLooksLikeTheDesign(t *testing.T) {
	data, err := Format(Default())
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	for _, want := range []string{
		"config_version = 1\n",
		"# machine_name = \"desktop\"\n",
		"\n[repository]\n",
		"# rclone_user = \"alice\"\n",
		"[backup]\n# When the regular backup runs",
		`extra_paths = ["/root", "/srv", "/usr/local"]`,
		`users = ["*"]`,
		"keep_daily = 7\n",
		"large_path_gb = 5\n",
		"desktop = true\n",
		"# ntfy_url = \"https://ntfy.sh/my-topic\"\n",
		"# extra_standard_repos = [\"myrepo\"]\n",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("file lacks %q:\n%s", want, text)
		}
	}
}

func TestLayoutHasEverySettingWithAComment(t *testing.T) {
	var inLayout []string
	for _, s := range layout {
		if s.name != "" && s.comment == "" {
			t.Errorf("section [%s] has no comment", s.name)
		}
		for _, l := range s.settings {
			key := l.key
			if s.name != "" {
				key = s.name + "." + l.key
			}
			inLayout = append(inLayout, key)
			if l.comment == "" || strings.Contains(l.comment, "\n") {
				t.Errorf("%s needs a one-line comment", key)
			}
		}
	}
	if !slices.Equal(inLayout, Keys()) {
		t.Errorf("layout writes %q\nbut the settings are %q", inLayout, Keys())
	}
}

func TestWriteKeepsModeAndSymlink(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "real.toml")
	if err := os.WriteFile(real, []byte("machine_name = \"old\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "config.toml")
	if err := os.Symlink("real.toml", link); err != nil {
		t.Fatal(err)
	}
	cfg := Default()
	cfg.MachineName = "desktop"
	if err := Write(link, cfg); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Lstat(link); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Errorf("the symlink was replaced: %v, %v", fi, err)
	}
	fi, err := os.Stat(real)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v, want 0600", fi.Mode().Perm())
	}
	if got, _, _ := Load(real); got.MachineName != "desktop" {
		t.Errorf("machine_name = %q", got.MachineName)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 2 {
		t.Errorf("leftover files: %v", entries)
	}

	fresh := filepath.Join(dir, "new.toml")
	if err := Write(fresh, cfg); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(fresh); fi.Mode().Perm() != 0o644 {
		t.Errorf("new file mode = %v, want 0644", fi.Mode().Perm())
	}
}

func TestGetAndSet(t *testing.T) {
	tests := []struct {
		key, value, want string
	}{
		{"machine_name", "desktop", "desktop"},
		{"backup.after_change_delay", "30min", "30min"},
		{"retention.keep_daily", " 10 ", "10"},
		{"detect.large_path_gb", "2.5", "2.5"},
		{"notify.desktop", "no", "false"},
		{"notify.desktop", "on", "true"},
		{"backup.extra_paths", "/srv, /opt/app ,", `["/srv", "/opt/app"]`},
		{"backup.extra_paths", `["/srv", "/var/lib/my, app"]`, `["/srv", "/var/lib/my, app"]`},
		{"backup.extra_paths", "", `[]`},
		{"backup.extra_paths", "[]", `[]`},
		{"backup.skip_default_excludes", "/home/*/.cache", `["/home/*/.cache"]`},
	}
	for _, tt := range tests {
		c := Default()
		if err := c.Set(tt.key, tt.value); err != nil {
			t.Errorf("Set(%q, %q) = %v", tt.key, tt.value, err)
			continue
		}
		if got, err := c.Get(tt.key); err != nil || got != tt.want {
			t.Errorf("after Set(%q, %q): Get = %q, %v; want %q", tt.key, tt.value, got, err, tt.want)
		}
	}
}

func TestSetRefusesBadValues(t *testing.T) {
	tests := []struct {
		key, value, want string
	}{
		{"retention.keep_daily", "seven", "needs a whole number"},
		{"retention.keep_daily", "1.5", "needs a whole number"},
		{"detect.large_path_gb", "NaN", "needs a number"},
		{"notify.desktop", "maybe", "needs true or false"},
		{"backup.extra_paths", `["/srv", 3]`, "isn't a list of quoted strings"},
		{"backup.extra_pathz", "/srv", `unknown setting "backup.extra_pathz"`},
		{"colour", "blue", `unknown setting "colour"`},
		{"backup", "x", "is a section"},
		{"backup.schedule.x", "x", "unknown setting"},
		{"config_version", "2", "managed by Carryon"},
	}
	for _, tt := range tests {
		c := Default()
		err := c.Set(tt.key, tt.value)
		if err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("Set(%q, %q) = %v, want %q", tt.key, tt.value, err, tt.want)
		}
		if !equalConfig(c, Default()) {
			t.Errorf("Set(%q, %q) changed the config on failure", tt.key, tt.value)
		}
	}
}

func TestUpdate(t *testing.T) {
	path := write(t, "# my comment\nmachine_name = \"desktop\"\n")
	notes, err := Update(path, func(c *Config) error { return c.Set("retention.keep_daily", "10") })
	if err != nil || len(notes) != 0 {
		t.Fatalf("notes = %q, err = %v", notes, err)
	}
	cfg, _, _ := Load(path)
	if cfg.Retention.KeepDaily != 10 || cfg.MachineName != "desktop" {
		t.Errorf("cfg = %+v", cfg)
	}

	before, _ := os.ReadFile(path)
	_, err = Update(path, func(c *Config) error { return c.Set("retention.keep_daily", "-1") })
	if err == nil || !strings.Contains(err.Error(), "nothing was changed") || !strings.Contains(err.Error(), "keep_daily can't be negative") {
		t.Errorf("err = %v", err)
	}
	if after, _ := os.ReadFile(path); string(after) != string(before) {
		t.Errorf("an invalid change was written")
	}
}

func TestUpdateLeavesAnUnchangedFileAlone(t *testing.T) {
	const text = "# my comment\nmachine_name = \"desktop\"\n"
	path := write(t, text)
	if _, err := Update(path, func(c *Config) error { return c.Set("machine_name", "desktop") }); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(path); string(got) != text {
		t.Errorf("file rewritten without a change:\n%s", got)
	}
}

func TestUpdateRefusesToDropUnknownSettings(t *testing.T) {
	path := write(t, "colour = \"blue\"\n")
	_, err := Update(path, func(c *Config) error { return c.Set("machine_name", "desktop") })
	if err == nil || !strings.Contains(err.Error(), `unknown setting "colour"`) {
		t.Fatalf("err = %v", err)
	}
}

func TestUpdateNeedsAFile(t *testing.T) {
	_, err := Update(filepath.Join(t.TempDir(), "config.toml"), func(*Config) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "carryon init") {
		t.Fatalf("err = %v", err)
	}
}
