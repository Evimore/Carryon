// Package record is Carryon's description of a machine: its items, its bundles and the facts about
// it. See docs/design.md, "The system record".
//
// The JSON field names are the prototype's, so records it wrote (format 1) can still be read.
package record

import (
	"fmt"
	"strings"
)

// Item types.
const (
	TypePkg     = "pkg"     // package from a repository
	TypeAUR     = "aur"     // foreign package (AUR or built by hand)
	TypeRepo    = "repo"    // extra package repository
	TypeFile    = "file"    // edited or added system file, copied into the record
	TypeSvc     = "svc"     // enabled system unit
	TypeMask    = "mask"    // masked system unit
	TypeUsvc    = "usvc"    // enabled user unit
	TypeUunit   = "uunit"   // user unit file or drop-in
	TypeUmask   = "umask"   // masked user unit
	TypeLinger  = "linger"  // user lingering
	TypeMkdir   = "mkdir"   // folder a unit needs
	TypeKparam  = "kparam"  // kernel parameter
	TypeGroup   = "group"   // supplementary group membership
	TypeShell   = "shell"   // login shell
	TypeCargo   = "cargo"   // cargo install
	TypePipx    = "pipx"    // pipx install
	TypeNpm     = "npm"     // global npm package
	TypeFlatpak = "flatpak" // Flatpak app
	TypePath    = "path"    // folder or file restored from the repository
)

// File kinds.
const (
	KindModified = "modified" // a package's backup= config file that differs from the package
	KindEdited   = "edited"   // another package file that differs from the package
	KindUnowned  = "unowned"  // a file no package owns
)

// Item is one thing in the record. Which fields are set depends on Type.
//
// Fields are declared in alphabetical order of their JSON names, so items are written with sorted
// keys, like the prototype wrote them.
type Item struct {
	AsDeps   bool     `json:"asdeps,omitempty"`    // pkg, aur: installed as a dependency
	Bundle   string   `json:"bundle,omitempty"`    // the bundle the item belongs to
	Conf     string   `json:"conf,omitempty"`      // repo: file holding its section ("" = pacman.conf)
	Extra    bool     `json:"extra,omitempty"`     // path: added through the config, not built in
	ForUnit  string   `json:"for_unit,omitempty"`  // pkg, aur, mkdir: the unit that needs it
	GID      *int     `json:"gid,omitempty"`       // path: numeric group at capture time
	Home     string   `json:"home,omitempty"`      // path: location relative to the user's home
	ID       string   `json:"id"`                  // what the item is: package name, path, unit...
	KeyNames []string `json:"key_names,omitempty"` // repo: names on the signing keys
	Keys     []string `json:"keys,omitempty"`      // repo: signing key fingerprints
	Kind     string   `json:"kind,omitempty"`      // file: modified, edited or unowned
	Link     string   `json:"link,omitempty"`      // file: symlink target
	Mode     string   `json:"mode,omitempty"`      // file: permission bits, octal
	Origin   string   `json:"origin,omitempty"`    // flatpak: remote
	Owner    string   `json:"owner,omitempty"`     // file, path: "user:group"
	Pkg      string   `json:"pkg,omitempty"`       // file, svc: owning package
	Rel      string   `json:"rel,omitempty"`       // mkdir: location relative to the user's home
	Repo     string   `json:"repo,omitempty"`      // pkg: repository it comes from
	Scope    string   `json:"scope,omitempty"`     // flatpak: system or user
	Section  string   `json:"section,omitempty"`   // repo: its pacman.conf section
	SHA      string   `json:"sha,omitempty"`       // file, uunit: sha256 of the content
	Source   string   `json:"source,omitempty"`    // kparam: config file it was read from
	TooBig   bool     `json:"too_big,omitempty"`   // file: too big to copy into the record
	Type     string   `json:"type"`                // one of the Type* constants
	UID      *int     `json:"uid,omitempty"`       // path: numeric owner at capture time
	UnitFile string   `json:"unit_file,omitempty"` // svc: unit file path
	User     string   `json:"user,omitempty"`      // per-user items (format 2)
}

// Key identifies an item within a record.
func (it Item) Key() string { return it.Type + ":" + it.ID }

// HomePath is how the item is shown to users: "~/..." for home items, the ID otherwise.
func (it Item) HomePath() string {
	if it.Home != "" {
		return "~/" + it.Home
	}
	return it.ID
}

// sameContent reports whether two versions of an item have the same content, for diffs.
func sameContent(a, b Item) bool {
	return a.SHA == b.SHA && a.Link == b.Link && a.Mode == b.Mode && a.Owner == b.Owner && a.Repo == b.Repo
}

// Describe is a one-line, column-aligned description of the item.
func (it Item) Describe() string {
	i := it.ID
	switch it.Type {
	case TypePkg:
		s := "package    " + i
		if it.Repo != "" && it.Repo != "?" {
			s += "  [" + it.Repo + "]"
		}
		if it.AsDeps {
			s += "  (as dependency)"
		}
		return s
	case TypeAUR:
		s := "AUR        " + i
		if it.AsDeps {
			s += "  (as dependency)"
		}
		return s
	case TypeRepo:
		s := "repo       [" + i + "]"
		if len(it.Keys) > 0 {
			s += " + signing key (" + Brief(it.KeyNames, 2) + ")"
		}
		return s
	case TypeFile:
		switch it.Kind {
		case KindModified:
			return "file       " + i + "  (edited package file)"
		case KindEdited:
			return "file       " + i + "  (changed package file)"
		}
		return "file       " + i
	case TypeSvc:
		return "enable     " + i
	case TypeMask:
		return "mask       " + i
	case TypeUsvc:
		return "user unit  enable " + i
	case TypeUunit:
		return "user unit  " + i
	case TypeUmask:
		return "user unit  mask " + i
	case TypeKparam:
		return "kernel     " + i
	case TypeGroup:
		return "group      " + i
	case TypeShell:
		return "shell      " + i
	case TypeLinger:
		return "linger     " + i
	case TypeCargo:
		return "cargo      " + i
	case TypePipx:
		return "pipx       " + i
	case TypeNpm:
		return "npm -g     " + i
	case TypeFlatpak:
		return "flatpak    " + i
	case TypePath:
		return "backup     " + it.HomePath()
	case TypeMkdir:
		if it.Rel != "" {
			return "folder     ~/" + it.Rel
		}
		return "folder     " + i
	}
	return fmt.Sprintf("%s %s", it.Type, i)
}

// Brief joins up to n names and says how many more there are.
func Brief(names []string, n int) string {
	if len(names) <= n {
		return strings.Join(names, ", ")
	}
	return strings.Join(names[:n], ", ") + fmt.Sprintf(" ... (+%d more)", len(names)-n)
}
