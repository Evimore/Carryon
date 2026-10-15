package dest

import (
	"path/filepath"
	"strings"
)

// Kind says what sort of filesystem a mount is, as far as guarding a destination goes.
type Kind int

const (
	// Local is a filesystem on this machine: a disk, a partition, tmpfs.
	Local Kind = iota
	// FUSE is a filesystem run by a program (rclone mount, sshfs, GVFS...). Root usually can't read a
	// user's FUSE mount, and some report a write as done before it reaches the remote.
	FUSE
	// Network is a kernel network filesystem (NFS, SMB/CIFS...).
	Network
)

func (k Kind) String() string {
	switch k {
	case FUSE:
		return "FUSE"
	case Network:
		return "network"
	default:
		return "local"
	}
}

// networkTypes are the kernel's network filesystems.
var networkTypes = map[string]bool{
	"nfs": true, "nfs4": true, "cifs": true, "smb3": true, "smbfs": true, "ncpfs": true,
	"afs": true, "ceph": true, "glusterfs": true, "9p": true, "lustre": true, "coda": true,
}

// Kind classifies the mount by its filesystem type. "fuseblk" (ntfs-3g, exfat-fuse) counts as local:
// it is a FUSE driver for a disk in this machine, with a device and a filesystem UUID like any other.
func (m Mount) Kind() Kind {
	switch {
	case m.FSType == "fuse" || strings.HasPrefix(m.FSType, "fuse."):
		return FUSE
	case networkTypes[m.FSType] || strings.HasPrefix(m.FSType, "nfs"):
		return Network
	default:
		return Local
	}
}

// Remote reports whether the mount is a FUSE or network mount, the kind Carryon never walks and
// never backs up to directly.
func (m Mount) Remote() bool { return m.Kind() != Local }

// OnRemoteMount reports whether path sits on a FUSE or network mount, and which one. Carryon uses
// it to warn when the destination, its own binary or its config lives there: the root service starts
// at boot, before any user mount exists, and root usually can't read a user's FUSE mount anyway.
func (t MountTable) OnRemoteMount(path string) (Mount, bool) {
	m, ok := t.Containing(path)
	if !ok || !m.Remote() {
		return Mount{}, false
	}
	return m, true
}

// DirectBackend suggests a restic repository URL that reaches path's remote without going through
// the mount: a fuse.rclone mount of "remote:dir" becomes "rclone:remote:dir/<rest>", and an sshfs
// mount of "user@host:/dir" becomes "sftp:user@host:/dir/<rest>". ok is false when Carryon knows no
// direct backend for the mount (GVFS, NFS, SMB...); the user then has to pick one.
func (t MountTable) DirectBackend(path string) (url string, ok bool) {
	path = filepath.Clean(path)
	m, found := t.Containing(path)
	if !found {
		return "", false
	}
	rel := strings.TrimPrefix(strings.TrimPrefix(path, m.MountPoint), "/")
	if m.Root != "" && m.Root != "/" { // a bind mount of a folder of the remote
		rel = joinRel(strings.TrimPrefix(m.Root, "/"), rel)
	}
	switch m.FSType {
	case "fuse.rclone":
		remote, dir, ok := strings.Cut(m.Source, ":")
		if !ok || remote == "" {
			return "", false
		}
		return "rclone:" + remote + ":" + joinRel(dir, rel), true
	case "fuse.sshfs":
		host, dir, ok := strings.Cut(m.Source, ":")
		if !ok || host == "" {
			return "", false
		}
		if dir == "" {
			// sshfs "host:" is the remote home folder; sftp: wants a path, and relative paths are
			// taken from the home folder too.
			if rel == "" {
				return "", false
			}
			return "sftp:" + host + ":" + rel, true
		}
		return "sftp:" + host + ":" + joinRel(dir, rel), true
	}
	return "", false
}

// joinRel joins a remote folder and a relative path with one slash, keeping dir's own form ("",
// "/", "dir", "/dir/").
func joinRel(dir, rel string) string {
	switch {
	case rel == "":
		return dir
	case dir == "" || strings.HasSuffix(dir, "/"):
		return dir + rel
	default:
		return dir + "/" + rel
	}
}
