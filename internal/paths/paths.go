// Package paths holds the standard places Carryon reads and writes.
//
// Every directory can be moved with an environment variable, so tests and development runs never
// touch the real system: CARRYON_CONF_DIR, CARRYON_STATE_DIR, CARRYON_CACHE_DIR, CARRYON_RUN_DIR and
// CARRYON_SHARE_DIR.
package paths

import (
	"os"
	"path/filepath"
)

// Dirs are Carryon's directories.
type Dirs struct {
	Conf  string // settings: /etc/carryon
	State string // record, run history, inbox, flags: /var/lib/carryon
	Cache string // restic cache: /var/cache/carryon
	Run   string // runtime files: /run/carryon
	Share string // files shipped with the package (default excludes, catalog): /usr/share/carryon
}

// Default returns the standard directories, with any CARRYON_* overrides applied.
func Default() Dirs {
	return Dirs{
		Conf:  env("CARRYON_CONF_DIR", "/etc/carryon"),
		State: env("CARRYON_STATE_DIR", "/var/lib/carryon"),
		Cache: env("CARRYON_CACHE_DIR", "/var/cache/carryon"),
		Run:   env("CARRYON_RUN_DIR", "/run/carryon"),
		Share: env("CARRYON_SHARE_DIR", "/usr/share/carryon"),
	}
}

func env(name, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return fallback
}

// ConfigFile is the main settings file.
func (d Dirs) ConfigFile() string { return filepath.Join(d.Conf, "config.toml") }

// ExcludesFile holds the user's exclude patterns (restic syntax).
func (d Dirs) ExcludesFile() string { return filepath.Join(d.Conf, "excludes.txt") }

// BundlesFile holds the user's bundle rules.
func (d Dirs) BundlesFile() string { return filepath.Join(d.Conf, "bundles.conf") }

// PasswordFile is the default location of the repository password (root only).
func (d Dirs) PasswordFile() string { return filepath.Join(d.Conf, "repository-password") }

// RecordDir holds this machine's system record.
func (d Dirs) RecordDir() string { return filepath.Join(d.State, "record") }

// StatusFile is the world-readable summary of recent runs.
func (d Dirs) StatusFile() string { return filepath.Join(d.State, "status.json") }

// RunsFile is the run history.
func (d Dirs) RunsFile() string { return filepath.Join(d.State, "runs.jsonl") }

// InboxFile holds suggestions waiting for the user.
func (d Dirs) InboxFile() string { return filepath.Join(d.State, "inbox.json") }

// RestoringFlag pauses automatic backups while a restore is in progress.
func (d Dirs) RestoringFlag() string { return filepath.Join(d.State, "restoring") }

// PendingFlag marks a change that still needs a backup (written by the pacman hook).
func (d Dirs) PendingFlag() string { return filepath.Join(d.State, "pending") }

// ResticCache is restic's cache directory.
func (d Dirs) ResticCache() string { return filepath.Join(d.Cache, "restic") }

// DefaultExcludes is the package's default exclude list.
func (d Dirs) DefaultExcludes() string { return filepath.Join(d.Share, "excludes.default") }
