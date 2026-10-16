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

// targets lists the system paths a backup covers, in a stable order, without any that lie inside
// another: SystemPaths, the record's backed-up paths, backup.extra_paths and the users' homes.
// Paths that don't exist under the system root are left out.
func targets(sys System, cfg config.Config, items []record.Item, users []capture.User) []string {
	var cand []string
	cand = append(cand, SystemPaths...)
	for _, it := range items {
		if it.Type != record.TypePath || !filepath.IsAbs(it.ID) {
			continue
		}
		if strings.ContainsAny(it.ID, "*?[") {
			matches, _ := filepath.Glob(sys.real(it.ID))
			for _, m := range matches {
				cand = append(cand, systemPath(sys.root(), m))
			}
			continue
		}
		cand = append(cand, it.ID)
	}
	cand = append(cand, cfg.Backup.ExtraPaths...)
	for _, u := range users {
		cand = append(cand, u.Home)
	}

	var keep []string
	seen := map[string]bool{}
	for _, p := range cand {
		if !filepath.IsAbs(p) {
			continue
		}
		p = filepath.Clean(p)
		if seen[p] || p == "/" {
			continue
		}
		seen[p] = true
		if _, err := os.Lstat(sys.real(p)); err != nil {
			continue
		}
		keep = append(keep, p)
	}
	return outermost(keep)
}

// outermost drops paths inside another path of the list, and sorts the rest.
func outermost(ps []string) []string {
	sorted := append([]string(nil), ps...)
	sort.Strings(sorted)
	var out []string
	for _, p := range sorted {
		if !underAny(p, out) {
			out = append(out, p)
		}
	}
	return out
}
