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
	inputs, warnings, err := ExcludeInputs(in)
	if err != nil {
		return nil, nil, err
	}
	pats, err := catalog.EffectiveExcludes(dst, inputs)
	return pats, warnings, err
}

// ExcludeInputs reads the parts of the effective exclude list, saying where each pattern comes
// from. BuildExcludes puts them together; carryon why explains them.
func ExcludeInputs(in ExcludeSources) (catalog.ExcludeInputs, []string, error) {
	var warnings []string
	defaults, err := config.ReadExcludes(in.Dirs.DefaultExcludes())
	if err != nil {
		return catalog.ExcludeInputs{}, nil, err
	}
	mine, err := config.ReadExcludes(in.Dirs.ExcludesFile())
	if err != nil {
		return catalog.ExcludeInputs{}, nil, err
	}
	from := map[string]string{}
	for _, p := range mine {
		from[p] = "your excludes in " + in.Dirs.ExcludesFile()
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
		for _, p := range ucfg.Excludes {
			if _, ok := from[p]; !ok {
				from[p] = u.Name + "'s own settings in ~/.config/carryon/config.toml"
			}
		}
		mine = append(mine, ucfg.Excludes...)
	}
	var auto []string
	for _, m := range in.Mounts.Under(homes...) {
		auto = append(auto, m.MountPoint)
		what := "a mount point"
		if m.FSType != "" {
			what = "a " + m.FSType + " mount"
		}
		if m.Source != "" && m.Source != "none" {
			what += " of " + m.Source
		}
		from[m.MountPoint] = what + " inside a home folder"
	}
	if url := in.Config.Repository.URL; strings.HasPrefix(url, "/") {
		auto = append(auto, filepath.Clean(url))
		from[filepath.Clean(url)] = "the backup's own folder (repository.url)"
	}
	var cat []string
	if in.Catalog != nil {
		cat = in.Catalog.Excludes()
		for p, app := range in.Catalog.ExcludeOwners() {
			from[p] = app
		}
	}
	return catalog.ExcludeInputs{
		Defaults:  defaults,
		Skip:      in.Config.Backup.SkipDefaultExcludes,
		Catalog:   cat,
		Automatic: auto,
		User:      mine,
		From:      from,
	}, warnings, nil
}
