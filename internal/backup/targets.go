package backup

import (
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/Evimore/carryon/internal/capture"
	"github.com/Evimore/carryon/internal/config"
	"github.com/Evimore/carryon/internal/record"
)

// SystemPaths are always in the backup when they exist: the system settings (with Carryon's own
// in /etc/carryon, so they travel to a new machine), root's home and /srv.
var SystemPaths = []string{"/etc", "/root", "/srv"}

// Target is a folder a backup covers, and why.
type Target struct {
	Path string `json:"path"`           // a system path
	Kind string `json:"from"`           // one of the Target* constants
	User string `json:"user,omitempty"` // TargetHome: whose home it is
}

// Why a folder is in the backup.
const (
	TargetSystem = "system"      // one of SystemPaths
	TargetRecord = "record"      // a path item of the record
	TargetExtra  = "extra_paths" // backup.extra_paths
	TargetHome   = "home"        // an included user's home
)

// targets lists the system paths a backup covers; see Targets.
func targets(sys System, cfg config.Config, items []record.Item, users []capture.User) []string {
	var out []string
	for _, t := range Targets(sys, cfg, items, users) {
		out = append(out, t.Path)
	}
	return out
}

// Targets lists the folders a backup covers, sorted, without any that lie inside another:
// SystemPaths, the record's backed-up paths, backup.extra_paths and the users' homes. A folder
// listed for several reasons keeps the first. Paths that don't exist under the system root are
// left out.
func Targets(sys System, cfg config.Config, items []record.Item, users []capture.User) []Target {
	var cand []Target
	for _, p := range SystemPaths {
		cand = append(cand, Target{Path: p, Kind: TargetSystem})
	}
	for _, it := range items {
		if it.Type != record.TypePath || !filepath.IsAbs(it.ID) {
			continue
		}
		if strings.ContainsAny(it.ID, "*?[") {
			matches, _ := filepath.Glob(sys.real(it.ID))
			for _, m := range matches {
				cand = append(cand, Target{Path: systemPath(sys.root(), m), Kind: TargetRecord})
			}
			continue
		}
		cand = append(cand, Target{Path: it.ID, Kind: TargetRecord})
	}
	for _, p := range cfg.Backup.ExtraPaths {
		cand = append(cand, Target{Path: p, Kind: TargetExtra})
	}
	for _, u := range users {
		cand = append(cand, Target{Path: u.Home, Kind: TargetHome, User: u.Name})
	}

	var keep []Target
	seen := map[string]bool{}
	for _, t := range cand {
		if !filepath.IsAbs(t.Path) {
			continue
		}
		t.Path = filepath.Clean(t.Path)
		if seen[t.Path] || t.Path == "/" {
			continue
		}
		seen[t.Path] = true
		if _, err := os.Lstat(sys.real(t.Path)); err != nil {
			continue
		}
		keep = append(keep, t)
	}
	return outermost(keep)
}

// outermost drops targets inside another target of the list, and sorts the rest.
func outermost(ts []Target) []Target {
	sorted := append([]Target(nil), ts...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].Path < sorted[j].Path })
	var out []Target
	var dirs []string
	for _, t := range sorted {
		if !underAny(t.Path, dirs) {
			out = append(out, t)
			dirs = append(dirs, t.Path)
		}
	}
	return out
}
