package restore

import (
	"bytes"
	"context"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
)

// fileLister lists the paths packages own, as pacman -Ql does (folders end in "/").
// pkgmgr.Manager provides it.
type fileLister interface {
	FilesOf(ctx context.Context, pkgs ...string) (map[string][]string, error)
}

// binDirs are the folders whose programs are also matched by name, for processes started through
// a link or a wrapper.
var binDirs = map[string]bool{"/usr/bin": true, "/usr/sbin": true, "/bin": true, "/sbin": true}

// appRunning tells whether u runs a program of the app being restored: prog names it, or is ""
// when none runs. known is false when Carryon can't tell (no app, its files can't be listed, or
// /proc can't be read), and the desktop check applies instead.
func (a *Applier) appRunning(ctx context.Context, u LocalUser) (prog string, known bool) {
	if len(a.AppPackages) == 0 {
		return "", false
	}
	fl, ok := a.Packages.(fileLister)
	if !ok {
		return "", false
	}
	files, err := fl.FilesOf(ctx, a.AppPackages...)
	if err != nil {
		return "", false
	}
	progs, names := map[string]bool{}, map[string]bool{}
	for _, list := range files {
		for _, f := range list {
			if strings.HasSuffix(f, "/") {
				continue
			}
			progs[f] = true
			if binDirs[path.Dir(f)] {
				names[path.Base(f)] = true
			}
		}
	}
	if len(progs) == 0 {
		return "", false
	}
	return runningOf(a.Here.root(), u.UID, progs, names)
}

// runningOf returns the name of a program in progs, or named in names, that a process of uid
// runs, or "". ok is false when the processes can't be read.
func runningOf(root string, uid int, progs, names map[string]bool) (prog string, ok bool) {
	procDir := filepath.Join(root, "proc")
	ents, err := os.ReadDir(procDir)
	if err != nil {
		return "", false
	}
	for _, e := range ents {
		if _, err := strconv.Atoi(e.Name()); err != nil {
			continue
		}
		dir := filepath.Join(procDir, e.Name())
		if id, ok := procUID(dir); !ok || id != uid {
			continue
		}
		if exe, err := os.Readlink(filepath.Join(dir, "exe")); err == nil {
			exe = strings.TrimSuffix(exe, " (deleted)")
			if progs[exe] {
				return path.Base(exe), true
			}
		}
		cmd, _ := os.ReadFile(filepath.Join(dir, "cmdline"))
		argv0, _, _ := bytes.Cut(cmd, []byte{0})
		if a0 := string(argv0); a0 != "" && (progs[a0] || names[path.Base(a0)]) {
			return path.Base(a0), true
		}
	}
	return "", true
}

// procUID reads a process's real user ID from its status file.
func procUID(dir string) (int, bool) {
	data, err := os.ReadFile(filepath.Join(dir, "status"))
	if err != nil {
		return 0, false
	}
	for _, l := range strings.Split(string(data), "\n") {
		if rest, ok := strings.CutPrefix(l, "Uid:"); ok {
			f := strings.Fields(rest)
			if len(f) == 0 {
				return 0, false
			}
			id, err := strconv.Atoi(f[0])
			return id, err == nil
		}
	}
	return 0, false
}
