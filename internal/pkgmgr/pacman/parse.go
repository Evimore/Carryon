package pacman

import (
	"regexp"
	"slices"
	"strings"

	"github.com/Evimore/carryon/internal/pkgmgr"
)

// parseSyncList maps each package in `pacman -Sl` ("repo name version [installed]") to its
// repository. The first repository listed wins, like pacman picks it.
func parseSyncList(out []byte) map[string]string {
	repoOf := map[string]string{}
	for _, line := range lines(out) {
		f := strings.Fields(line)
		if len(f) < 2 {
			continue
		}
		if _, ok := repoOf[f[1]]; !ok {
			repoOf[f[1]] = f[0]
		}
	}
	return repoOf
}

// parseFileList reads `pacman -Ql` ("package /path") into the paths of each package.
func parseFileList(out []byte) map[string][]string {
	files := map[string][]string{}
	for _, line := range lines(out) {
		pkg, path, ok := strings.Cut(line, " ")
		if ok && path != "" {
			files[pkg] = append(files[pkg], path)
		}
	}
	return files
}

type owner struct{ path, pkg string }

// ownedBy matches `pacman -Qo` answers: "/usr/bin/ls is owned by coreutils 9.5-1". Paths that no
// package owns are reported on stderr and aren't seen here.
var ownedBy = regexp.MustCompile(`^(.+) is owned by (\S+) `)

func parseOwners(out []byte) []owner {
	var owners []owner
	for _, line := range lines(out) {
		if m := ownedBy.FindStringSubmatch(line); m != nil {
			owners = append(owners, owner{m[1], m[2]})
		}
	}
	return owners
}

type backupFile struct{ path, status string }

// parseBackupFiles reads the backup= files from `pacman -Qii`. pacman prints them in one of two
// ways, depending on its version:
//
//	Backup Files    : /etc/pacman.conf [modified]
//	                  /etc/makepkg.conf [unmodified]
//
// or, under a "Backup Files" heading:
//
//	MODIFIED	/etc/pacman.conf
//
// The status is returned in lower case: modified, unmodified, missing, unreadable.
func parseBackupFiles(out []byte) []backupFile {
	var files []backupFile
	for _, line := range lines(out) {
		if status, path, ok := strings.Cut(line, "\t"); ok && isBackupStatus(strings.ToLower(status)) &&
			strings.HasPrefix(path, "/") {
			files = append(files, backupFile{path, strings.ToLower(status)})
			continue
		}
		s := strings.TrimSpace(line)
		if !strings.HasSuffix(s, "]") {
			continue
		}
		i := strings.LastIndex(s, " [")
		if i < 0 || !isBackupStatus(s[i+2:len(s)-1]) {
			continue
		}
		path, status := s[:i], s[i+2:len(s)-1]
		if !strings.HasPrefix(path, "/") {
			// the first file follows the "Backup Files    : " label
			j := strings.Index(path, ": /")
			if j < 0 {
				continue
			}
			path = path[j+2:]
		}
		files = append(files, backupFile{path, status})
	}
	return files
}

func isBackupStatus(s string) bool {
	switch s {
	case "modified", "unmodified", "missing", "unreadable":
		return true
	}
	return false
}

// editReasons are the `pacman -Qkk` findings that mean a file's contents changed. Modification
// time, permission and owner differences alone don't.
var editReasons = map[string]bool{
	"Size mismatch":            true,
	"MD5 checksum mismatch":    true,
	"SHA256 checksum mismatch": true,
}

// parseCheck reads `pacman -Qkk` output (stdout and stderr together, C locale) and returns the
// files whose contents differ from their package.
//
// pacman (src/pacman/check.c) reports each difference on its own line. Package files go to stderr
// as warnings; backup= files go to stdout with a "backup file" prefix, because they're expected
// to change:
//
//	warning: zsh: /etc/zsh/zprofile (Modification time mismatch)
//	warning: zsh: /usr/share/zsh/functions/Completion/_ls (Size mismatch)
//	warning: zsh: /usr/share/zsh/functions/Completion/_ls (SHA256 checksum mismatch)
//	backup file: pacman: /etc/pacman.conf (Size mismatch)
//	zsh: 1923 total files, 2 altered files
//
// Missing files ("(No such file or directory)") can't be backed up and are left out, and so are
// pacman's own database files (.INSTALL, .CHANGELOG).
func parseCheck(out []byte) []pkgmgr.EditedFile {
	var files []pkgmgr.EditedFile
	index := map[string]int{}
	for _, line := range lines(out) {
		rest, ok := strings.CutPrefix(line, "warning: ")
		if !ok {
			continue
		}
		pkg, rest, ok := strings.Cut(rest, ": ")
		if !ok || strings.Contains(pkg, " ") || !strings.HasSuffix(rest, ")") {
			continue
		}
		i := strings.LastIndex(rest, " (")
		if i < 0 {
			continue
		}
		path, reason := rest[:i], rest[i+2:len(rest)-1]
		if !editReasons[reason] || !strings.HasPrefix(path, "/") || strings.Contains(path, "/var/lib/pacman/local/") {
			continue
		}
		if n, seen := index[path]; seen {
			if !slices.Contains(files[n].Reasons, reason) {
				files[n].Reasons = append(files[n].Reasons, reason)
			}
			continue
		}
		index[path] = len(files)
		files = append(files, pkgmgr.EditedFile{Path: path, Package: pkg, Reasons: []string{reason}})
	}
	return files
}
