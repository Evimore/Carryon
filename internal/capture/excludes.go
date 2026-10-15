package capture

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/Evimore/carryon/internal/catalog"
	"github.com/Evimore/carryon/internal/config"
	"github.com/Evimore/carryon/internal/dest"
	"github.com/Evimore/carryon/internal/paths"
)

// ExcludeSources are where the effective exclude list comes from.
type ExcludeSources struct {
	Dirs    paths.Dirs       // default excludes and excludes.txt
	Config  config.Config    // backup.skip_default_excludes, repository.url
	Catalog *catalog.Catalog // may be nil
	Users   []User           // whose own settings add excludes in their home
	Mounts  dest.MountTable  // mounts inside the users' homes are left out
}

// BuildExcludes builds the effective exclude list on the real system, writes it to dst for restic
// and returns it for Carryon's own matching: the defaults and the catalog's patterns minus the
// switched-off ones, mount points inside home folders and a local repository's own folder, then
// excludes.txt and each user's own excludes. The warnings say what was ignored along the way.
func BuildExcludes(dst string, in ExcludeSources) ([]string, []string, error) {
	var warnings []string
	defaults, err := config.ReadExcludes(in.Dirs.DefaultExcludes())
	if err != nil {
		return nil, nil, err
	}
	mine, err := config.ReadExcludes(in.Dirs.ExcludesFile())
	if err != nil {
		return nil, nil, err
	}
	var homes []string
	for _, u := range in.Users {
		homes = append(homes, u.Home)
		ucfg, w, err := config.LoadUser(u.Home, u.UID)
		warnings = append(warnings, w...)
		if err != nil {
			warnings = append(warnings, fmt.Sprintf("%s's own settings were ignored: %v", u.Name, err))
			continue
		}
		mine = append(mine, ucfg.Excludes...)
	}
	var auto []string
	for _, m := range in.Mounts.Under(homes...) {
		auto = append(auto, m.MountPoint)
	}
	if url := in.Config.Repository.URL; strings.HasPrefix(url, "/") {
		auto = append(auto, filepath.Clean(url))
	}
	var cat []string
	if in.Catalog != nil {
		cat = in.Catalog.Excludes()
	}
	pats, err := catalog.EffectiveExcludes(dst, catalog.ExcludeInputs{
		Defaults:  defaults,
		Skip:      in.Config.Backup.SkipDefaultExcludes,
		Catalog:   cat,
		Automatic: auto,
		User:      mine,
	})
	return pats, warnings, err
}
