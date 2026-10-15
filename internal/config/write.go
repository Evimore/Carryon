package config

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
)

// section is one part of the written file. An empty name is the top of the file.
type section struct {
	name, comment string
	settings      []line
}

// line is one setting with its comment. When the setting is empty and has an example, the
// example is written commented out instead, to show what could go there.
type line struct {
	key, comment, example string
}

// layout is the written file, in the order of design.md, "Configuration". Every setting in Config
// must be here (a test checks this).
var layout = []section{
	{"", "", []line{
		{"config_version", "Format of this file. Carryon updates it; don't change it by hand.", ""},
		{"machine_name", "Name this machine's backups are stored under in the repository.", `"desktop"`},
	}},
	{"repository", "Where backups go.", []line{
		{"url", "Any restic repository: a folder, sftp:, rest:, s3:, b2:, rclone: and others.", `"/mnt/backup/carryon"`},
		{"password_file", "File holding the repository password, readable by root only.", `"/etc/carryon/repository-password"`},
		{"password_command", "Or a command that prints the password, instead of password_file.", `"systemd-creds decrypt /etc/carryon/repository-password.cred"`},
		{"rclone_user", "For rclone: URLs, the user whose rclone login is used.", `"alice"`},
	}},
	{"backup", "What gets backed up, and when.", []line{
		{"schedule", "When the regular backup runs, as a systemd calendar time (\"daily\", \"Mon *-*-* 03:00\").", ""},
		{"after_change_delay", "Back up this long after the last package change (\"30min\", \"1h\"), or \"off\".", ""},
		{"extra_paths", "System folders to back up besides the built-in ones. Add more with \"carryon add\".", ""},
		{"users", "Users whose home folders are backed up; \"*\" means every user who can sign in.", ""},
		{"skip_default_excludes", "Default exclude patterns to switch off, so those paths are backed up after all.", `["/home/*/.local/share/Steam/steamapps/common"]`},
	}},
	{"retention", "How many backups each machine keeps.", []line{
		{"keep_within", "Keep every backup this recent, in restic's form (\"2d\", \"1y6m\"); empty for none.", ""},
		{"keep_daily", "Keep the last backup of this many days.", ""},
		{"keep_weekly", "Keep the last backup of this many weeks.", ""},
		{"keep_monthly", "Keep the last backup of this many months.", ""},
		{"keep_yearly", "Keep the last backup of this many years.", ""},
		{"prune_every_days", "Free the space of forgotten backups every this many days.", ""},
	}},
	{"verify", "How backups are checked.", []line{
		{"schedule", "When the deep check runs, as a systemd calendar time.", ""},
		{"max_age_hours", "Warn when the newest backup is older than this many hours.", ""},
		{"read_subset", "Share of the backup the deep check downloads: \"2%\", \"1/12\" or a size like \"500M\".", ""},
	}},
	{"detect", "What happens when new data appears outside the backed-up folders.", []line{
		{"new_paths", "\"ask\" to suggest it, \"auto\" to back it up, \"off\" to ignore it.", ""},
		{"large_path_gb", "Suggestions bigger than this many GB are marked as large.", ""},
	}},
	{"notify", "How problems reach you.", []line{
		{"desktop", "Show desktop notifications.", ""},
		{"ntfy_url", "Also send alerts to this ntfy topic.", `"https://ntfy.sh/my-topic"`},
		{"webhook_url", "Also send alerts to this web address.", `"https://example.com/carryon-hook"`},
	}},
	{"pacman", "Settings for pacman, Arch's package manager.", []line{
		{"extra_standard_repos", "Package repositories to treat as the distribution's own.", `["myrepo"]`},
	}},
}

// Format returns the config as the commented file Carryon writes. Loading the result gives back
// an equal Config.
func Format(cfg Config) ([]byte, error) {
	if cfg.ConfigVersion == 0 {
		cfg.ConfigVersion = CurrentVersion
	}
	root := reflect.ValueOf(cfg)
	var b bytes.Buffer
	b.WriteString("# Carryon settings. Check them with \"carryon config check\"; change them with\n")
	b.WriteString("# \"carryon config set\" or by editing this file. Carryon rewrites this file when it\n")
	b.WriteString("# changes a setting, so comments you add here are not kept.\n")
	for _, s := range layout {
		if s.name != "" {
			fmt.Fprintf(&b, "\n# %s\n[%s]\n", s.comment, s.name)
		}
		for _, l := range s.settings {
			key := l.key
			if s.name != "" {
				key = s.name + "." + l.key
			}
			v, err := setting(root, key)
			if err != nil {
				return nil, fmt.Errorf("writing the config: %w", err)
			}
			if s.name == "" {
				b.WriteByte('\n')
			}
			fmt.Fprintf(&b, "# %s\n", l.comment)
			if v.IsZero() || (v.Kind() == reflect.Slice && v.Len() == 0) {
				if l.example != "" {
					fmt.Fprintf(&b, "# %s = %s\n", l.key, l.example)
					continue
				}
			}
			fmt.Fprintf(&b, "%s = %s\n", l.key, literal(v))
		}
	}
	return b.Bytes(), nil
}

// Write saves the config as a commented file. It replaces the file in one step, so a crash
// never leaves half a file, and keeps an existing file's permissions (new files get 0644).
func Write(path string, cfg Config) error {
	data, err := Format(cfg)
	if err != nil {
		return err
	}
	return WriteFileAtomic(path, data, 0o644)
}

// WriteFileAtomic writes data to a temporary file next to path and renames it over path. An
// existing file keeps its permissions; a new one gets perm. If path is a symlink, the file it
// points to is replaced and the link stays.
func WriteFileAtomic(path string, data []byte, perm fs.FileMode) error {
	if real, err := filepath.EvalSymlinks(path); err == nil {
		path = real
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	if st, err := os.Stat(path); err == nil {
		perm = st.Mode().Perm()
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	dir, base := filepath.Split(path)
	if dir == "" {
		dir = "."
	}
	tmp, err := os.CreateTemp(dir, "."+base+".*.tmp")
	if err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	done := false
	defer func() {
		if !done {
			tmp.Close()
			os.Remove(tmp.Name())
		}
	}()
	if _, err := tmp.Write(data); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	if err := tmp.Chmod(perm); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	if err := tmp.Sync(); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	done = true
	return nil
}
