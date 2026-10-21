package bundle

import (
	"path"
	"strings"
)

// FindsByName reports whether an added system folder joins package pkg's app bundle by its name
// alone (/var/lib/foo <-> foo), as grouping does for extra folders. When it doesn't, the folder
// needs a put rule in bundles.conf to join that bundle.
func FindsByName(folder, pkg string) bool {
	k := appKey(path.Base(folder))
	return k != "" && k == appKey(pkg)
}

// Name matches, from closest to loosest; see NameMatch.
const (
	NoMatch    = 0
	StemMatch  = 1 // the folder is named after the package's stem (/var/lib/jellyfin <-> jellyfin-server)
	ExactMatch = 2 // the folder is named like the package (/var/lib/foo <-> foo, foo-bin)
)

// NameMatch says how closely a system folder's name matches package pkg: ExactMatch when grouping
// finds it by name alone (FindsByName), StemMatch when it is named after the package's stem, the
// name app bundles are known by.
func NameMatch(folder, pkg string) int {
	switch {
	case FindsByName(folder, pkg):
		return ExactMatch
	case runes(path.Base(folder)) >= 4 && strings.ToLower(path.Base(folder)) == stemOf(pkg):
		return StemMatch
	}
	return NoMatch
}

// IsBuiltin reports whether id is one of the bundles Carryon always has, or the catch-all
// "other", rather than an app's bundle.
func IsBuiltin(id string) bool {
	_, ok := Builtin[id]
	return ok || id == "other"
}
