package dest

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Identity is what makes a local destination itself. init records it, and every run checks it before
// writing anything.
type Identity struct {
	FSUUID     string `json:"fs_uuid,omitempty"` // UUID of the filesystem holding the repository ("" if it has none)
	MountPoint string `json:"mount_point"`       // where that filesystem was mounted
	RepoID     string `json:"repo_id"`           // the restic repository's ID
}

// Repository reads a repository's ID. The restic engine implements it with `restic cat config`.
type Repository interface {
	RepositoryID(ctx context.Context) (string, error)
}

// ByUUIDDir is where udev links filesystem UUIDs to their devices, relative to the root.
const ByUUIDDir = "/dev/disk/by-uuid"

// ErrNoUUID means the mount's filesystem has no UUID Carryon can find (tmpfs, overlay, a network
// filesystem, or a device udev didn't link).
var ErrNoUUID = errors.New("no filesystem UUID found")

// FSUUID finds the UUID of the filesystem mounted as m, by matching its source device against the
// links in /dev/disk/by-uuid under root. It runs no programs. Links are followed inside root, so
// /dev/mapper names and /dev/disk/by-uuid both resolve to the same /dev/dm-N.
func FSUUID(root string, m Mount) (string, error) {
	if !strings.HasPrefix(m.Source, "/dev/") {
		return "", ErrNoUUID
	}
	want, err := ResolvePath(root, m.Source)
	if err != nil {
		return "", fmt.Errorf("finding the device %s: %w", m.Source, err)
	}
	dir := filepath.Join(root, ByUUIDDir)
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return "", ErrNoUUID
	}
	if err != nil {
		return "", fmt.Errorf("reading %s: %w", dir, err)
	}
	for _, e := range entries {
		link := filepath.Join(ByUUIDDir, e.Name())
		dev, err := ResolvePath(root, link)
		if err != nil {
			continue
		}
		if dev == want || sameDevice(filepath.Join(root, dev), m) {
			return unescapeUdev(e.Name()), nil
		}
	}
	return "", ErrNoUUID
}

// unescapeUdev undoes udev's \xNN escapes in link names (a label or UUID with a space or slash).
func unescapeUdev(s string) string {
	if !strings.Contains(s, `\x`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) && s[i+1] == 'x' && isHex(s[i+2]) && isHex(s[i+3]) {
			b.WriteByte(hexVal(s[i+2])<<4 | hexVal(s[i+3]))
			i += 3
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

func isHex(c byte) bool {
	return c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F'
}

func hexVal(c byte) byte {
	switch {
	case c >= 'a':
		return c - 'a' + 10
	case c >= 'A':
		return c - 'A' + 10
	default:
		return c - '0'
	}
}

// maxLinks is how many symlinks ResolvePath follows before giving up, like the kernel's limit.
const maxLinks = 40

// ResolvePath resolves the symlinks in the absolute path as the system under root would see it:
// absolute link targets are taken relative to root, and ".." never climbs above root. It returns the
// resolved path without the root. Parts of the path that don't exist, or can't be read, are kept as
// written: a missing destination folder still resolves, so Carryon can find the mount it would land on.
func ResolvePath(root, path string) (string, error) {
	if !filepath.IsAbs(path) {
		return "", fmt.Errorf("%q isn't an absolute path", path)
	}
	done := "/"
	todo := strings.Split(strings.TrimPrefix(filepath.Clean(path), "/"), "/")
	links := 0
	for len(todo) > 0 {
		part := todo[0]
		todo = todo[1:]
		switch part {
		case "", ".":
			continue
		case "..":
			done = filepath.Dir(done)
			continue
		}
		next := filepath.Join(done, part)
		fi, err := os.Lstat(filepath.Join(root, next))
		if err != nil || fi.Mode()&os.ModeSymlink == 0 {
			// Missing or unreadable (a user's FUSE mount, as root): keep the rest as written rather
			// than look any deeper.
			done = next
			if err != nil {
				return filepath.Join(append([]string{done}, todo...)...), nil
			}
			continue
		}
		links++
		if links > maxLinks {
			return "", fmt.Errorf("too many symlinks in %s", path)
		}
		target, err := os.Readlink(filepath.Join(root, next))
		if err != nil {
			return "", fmt.Errorf("reading the link %s: %w", next, err)
		}
		if filepath.IsAbs(target) {
			done = "/"
		}
		todo = append(strings.Split(target, "/"), todo...)
	}
	return done, nil
}

// Record works out the identity of the local destination at path, for init to store. The filesystem
// UUID is left empty when the filesystem has none; the mount point and repository ID are required.
func Record(ctx context.Context, root, path string, repo Repository) (Identity, error) {
	mounts, err := ReadMounts(root)
	if err != nil {
		return Identity{}, err
	}
	resolved, err := ResolvePath(root, path)
	if err != nil {
		return Identity{}, err
	}
	m, ok := mounts.Containing(resolved)
	if !ok {
		return Identity{}, fmt.Errorf("no filesystem is mounted at %s or above it", path)
	}
	uuid, err := FSUUID(root, m)
	if err != nil && !errors.Is(err, ErrNoUUID) {
		return Identity{}, err
	}
	id, err := repo.RepositoryID(ctx)
	if err != nil {
		return Identity{}, fmt.Errorf("reading the repository ID: %w", err)
	}
	if id == "" {
		return Identity{}, errors.New("the repository has an empty ID")
	}
	return Identity{FSUUID: uuid, MountPoint: m.MountPoint, RepoID: id}, nil
}
