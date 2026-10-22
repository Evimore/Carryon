package capture

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"

	"github.com/Evimore/carryon/internal/record"
)

// notCaptured matches system files that are never recorded: machine-specific or generated files,
// secrets restored from the encrypted backup instead, and Carryon's own files. From the
// prototype's NOT_CAPTURED, with Carryon's files in place of the prototype's own.
var notCaptured = regexp.MustCompile(strings.Join([]string{
	`^/etc/(machine-id|adjtime|ld\.so\.cache|\.pwd\.lock|\.updated|mtab|resolv\.conf)$`,
	`^/etc/(passwd|group|shadow|gshadow|subuid|subgid)-?$`,
	`^/etc/(ca-certificates/extracted|ssl/certs|pacman\.d/gnupg|fonts/conf\.d)/`,
	`^/etc/ssh/ssh_host_[^/]*_key(\.pub)?$`,    // identity: restored from the encrypted backup
	`^/etc/NetworkManager/system-connections/`, // wifi passwords: same
	`^/etc/(cachyos/installation-snapshot|os-release)$`,
	// Carryon itself: the package (or init) puts it on each machine, and it's in every backup
	`^/etc/carryon/`, `^/usr/local/(lib/carryon/|bin/carryon$)`,
	`^/etc/pacman\.d/hooks/[^/]*carryon[^/]*\.hook$`, `^/etc/systemd/(system|user)/carryon`,
	`^/usr/lib/modules/`, `^/usr/share/mime/`, `/__pycache__/`, `\.pyc$`,
	`\.(pacnew|pacsave|pacorig|pre-restore|bak|old|orig)$`, `mirrorlist$`, `\.cache$`,
	`^/usr/lib/locale/locale-archive$`, `^/(etc|usr/lib)/udev/hwdb\.bin$`,
	`^/usr/share/glib-2\.0/schemas/gschemas\.compiled$`, `^/usr/share/info/dir$`,
	`^/usr/share/vim/vimfiles/doc/tags$`, `^/etc/xml/catalog$`,
}, "|"))

// handledElsewhere are files other items cover (users, groups, shells, repositories).
var handledElsewhere = map[string]bool{
	"/etc/passwd": true, "/etc/group": true, "/etc/shadow": true, "/etc/gshadow": true,
	"/etc/subuid": true, "/etc/subgid": true, "/etc/shells": true, "/etc/pacman.conf": true,
	"/etc/resolv.conf": true,
}

// NeverCaptured reports whether a system file is never recorded as a file item.
func NeverCaptured(path string) bool {
	return notCaptured.MatchString(path) || handledElsewhere[path]
}

// systemDirs are walked for files no package owns.
var systemDirs = []string{"/etc", "/usr", "/opt"}

// capturePackages records packages, extra repositories and their signing keys. Ported from
// capture_packages and capture_repos; the parsing is in the package manager backend.
func (c *capturer) capturePackages(ctx context.Context) error {
	pm := c.opt.Packages
	repoOf, err := pm.SyncRepos(ctx)
	if err != nil {
		return err
	}
	c.res.RepoOf = repoOf
	pkgs, err := pm.Packages(ctx)
	if err != nil {
		return err
	}
	for _, p := range pkgs {
		if p.Foreign {
			c.add(record.Item{Type: record.TypeAUR, ID: p.Name, AsDeps: p.AsDeps})
		} else {
			c.add(record.Item{Type: record.TypePkg, ID: p.Name, Repo: p.Repo, AsDeps: p.AsDeps})
		}
	}
	repos, err := pm.ExtraRepos(ctx)
	if err != nil {
		return err
	}
	if len(repos) == 0 {
		return nil
	}
	// signing keys added by hand go with every extra repository: which key signs which isn't known
	keys, err := pm.ExtraKeys(ctx)
	if err != nil {
		return err
	}
	var fprs, names []string
	for _, k := range keys {
		fprs = append(fprs, k.Fingerprint)
		names = append(names, k.UID)
	}
	if len(fprs) > 0 {
		armored, err := pm.ExportKeys(ctx, fprs)
		if err != nil {
			return err
		}
		if err := c.write(RepoKeysFile, armored); err != nil {
			return err
		}
	}
	for _, r := range repos {
		if r.Conf != "" {
			c.res.RepoFiles = append(c.res.RepoFiles, r.Conf)
		}
		c.add(record.Item{Type: record.TypeRepo, ID: r.Name, Conf: r.Conf, Section: r.Section, Keys: fprs, KeyNames: names})
	}
	return nil
}

// captureFiles records system files that differ from their package (modified: backup= files;
// edited: other package files, nightly only) and files in /etc, /usr and /opt no package owns, and
// copies them into files/. Ported from capture_files.
func (c *capturer) captureFiles(ctx context.Context) error {
	pm := c.opt.Packages
	owned, err := pm.OwnedFiles(ctx)
	if err != nil {
		return err
	}
	c.res.Owned = make(map[string]bool, len(owned))
	for _, p := range owned {
		c.res.Owned[strings.TrimRight(p, "/")] = true
	}
	modified, err := pm.ModifiedConfigs(ctx)
	if err != nil {
		return err
	}
	ownerOf, err := pm.Owners(ctx, modified)
	if err != nil {
		return err
	}
	type found struct {
		path, kind, pkg string
	}
	var files []found
	for _, p := range modified {
		files = append(files, found{p, record.KindModified, ownerOf[p]})
	}
	if c.opt.Nightly {
		edited, err := pm.EditedFiles(ctx)
		if err != nil {
			return err
		}
		for _, e := range edited {
			if underAny(e.Path, systemDirs) {
				files = append(files, found{e.Path, record.KindEdited, e.Package})
			}
		}
	}
	for _, p := range c.unownedFiles() {
		files = append(files, found{p, record.KindUnowned, ""})
	}

	repoFile := map[string]bool{}
	for _, f := range c.res.RepoFiles {
		repoFile[f] = true
	}
	seen := map[string]bool{}
	for _, f := range files {
		if seen[f.path] || NeverCaptured(f.path) || repoFile[f.path] {
			continue
		}
		seen[f.path] = true
		if it, ok := c.copyFile(f.path, f.kind, f.pkg); ok {
			c.add(it)
		}
	}
	return nil
}

// unownedFiles walks /etc, /usr and /opt, staying on each one's filesystem, for files and symlinks
// no package owns. Enable links in /etc/systemd are left out: services cover them.
func (c *capturer) unownedFiles() []string {
	var out []string
	for _, dir := range systemDirs {
		top := c.path(dir)
		st, err := os.Lstat(top)
		if err != nil || !st.IsDir() {
			continue
		}
		dev := device(st)
		filepath.WalkDir(top, func(full string, d fs.DirEntry, err error) error {
			abs := c.abs(full)
			if err != nil {
				if d != nil && d.IsDir() {
					c.skip(abs + "/")
				}
				return nil
			}
			if d.IsDir() {
				if fi, err := d.Info(); err == nil && device(fi) != dev {
					return filepath.SkipDir
				}
				return nil
			}
			if !d.Type().IsRegular() && d.Type()&fs.ModeSymlink == 0 {
				return nil
			}
			if c.res.Owned[abs] {
				return nil
			}
			if d.Type()&fs.ModeSymlink != 0 && (strings.HasPrefix(abs, "/etc/systemd/") || strings.HasPrefix(abs, "/etc/xdg/systemd/")) {
				return nil
			}
			out = append(out, abs)
			return nil
		})
	}
	return out
}

// copyFile makes the item for a system file and copies the file into the record. ok is false when
// the file is gone or couldn't be read (then it's in the skipped list).
func (c *capturer) copyFile(abs, kind, pkg string) (record.Item, bool) {
	full := c.path(abs)
	fi, err := os.Lstat(full)
	if err != nil {
		return record.Item{}, false
	}
	uid, gid := c.owner(full, fi)
	it := record.Item{
		Type:  record.TypeFile,
		ID:    abs,
		Kind:  kind,
		Mode:  fmt.Sprintf("%o", permBits(fi)),
		Owner: c.ids.owner(uid, gid),
	}
	if kind != record.KindUnowned {
		it.Pkg = pkg
	}
	dst := filepath.Join(c.stage, FilesDir, abs)
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		c.warn("can't copy %s into the record: %v", abs, err)
		return record.Item{}, false
	}
	switch {
	case fi.Mode()&fs.ModeSymlink != 0:
		target, err := os.Readlink(full)
		if err != nil {
			c.skip(abs)
			return record.Item{}, false
		}
		it.Link = target
		if err := os.Symlink(target, dst); err != nil {
			c.warn("can't copy %s into the record: %v", abs, err)
			return record.Item{}, false
		}
	case fi.Size() > MaxFile:
		it.TooBig = true
		c.warn("not copying %s (%d MiB); restore it by hand if you need it", abs, fi.Size()>>20)
	default:
		data, err := c.readFile(full)
		if err != nil {
			c.skip(abs)
			return record.Item{}, false
		}
		sum := sha256.Sum256(data)
		it.SHA = hex.EncodeToString(sum[:])
		if err := os.WriteFile(dst, data, 0o600); err == nil {
			err = os.Chmod(dst, fs.FileMode(permBits(fi)&0o777)) // the item has setuid and the like
		}
		if err != nil {
			c.warn("can't copy %s into the record: %v", abs, err)
			return record.Item{}, false
		}
	}
	return it, true
}

// permBits returns a file's permission bits with setuid, setgid and sticky, as stat's st_mode & 0o7777.
func permBits(fi fs.FileInfo) uint32 {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return uint32(st.Mode) & 0o7777
	}
	m := fi.Mode()
	bits := uint32(m.Perm())
	if m&fs.ModeSetuid != 0 {
		bits |= 0o4000
	}
	if m&fs.ModeSetgid != 0 {
		bits |= 0o2000
	}
	if m&fs.ModeSticky != 0 {
		bits |= 0o1000
	}
	return bits
}

// device is the filesystem a file is on.
func device(fi fs.FileInfo) uint64 {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return uint64(st.Dev) //nolint:unconvert // Dev's type differs by architecture
	}
	return 0
}

// abs turns a path under the root back into the system path.
func (c *capturer) abs(full string) string {
	rel, err := filepath.Rel(c.root, full)
	if err != nil || rel == "." {
		return "/"
	}
	return "/" + filepath.ToSlash(rel)
}

// underAny reports whether path is inside one of dirs.
func underAny(path string, dirs []string) bool {
	for _, d := range dirs {
		if strings.HasPrefix(path, d+"/") {
			return true
		}
	}
	return false
}
