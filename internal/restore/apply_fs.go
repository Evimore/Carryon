package restore

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// PreRestore is added to the name of a file a restore replaces. An existing copy is kept, so
// running the restore again doesn't lose the version that was there first.
const PreRestore = ".pre-restore"

// maxAdapt is the largest text file whose paths are adapted, as in the prototype's adapt_path.
const maxAdapt = 2 << 20

// maxLinks is how many symlinks resolving one path may follow.
const maxLinks = 40

// errLinkLoop is returned for a path with too many symlinks.
var errLinkLoop = errors.New("too many levels of symbolic links")

// splitPath splits an absolute system path into its names.
func splitPath(abs string) []string {
	var out []string
	for _, p := range strings.Split(abs, "/") {
		if p != "" && p != "." {
			out = append(out, p)
		}
	}
	return out
}

// resolveDir follows the symlinks in a folder's system path, staying inside the root: a link to
// /etc in a fake tree points at the tree's etc, never the real one. Names that don't exist yet are
// kept as they are.
func (m *Machine) resolveDir(abs string) (string, error) {
	parts := splitPath(abs)
	cur := "/"
	for hops := 0; len(parts) > 0; {
		next := path.Join(cur, parts[0])
		fi, err := os.Lstat(m.path(next))
		if errors.Is(err, fs.ErrNotExist) {
			return path.Join(append([]string{next}, parts[1:]...)...), nil
		}
		if err != nil {
			return "", err
		}
		if fi.Mode()&os.ModeSymlink == 0 {
			cur, parts = next, parts[1:]
			continue
		}
		if hops++; hops > maxLinks {
			return "", fmt.Errorf("%s: %w", abs, errLinkLoop)
		}
		t, err := os.Readlink(m.path(next))
		if err != nil {
			return "", err
		}
		if !path.IsAbs(t) {
			t = path.Join(cur, t)
		}
		parts = append(splitPath(t), parts[1:]...)
		cur = "/"
	}
	return cur, nil
}

// resolve returns where writing the system path would really go: its folder with symlinks
// followed, plus its own name (a symlink there is replaced, never followed).
func (m *Machine) resolve(abs string) (string, error) {
	dir, err := m.resolveDir(path.Dir(abs))
	if err != nil {
		return "", err
	}
	return path.Join(dir, path.Base(abs)), nil
}

// cleanAbs reports whether p is an absolute path in its shortest form, as recorded paths are.
func cleanAbs(p string) bool { return path.IsAbs(p) && path.Clean(p) == p }

// within reports whether the system path p is dir or inside it.
func within(p, dir string) bool {
	dir = path.Clean(dir)
	return p == dir || strings.HasPrefix(p, strings.TrimSuffix(dir, "/")+"/")
}

// protected are system paths a restore never writes: the review-only files and the initramfs
// config, which is copied only from the files phase after the boot-risk check.
var protected = []string{
	"/etc/crypttab", "/etc/default/grub", "/etc/default/limine", "/etc/fstab", "/etc/kernel/cmdline",
	"/etc/mkinitcpio.conf", "/etc/mkinitcpio.conf.d", "/etc/mkinitcpio.d", "/etc/sdboot-manage.conf",
}

// coversProtected reports whether restoring the path or pattern p from the backup would write a
// protected path: p is one, holds one, or matches one or a folder holding one.
func coversProtected(p string) (string, bool) {
	for _, q := range protected {
		for a := q; ; a = path.Dir(a) {
			if ok, _ := path.Match(p, a); ok || a == p {
				return q, true
			}
			if a == "/" {
				break
			}
		}
		if within(q, p) || within(p, q) {
			return q, true
		}
	}
	return "", false
}

// keepOld moves what is at the root path p out of the way as p.pre-restore, or removes it when an
// earlier restore's copy is already there.
func keepOld(p string) error {
	if _, err := os.Lstat(p); errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if _, err := os.Lstat(p + PreRestore); err == nil {
		return os.Remove(p)
	}
	return os.Rename(p, p+PreRestore)
}

// copyOld copies the file at the root path p to p.pre-restore, unless that exists already.
func copyOld(p string) error {
	if _, err := os.Lstat(p + PreRestore); err == nil {
		return nil
	}
	fi, err := os.Stat(p)
	if err != nil {
		return err
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return err
	}
	return writeNew(p+PreRestore, data, fi.Mode().Perm())
}

// writeNew creates a file that isn't there yet, with exactly this mode. It never follows a
// symlink: one in the way makes it fail.
func writeNew(p string, data []byte, mode fs.FileMode) error {
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Chmod(p, mode)
}

// replaceFile writes data over the file at the root path p, keeping its mode, by writing a new
// file next to it and renaming it into place.
func replaceFile(p string, data []byte) error {
	fi, err := os.Lstat(p)
	if err != nil {
		return err
	}
	tmp := p + ".carryon-new"
	os.Remove(tmp)
	if err := writeNew(tmp, data, fi.Mode().Perm()); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

// parseMode reads a recorded octal mode such as "644"; "" or a bad one gives 0644.
func parseMode(s string) fs.FileMode {
	n, err := strconv.ParseUint(s, 8, 32)
	if err != nil || s == "" {
		return 0o644
	}
	return fs.FileMode(n) & fs.ModePerm
}

// copyTree copies the file, symlink or folder at src to dst, keeping modes, times and symlinks.
// Folders are merged into what is there; a symlink or file in the way of a folder is replaced,
// never followed. It returns every path it created or changed, for handing them to their owner.
func copyTree(src, dst string) ([]string, error) {
	var touched, dirs []string
	type dirMode struct {
		path string
		mode fs.FileMode
	}
	var modes []dirMode
	err := filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		target := filepath.Join(dst, rel)
		fi, err := os.Lstat(p)
		if err != nil {
			return err
		}
		have, herr := os.Lstat(target)
		exists := herr == nil
		switch {
		case fi.IsDir():
			if exists && !have.IsDir() {
				if err := os.Remove(target); err != nil {
					return err
				}
				exists = false
			}
			if !exists {
				if err := os.Mkdir(target, 0o700); err != nil {
					return err
				}
			}
			dirs = append(dirs, target)
			modes = append(modes, dirMode{target, fi.Mode().Perm()})
			return nil
		case exists && have.IsDir():
			return fmt.Errorf("%s is a folder here, but not in the backup: move it away and restore again", target)
		case exists:
			if err := os.Remove(target); err != nil {
				return err
			}
		}
		if fi.Mode()&os.ModeSymlink != 0 {
			t, err := os.Readlink(p)
			if err != nil {
				return err
			}
			if err := os.Symlink(t, target); err != nil {
				return err
			}
			touched = append(touched, target)
			return nil
		}
		if !fi.Mode().IsRegular() {
			return nil // sockets and pipes aren't restored
		}
		if err := copyFile(p, target, fi); err != nil {
			return err
		}
		touched = append(touched, target)
		return nil
	})
	for i := len(modes) - 1; i >= 0; i-- {
		if cerr := os.Chmod(modes[i].path, modes[i].mode); cerr != nil && err == nil {
			err = cerr
		}
	}
	return append(dirs, touched...), err
}

func copyFile(src, dst string, fi fs.FileInfo) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, fi.Mode().Perm())
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	if err := os.Chmod(dst, fi.Mode().Perm()); err != nil {
		return err
	}
	return os.Chtimes(dst, fi.ModTime(), fi.ModTime())
}

// adaptTree rewrites the recorded homes in text files (up to 2 MiB) and symlinks under the root
// path p, for the users who receive them. It returns how many it changed. Ported from adapt_path.
func adaptTree(a *Adapter, p string) int {
	if !a.Active() {
		return 0
	}
	changed := 0
	filepath.WalkDir(p, func(f string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		fi, err := os.Lstat(f)
		if err != nil {
			return nil
		}
		switch {
		case fi.Mode()&os.ModeSymlink != 0:
			t, err := os.Readlink(f)
			if err != nil {
				return nil
			}
			if nt := a.String(t); nt != t {
				if os.Remove(f) == nil && os.Symlink(nt, f) == nil {
					changed++
				}
			}
		case fi.Mode().IsRegular() && fi.Size() <= maxAdapt:
			data, err := os.ReadFile(f)
			if err != nil {
				return nil
			}
			if nd := a.Bytes(data); string(nd) != string(data) {
				if replaceFile(f, nd) == nil {
					os.Chtimes(f, fi.ModTime(), fi.ModTime())
					changed++
				}
			}
		}
		return nil
	})
	return changed
}

// ids maps the account and group names in a system's /etc/passwd and /etc/group to their numbers.
type ids struct {
	users, groups map[string]int
}

// readIDs reads the names under the root. Missing files give empty maps.
func readIDs(root string) ids {
	read := func(name string) map[string]int {
		out := map[string]int{}
		data, _ := os.ReadFile(filepath.Join(root, "etc", name))
		for _, line := range strings.Split(string(data), "\n") {
			f := strings.Split(line, ":")
			if len(f) < 3 || f[0] == "" || strings.HasPrefix(f[0], "#") {
				continue
			}
			if n, err := strconv.Atoi(f[2]); err == nil {
				out[f[0]] = n
			}
		}
		return out
	}
	return ids{users: read("passwd"), groups: read("group")}
}

// owner returns the numbers of a "user:group" owner, and false when either name is unknown here.
func (x ids) owner(s string) (uid, gid int, ok bool) {
	u, g, _ := strings.Cut(s, ":")
	uid, ok1 := x.users[u]
	gid, ok2 := x.groups[g]
	return uid, gid, ok1 && ok2
}

// ownerOf returns the numeric owner and group of a path, without following a symlink.
func ownerOf(p string) (uid, gid int, ok bool) {
	fi, err := os.Lstat(p)
	if err != nil {
		return 0, 0, false
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0, false
	}
	return int(st.Uid), int(st.Gid), true
}
