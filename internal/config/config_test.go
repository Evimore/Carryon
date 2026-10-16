package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestDefaultsAreValid(t *testing.T) {
	if err := Default().Validate(); err != nil {
		t.Fatalf("defaults don't validate: %v", err)
	}
}

func TestLoadKeepsDefaultsForMissingSettings(t *testing.T) {
	path := write(t, `
machine_name = "desktop"

[repository]
url = "/mnt/backup/carryon"

[backup]
extra_paths = ["/var/lib/jellyfin"]
`)
	cfg, warnings, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(warnings) != 0 {
		t.Errorf("warnings = %q", warnings)
	}
	if cfg.MachineName != "desktop" || cfg.Repository.URL != "/mnt/backup/carryon" {
		t.Errorf("cfg = %+v", cfg)
	}
	if got := cfg.Backup.ExtraPaths; len(got) != 1 || got[0] != "/var/lib/jellyfin" {
		t.Errorf("extra_paths = %q (a list in the file replaces the default)", got)
	}
	if cfg.Backup.AfterChangeDelay != "1h" || cfg.Retention.KeepDaily != 7 || cfg.Verify.ReadSubset != "2%" {
		t.Errorf("defaults lost: %+v", cfg)
	}
	if cfg.ConfigVersion != CurrentVersion {
		t.Errorf("a file without config_version counts as the current version, got %d", cfg.ConfigVersion)
	}
	if err := cfg.Validate(); err != nil {
		t.Errorf("Validate() = %v", err)
	}
}

func TestLoadWarnsAboutUnknownSettings(t *testing.T) {
	path := write(t, "colour = \"blue\"\n[backup]\nschedul = \"daily\"\n")
	_, warnings, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(warnings, "\n")
	if !strings.Contains(joined, `unknown setting "colour"`) || !strings.Contains(joined, `unknown setting "backup.schedul"`) {
		t.Errorf("warnings = %q", warnings)
	}
}

func TestLoadReportsSyntaxErrors(t *testing.T) {
	path := write(t, "[backup\n")
	if _, _, err := Load(path); err == nil || !strings.Contains(err.Error(), path) {
		t.Fatalf("err = %v, want one naming the file", err)
	}
}

func TestValidateFindsEveryProblem(t *testing.T) {
	c := Default()
	c.ConfigVersion = 9
	c.MachineName = "my desktop"
	c.Repository = Repository{URL: "backups/here", PasswordFile: "pw", PasswordCommand: "pass show x"}
	c.Backup.AfterChangeDelay = "soon"
	c.Backup.ExtraPaths = []string{"var/lib/x", "/", "/proc/1", "/var/lib/ok"}
	c.Backup.Users = nil
	c.Retention = Retention{KeepWithin: "2 days", KeepDaily: -1, PruneEveryDays: 0}
	c.Verify = Verify{Schedule: "", MaxAgeHours: 0, ReadSubset: "150%"}
	c.Detect = Detect{NewPaths: "maybe", LargePathGB: -1}
	c.Notify.NtfyURL = "ntfy.sh/topic"

	err := c.Validate()
	if err == nil {
		t.Fatal("want errors")
	}
	for _, want := range []string{
		"config_version 9",
		`machine_name "my desktop"`,
		`repository.url "backups/here"`,
		"password_file or password_command, not both",
		`password_file "pw" must be an absolute path`,
		"backup.after_change_delay",
		`"var/lib/x" isn't an absolute path`,
		`"/" would back up the whole system`,
		`"/proc/1" is in a folder that only exists while the system runs`,
		"backup.users is empty",
		`retention.keep_within "2 days"`,
		"retention.keep_daily can't be negative",
		"retention.prune_every_days must be at least 1",
		"verify.schedule is empty",
		"verify.max_age_hours must be at least 1",
		`verify.read_subset "150%" must be between`,
		`detect.new_paths "maybe"`,
		"detect.large_path_gb can't be negative",
		`notify.ntfy_url "ntfy.sh/topic"`,
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("missing problem %q in:\n%v", want, err)
		}
	}
	if strings.Contains(err.Error(), "/var/lib/ok") {
		t.Errorf("a valid extra path was reported: %v", err)
	}
}

func TestValidateAcceptsRepositoryURLs(t *testing.T) {
	for _, url := range []string{"/mnt/backup", "sftp:alice@nas:/srv/carryon", "rest:https://host:8000/", "s3:s3.amazonaws.com/bucket", "b2:bucket:carryon", "rclone:remote:Backups/carryon"} {
		c := Default()
		c.Repository.URL = url
		if err := c.Validate(); err != nil {
			t.Errorf("%s: %v", url, err)
		}
	}
}

func TestRetentionMustKeepSomething(t *testing.T) {
	c := Default()
	c.Retention = Retention{PruneEveryDays: 7}
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "retention keeps nothing") {
		t.Fatalf("err = %v", err)
	}
}

func TestParseSpan(t *testing.T) {
	good := map[string]time.Duration{
		"1h": time.Hour, "30min": 30 * time.Minute, "2d": 48 * time.Hour, "1h 30min": 90 * time.Minute,
		"1h30min": 90 * time.Minute, "45": 45 * time.Second, "1w": 7 * 24 * time.Hour, " 10 s ": 10 * time.Second,
	}
	for in, want := range good {
		if got, err := ParseSpan(in); err != nil || got != want {
			t.Errorf("ParseSpan(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	for _, in := range []string{"", "soon", "1.5h", "1y", "h"} {
		if _, err := ParseSpan(in); err == nil {
			t.Errorf("ParseSpan(%q) should fail", in)
		}
	}
}

func write(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestParseMatchesLoad(t *testing.T) {
	data := []byte("machine_name = \"desktop\"\ncolour = 1\n[repository]\nurl = \"/mnt/backup\"\n")
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	loaded, lw, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	parsed, pw, err := Parse(path, data)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(loaded, parsed) || !reflect.DeepEqual(lw, pw) {
		t.Errorf("Parse = %+v %q, Load = %+v %q", parsed, pw, loaded, lw)
	}
	if _, _, err := Parse("snapshot:config.toml", []byte("[repository\n")); err == nil || !strings.Contains(err.Error(), "snapshot:config.toml") {
		t.Errorf("err = %v", err)
	}
}
