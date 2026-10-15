package dest

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// sameDevice reports whether the device node at path is the device mounted as m. It catches what
// matching names can't (a source of /dev/root, say); a fake tree has no device nodes, so there it is
// always false.
func sameDevice(path string, m Mount) bool {
	fi, err := os.Stat(path)
	if err != nil || fi.Mode()&os.ModeDevice == 0 || m.Major == 0 {
		return false
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return false
	}
	rdev := uint64(st.Rdev)
	major := (rdev>>8)&0xfff | (rdev>>32)&^uint64(0xfff)
	minor := rdev&0xff | (rdev>>12)&^uint64(0xff)
	return major == uint64(m.Major) && minor == uint64(m.Minor)
}

// FreeSpace returns the bytes an unprivileged user can still write on the filesystem holding path
// (under root), as statfs reports it. Only call it for local destinations: on a FUSE mount it asks the
// remote, and on a stale one it can hang.
func FreeSpace(root, path string) (uint64, error) {
	var st syscall.Statfs_t
	full := filepath.Join(root, path)
	if err := syscall.Statfs(full, &st); err != nil {
		return 0, fmt.Errorf("checking free space on %s: %w", path, err)
	}
	return st.Bavail * uint64(st.Bsize), nil
}
