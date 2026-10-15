// Package dest guards backup destinations. See docs/design.md, "Destinations and mount guards".
//
// Before every run Carryon checks that a local destination is still the disk and repository it
// recorded at init, so it never writes into an empty mount point (which fills the system disk) or into
// someone else's repository. It also spots FUSE and network mounts, which root usually can't read and
// which can report a write as done before it reaches the remote, and suggests the direct backend.
//
// Every reader takes a root path, so tests use fake trees instead of the real /proc and /dev.
package dest

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Mount is one line of /proc/self/mountinfo.
type Mount struct {
	ID           int      // mount ID (reused after unmount)
	ParentID     int      // ID of the parent mount
	Major, Minor int      // device number (st_dev) of files on this mount
	Root         string   // folder of the filesystem mounted here (not "/" for bind mounts and btrfs subvolumes)
	MountPoint   string   // where it is mounted
	Options      []string // per-mount options (rw, noatime...)
	Optional     []string // optional fields (shared:1, master:2...)
	FSType       string   // ext4, btrfs, fuse.rclone, nfs4...
	Source       string   // /dev/sdb1, remote:path, user@host:/path, server:/export...
	SuperOptions []string // per-filesystem options
}

// MountTable is the list of mounts, in the kernel's order: a later mount at the same place hides an
// earlier one.
type MountTable []Mount

// MountinfoPath is where the mount table is read from, relative to the root.
const MountinfoPath = "/proc/self/mountinfo"

// ReadMounts reads the mount table under root ("/" for the real system).
func ReadMounts(root string) (MountTable, error) {
	path := filepath.Join(root, MountinfoPath)
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("reading the mount table: %w", err)
	}
	defer f.Close()
	mounts, err := ParseMountinfo(f)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	return mounts, nil
}

// ParseMountinfo parses the format of /proc/<pid>/mountinfo (see proc(5)):
//
//	36 35 98:0 /mnt1 /mnt2 rw,noatime master:1 - ext3 /dev/root rw,errors=continue
func ParseMountinfo(r io.Reader) (MountTable, error) {
	var mounts MountTable
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		m, err := parseMountLine(line)
		if err != nil {
			return nil, fmt.Errorf("line %d: %w", n, err)
		}
		mounts = append(mounts, m)
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return mounts, nil
}

func parseMountLine(line string) (Mount, error) {
	f := strings.Fields(line)
	sep := -1
	for i := 6; i < len(f); i++ {
		if f[i] == "-" {
			sep = i
			break
		}
	}
	if sep < 0 || len(f) < sep+3 {
		return Mount{}, fmt.Errorf("unexpected format: %q", line)
	}
	var m Mount
	var err error
	if m.ID, err = strconv.Atoi(f[0]); err != nil {
		return Mount{}, fmt.Errorf("bad mount ID %q", f[0])
	}
	if m.ParentID, err = strconv.Atoi(f[1]); err != nil {
		return Mount{}, fmt.Errorf("bad parent ID %q", f[1])
	}
	maj, min, ok := strings.Cut(f[2], ":")
	if !ok {
		return Mount{}, fmt.Errorf("bad device number %q", f[2])
	}
	if m.Major, err = strconv.Atoi(maj); err != nil {
		return Mount{}, fmt.Errorf("bad device number %q", f[2])
	}
	if m.Minor, err = strconv.Atoi(min); err != nil {
		return Mount{}, fmt.Errorf("bad device number %q", f[2])
	}
	m.Root = unescape(f[3])
	m.MountPoint = unescape(f[4])
	m.Options = splitOptions(f[5])
	if sep > 6 {
		m.Optional = f[6:sep]
	}
	m.FSType = unescape(f[sep+1])
	m.Source = unescape(f[sep+2])
	if len(f) > sep+3 {
		m.SuperOptions = splitOptions(f[sep+3])
	}
	return m, nil
}

func splitOptions(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(unescape(s), ",")
}

// unescape undoes the kernel's octal escapes (\040 for a space, \011 tab, \012 newline, \134
// backslash). Anything that isn't a valid three-digit octal escape is kept as it is.
func unescape(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) && s[i+1] >= '0' && s[i+1] <= '3' && isOctal(s[i+2]) && isOctal(s[i+3]) {
			v := (s[i+1]-'0')<<6 | (s[i+2]-'0')<<3 | (s[i+3] - '0')
			b.WriteByte(v)
			i += 3
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

func isOctal(c byte) bool { return c >= '0' && c <= '7' }

// Containing returns the mount that holds path: the deepest mount point that is path or one of its
// parents, and of several mounts at the same place the last one. path must be absolute and should
// have its symlinks resolved (see ResolvePath). ok is false only when the table has no mount for it.
func (t MountTable) Containing(path string) (m Mount, ok bool) {
	path = filepath.Clean(path)
	best := -1
	for i, cand := range t {
		if !within(path, cand.MountPoint) {
			continue
		}
		if best < 0 || len(cand.MountPoint) >= len(t[best].MountPoint) {
			best = i
		}
	}
	if best < 0 {
		return Mount{}, false
	}
	return t[best], true
}

// Under returns the mounts whose mount point lies strictly inside one of dirs, in table order, each
// once. A folder's own mount (say /home on its own partition) isn't included. Carryon uses this to
// leave cloud folders and network shares inside home folders out of the backup, and to report the
// other filesystems it doesn't cross.
func (t MountTable) Under(dirs ...string) []Mount {
	var out []Mount
	for _, m := range t {
		for _, d := range dirs {
			d = filepath.Clean(d)
			if m.MountPoint != d && within(m.MountPoint, d) {
				out = append(out, m)
				break
			}
		}
	}
	return out
}

// within reports whether path is dir or inside it. Both must be clean.
func within(path, dir string) bool {
	if dir == "/" {
		return strings.HasPrefix(path, "/")
	}
	return path == dir || strings.HasPrefix(path, dir+"/")
}
