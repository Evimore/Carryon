package backup

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"

	"github.com/Evimore/carryon/internal/dest"
)

// restic runs with --one-file-system, so it doesn't wander into network shares, removable drives or
// /proc. On btrfs that also stops it at every subvolume inside a backed-up folder, which the
// prototype silently left out. Carryon finds those subvolumes and names them to restic as folders
// of their own, and reports the other filesystems it leaves out.

// LeftOut is a filesystem inside a backed-up folder that isn't backed up.
type LeftOut struct {
	Path   string // where it is
	FSType string // its filesystem type, when it's a mount
	Source string // what is mounted there
	Why    string // why it's left out
}

func (l LeftOut) String() string {
	what := "a separate filesystem"
	if l.FSType != "" {
		what = "a separate " + l.FSType + " filesystem"
		if l.Source != "" && l.Source != "none" {
			what += " (" + l.Source + ")"
		}
	}
	msg := fmt.Sprintf("%s is %s and isn't backed up", l.Path, what)
	if l.Why != "" {
		msg += ": " + l.Why
	}
	return msg
}

// whyNotCrossed is the note for a mount Carryon doesn't cross.
const whyNotCrossed = "add it to backup.extra_paths to include it"

// snapshotDirs are subvolume names that hold snapshots of other subvolumes (snapper). Backing
// them up would copy every snapshot.
var snapshotDirs = map[string]bool{".snapshots": true}

// isSubvolume reports whether m is a btrfs subvolume of the same filesystem as the mount holding
// its parent folder: part of the same disk, so it belongs in the backup.
func isSubvolume(t dest.MountTable, m dest.Mount) bool {
	if m.FSType != "btrfs" || m.MountPoint == "/" {
		return false
	}
	parent, ok := t.Containing(filepath.Dir(m.MountPoint))
	return ok && parent.FSType == "btrfs" && parent.Source == m.Source
}

// withoutSubvolumes is the mount table without nested btrfs subvolumes of the same filesystem, for
// the exclude list and capture: mounts in home folders are left out, but these are part of the
// home.
func withoutSubvolumes(t dest.MountTable) dest.MountTable {
	if t == nil {
		return nil
	}
	out := dest.MountTable{}
	for _, m := range t {
		if !isSubvolume(t, m) {
			out = append(out, m)
		}
	}
	return out
}

// volumeScan looks for filesystems inside the backed-up folders.
type volumeScan struct {
	sys      System
	mounts   dest.MountTable
	excluded func(path string) bool            // the effective exclude list
	devOf    func(path string) (uint64, error) // the device of a real path; tests replace it
}

// scan returns the nested subvolumes to back up as folders of their own, and what is left out.
// targets are system paths; so are the results.
func (s volumeScan) scan(targets []string) (add []string, left []LeftOut) {
	seenAdd := map[string]bool{}
	seenLeft := map[string]bool{}
	addPath := func(p string) {
		if !seenAdd[p] {
			seenAdd[p] = true
			add = append(add, p)
		}
	}
	leave := func(l LeftOut) {
		if !seenLeft[l.Path] {
			seenLeft[l.Path] = true
			left = append(left, l)
		}
	}
	mountAt := map[string]dest.Mount{}
	for _, m := range s.mounts {
		mountAt[m.MountPoint] = m // a later mount hides an earlier one
	}

	for _, t := range targets {
		base, ok := s.mounts.Containing(t)
		if !ok {
			continue
		}
		// Mounts inside the folder, shallowest first, so a mount inside one left out is skipped.
		inner := s.mounts.Under(t)
		sort.SliceStable(inner, func(i, j int) bool { return len(inner[i].MountPoint) < len(inner[j].MountPoint) })
		var skipped []string
		for _, m := range inner {
			if mountAt[m.MountPoint].ID != m.ID || underAny(m.MountPoint, skipped) {
				continue
			}
			switch {
			case s.excluded(m.MountPoint):
				skipped = append(skipped, m.MountPoint) // left out on purpose: say nothing
			case isSubvolume(s.mounts, m) && snapshotDirs[filepath.Base(m.MountPoint)]:
				skipped = append(skipped, m.MountPoint)
				leave(LeftOut{Path: m.MountPoint, FSType: m.FSType, Why: "it holds snapshots of other subvolumes"})
			case isSubvolume(s.mounts, m):
				addPath(m.MountPoint)
			default:
				skipped = append(skipped, m.MountPoint)
				leave(LeftOut{Path: m.MountPoint, FSType: m.FSType, Source: m.Source, Why: whyNotCrossed})
			}
		}
		// Subvolumes that aren't mounts (made with `btrfs subvolume create`) only show as a change
		// of device number, so the folder is walked. Only btrfs has them.
		if base.FSType == "btrfs" || anyBtrfs(inner) {
			s.walk(t, mountAt, skipped, addPath, leave)
		}
	}
	return add, left
}

// walk finds unmounted subvolumes under the system path t.
func (s volumeScan) walk(t string, mountAt map[string]dest.Mount, skipped []string, addPath func(string), leave func(LeftOut)) {
	root := s.sys.root()
	start := s.sys.real(t)
	devs := map[string]uint64{}
	_ = filepath.WalkDir(start, func(real string, d fs.DirEntry, err error) error {
		if err != nil {
			if d != nil && d.IsDir() && real != start {
				return fs.SkipDir
			}
			return nil
		}
		if !d.IsDir() {
			return nil
		}
		sysPath := systemPath(root, real)
		if real != start && (s.excluded(sysPath) || underAny(sysPath, skipped)) {
			return fs.SkipDir
		}
		dev, err := s.devOf(real)
		if err != nil {
			return fs.SkipDir
		}
		devs[real] = dev
		if real == start {
			return nil
		}
		parentDev, ok := devs[filepath.Dir(real)]
		if !ok || parentDev == dev {
			return nil
		}
		if m, isMount := mountAt[sysPath]; isMount {
			if isSubvolume(s.mounts, m) {
				return nil // handled with the mounts; look for more inside
			}
			return fs.SkipDir
		}
		if snapshotDirs[d.Name()] {
			leave(LeftOut{Path: sysPath, FSType: "btrfs", Why: "it holds snapshots of other subvolumes"})
			return fs.SkipDir
		}
		addPath(sysPath)
		return nil
	})
}

// systemPath turns a real path under root back into the system path.
func systemPath(root, real string) string {
	if root == "/" {
		return real
	}
	rel, err := filepath.Rel(root, real)
	if err != nil || rel == "." {
		return "/"
	}
	return "/" + rel
}

func anyBtrfs(ms []dest.Mount) bool {
	for _, m := range ms {
		if m.FSType == "btrfs" {
			return true
		}
	}
	return false
}

// underAny reports whether p is one of dirs or inside one.
func underAny(p string, dirs []string) bool {
	for _, d := range dirs {
		if p == d || strings.HasPrefix(p, strings.TrimSuffix(d, "/")+"/") {
			return true
		}
	}
	return false
}

// deviceOf returns the device number of the file at path, without following a final symlink.
func deviceOf(path string) (uint64, error) {
	fi, err := os.Lstat(path)
	if err != nil {
		return 0, err
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, errors.New("no device number")
	}
	return uint64(st.Dev), nil //nolint:unconvert // Dev is uint32 on some platforms
}
