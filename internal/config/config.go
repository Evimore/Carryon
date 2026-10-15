// Package config reads and checks /etc/carryon/config.toml. See docs/design.md, "Configuration".
//
// Every setting has a default, so a config file only needs what differs. Defaults follow the
// prototype's (legacy/sysbackup.conf).
package config

import (
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/BurntSushi/toml"
)

// CurrentVersion is the config format this Carryon writes. Older files are migrated.
const CurrentVersion = 1

// Config is Carryon's configuration.
type Config struct {
	ConfigVersion int        `toml:"config_version"`
	MachineName   string     `toml:"machine_name"` // name this machine's snapshots are stored under
	Repository    Repository `toml:"repository"`
	Backup        Backup     `toml:"backup"`
	Retention     Retention  `toml:"retention"`
	Verify        Verify     `toml:"verify"`
	Detect        Detect     `toml:"detect"`
	Notify        Notify     `toml:"notify"`
	Pacman        Pacman     `toml:"pacman"`
}

// Repository is where backups go.
type Repository struct {
	URL             string `toml:"url"`              // any restic repository: /path, sftp:, rest:, s3:, b2:, rclone:...
	PasswordFile    string `toml:"password_file"`    // root-only file holding the password
	PasswordCommand string `toml:"password_command"` // or a command printing it (e.g. systemd-creds)
	RcloneUser      string `toml:"rclone_user"`      // for rclone: URLs, whose rclone login to use
}

// Backup is what gets backed up and when.
type Backup struct {
	Schedule         string   `toml:"schedule"`           // systemd OnCalendar= value
	AfterChangeDelay string   `toml:"after_change_delay"` // back up this long after the last package change; "off" disables
	ExtraPaths       []string `toml:"extra_paths"`        // system folders backed up besides the built-in ones
	Users            []string `toml:"users"`              // users whose homes are included; "*" means every human user
}

// Retention is how many snapshots each machine keeps.
type Retention struct {
	KeepWithin     string `toml:"keep_within"` // restic duration, e.g. "2d"
	KeepDaily      int    `toml:"keep_daily"`
	KeepWeekly     int    `toml:"keep_weekly"`
	KeepMonthly    int    `toml:"keep_monthly"`
	KeepYearly     int    `toml:"keep_yearly"`
	PruneEveryDays int    `toml:"prune_every_days"`
}

// Verify is how backups are checked.
type Verify struct {
	Schedule    string `toml:"schedule"`      // systemd OnCalendar= value for the deep check
	MaxAgeHours int    `toml:"max_age_hours"` // the newest backup may be this old before it's a problem
	ReadSubset  string `toml:"read_subset"`   // share of the data the deep check downloads: "2%", "1/12" or a size
}

// Detect is how new data outside the backed-up folders is handled.
type Detect struct {
	NewPaths    string  `toml:"new_paths"`     // ask, auto or off
	LargePathGB float64 `toml:"large_path_gb"` // suggestions bigger than this are marked as large
}

// Notify is how problems reach the user.
type Notify struct {
	Desktop    bool   `toml:"desktop"`
	NtfyURL    string `toml:"ntfy_url"`
	WebhookURL string `toml:"webhook_url"`
}

// Pacman holds Arch-specific settings.
type Pacman struct {
	ExtraStandardRepos []string `toml:"extra_standard_repos"` // repositories to treat as the distribution's own
}

// Default returns the configuration used for settings a file doesn't mention.
func Default() Config {
	return Config{
		ConfigVersion: CurrentVersion,
		Backup: Backup{
			Schedule:         "daily",
			AfterChangeDelay: "1h",
			ExtraPaths:       []string{"/root", "/srv", "/usr/local"},
			Users:            []string{"*"},
		},
		Retention: Retention{KeepWithin: "2d", KeepDaily: 7, KeepWeekly: 4, KeepMonthly: 6, PruneEveryDays: 7},
		Verify:    Verify{Schedule: "weekly", MaxAgeHours: 48, ReadSubset: "2%"},
		Detect:    Detect{NewPaths: "ask", LargePathGB: 5},
		Notify:    Notify{Desktop: true},
	}
}

// Load reads a config file over the defaults. Settings Carryon doesn't know are returned as
// warnings rather than errors, so a newer file still loads. The result isn't validated; call
// Validate.
func Load(path string) (Config, []string, error) {
	cfg := Default()
	cfg.ConfigVersion = 0 // so a file without config_version can be told apart
	md, err := toml.DecodeFile(path, &cfg)
	if err != nil {
		return Config{}, nil, fmt.Errorf("%s: %w", path, err)
	}
	if cfg.ConfigVersion == 0 {
		cfg.ConfigVersion = CurrentVersion
	}
	var warnings []string
	for _, k := range md.Undecoded() {
		warnings = append(warnings, fmt.Sprintf("%s: unknown setting %q (ignored)", path, k.String()))
	}
	return cfg, warnings, nil
}

var (
	machineNameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)
	resticDurRE   = regexp.MustCompile(`^(\d+[ymdh])+$`)
	subsetRE      = regexp.MustCompile(`^(\d+(\.\d+)?%|\d+/\d+|\d+[KMGT]?)$`)
	repoSchemes   = []string{"sftp:", "rest:", "s3:", "b2:", "azure:", "gs:", "swift:", "rclone:"}
	virtualDirs   = []string{"/proc", "/sys", "/dev", "/run", "/tmp"}
)

// Validate checks every setting and returns all problems found, joined.
func (c Config) Validate() error {
	var errs []error
	bad := func(format string, a ...any) { errs = append(errs, fmt.Errorf(format, a...)) }

	if c.ConfigVersion > CurrentVersion {
		bad("config_version %d was written by a newer Carryon (this one understands up to %d)", c.ConfigVersion, CurrentVersion)
	}
	if c.MachineName != "" && !machineNameRE.MatchString(c.MachineName) {
		bad("machine_name %q: use letters, digits, '.', '_' and '-'", c.MachineName)
	}

	r := c.Repository
	if r.URL != "" && !strings.HasPrefix(r.URL, "/") && !hasAnyPrefix(r.URL, repoSchemes) {
		bad("repository.url %q: use an absolute path or a restic URL (%s)", r.URL, strings.Join(repoSchemes, " "))
	}
	if r.PasswordFile != "" && r.PasswordCommand != "" {
		bad("repository: set password_file or password_command, not both")
	}
	if r.PasswordFile != "" && !filepath.IsAbs(r.PasswordFile) {
		bad("repository.password_file %q must be an absolute path", r.PasswordFile)
	}

	b := c.Backup
	if strings.TrimSpace(b.Schedule) == "" {
		bad("backup.schedule is empty")
	}
	if b.AfterChangeDelay != "off" {
		if _, err := ParseSpan(b.AfterChangeDelay); err != nil {
			bad("backup.after_change_delay: %v (or \"off\")", err)
		}
	}
	for _, p := range b.ExtraPaths {
		switch {
		case !filepath.IsAbs(p):
			bad("backup.extra_paths: %q isn't an absolute path", p)
		case filepath.Clean(p) == "/":
			bad("backup.extra_paths: \"/\" would back up the whole system, including package files")
		case underAny(filepath.Clean(p), virtualDirs):
			bad("backup.extra_paths: %q is in a folder that only exists while the system runs", p)
		}
	}
	if len(b.Users) == 0 {
		bad("backup.users is empty: use [\"*\"] for every user")
	}
	for _, u := range b.Users {
		if strings.TrimSpace(u) == "" {
			bad("backup.users has an empty name")
		}
	}

	rt := c.Retention
	if rt.KeepWithin != "" && !resticDurRE.MatchString(rt.KeepWithin) {
		bad("retention.keep_within %q: use restic's form, like \"2d\" or \"1y6m\"", rt.KeepWithin)
	}
	for _, k := range []struct {
		name string
		n    int
	}{{"keep_daily", rt.KeepDaily}, {"keep_weekly", rt.KeepWeekly}, {"keep_monthly", rt.KeepMonthly}, {"keep_yearly", rt.KeepYearly}} {
		if k.n < 0 {
			bad("retention.%s can't be negative", k.name)
		}
	}
	if rt.KeepWithin == "" && rt.KeepDaily <= 0 && rt.KeepWeekly <= 0 && rt.KeepMonthly <= 0 && rt.KeepYearly <= 0 {
		bad("retention keeps nothing: set keep_within or at least one keep_* value")
	}
	if rt.PruneEveryDays < 1 {
		bad("retention.prune_every_days must be at least 1")
	}

	v := c.Verify
	if strings.TrimSpace(v.Schedule) == "" {
		bad("verify.schedule is empty")
	}
	if v.MaxAgeHours < 1 {
		bad("verify.max_age_hours must be at least 1")
	}
	if !subsetRE.MatchString(v.ReadSubset) {
		bad("verify.read_subset %q: use a percentage (\"2%%\"), a fraction (\"1/12\") or a size (\"500M\")", v.ReadSubset)
	} else if pct, ok := strings.CutSuffix(v.ReadSubset, "%"); ok {
		if f, _ := strconv.ParseFloat(pct, 64); f <= 0 || f > 100 {
			bad("verify.read_subset %q must be between 0%% and 100%%", v.ReadSubset)
		}
	}

	switch c.Detect.NewPaths {
	case "ask", "auto", "off":
	default:
		bad("detect.new_paths %q: use \"ask\", \"auto\" or \"off\"", c.Detect.NewPaths)
	}
	if c.Detect.LargePathGB < 0 {
		bad("detect.large_path_gb can't be negative")
	}

	for _, u := range []struct{ name, url string }{{"notify.ntfy_url", c.Notify.NtfyURL}, {"notify.webhook_url", c.Notify.WebhookURL}} {
		if u.url != "" && !strings.HasPrefix(u.url, "https://") && !strings.HasPrefix(u.url, "http://") {
			bad("%s %q must start with https://", u.name, u.url)
		}
	}
	return errors.Join(errs...)
}

func hasAnyPrefix(s string, prefixes []string) bool {
	for _, p := range prefixes {
		if strings.HasPrefix(s, p) {
			return true
		}
	}
	return false
}

func underAny(p string, dirs []string) bool {
	for _, d := range dirs {
		if p == d || strings.HasPrefix(p, d+"/") {
			return true
		}
	}
	return false
}
