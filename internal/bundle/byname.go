package bundle

import "path"

// FindsByName reports whether an added system folder joins package pkg's app bundle by its name
// alone (/var/lib/foo <-> foo), as grouping does for extra folders. When it doesn't, the folder
// needs a put rule in bundles.conf to join that bundle.
func FindsByName(folder, pkg string) bool {
	k := appKey(path.Base(folder))
	return k != "" && k == appKey(pkg)
}
