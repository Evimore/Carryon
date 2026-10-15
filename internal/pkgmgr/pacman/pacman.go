// Package pacman is the package manager backend for Arch Linux and the distributions built on it.
//
// Every parser is tested with recorded pacman output in testdata/. Commands whose output is
// translated run with LC_ALL=C so the parsers always see English.
package pacman

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/Evimore/carryon/internal/pkgmgr"
)

// ownerBatch is how many paths go into one `pacman -Qo` call, to stay well under the argument limit.
const ownerBatch = 200

// Options configure the backend.
type Options struct {
	// Root is where the system's files are read from: pacman.conf, os-release, keyrings. Tests
	// point it at a fake tree. Empty means "/". Programs always run on the real system.
	Root string
	// ExtraStandardRepos are repositories to treat as the distribution's own
	// (config: pacman.extra_standard_repos).
	ExtraStandardRepos []string
	// TempDir is where the scratch keyring for listing distribution keys is made. Empty means the
	// system's temp folder.
	TempDir string
}

// Pacman is the pacman backend.
type Pacman struct {
	run pkgmgr.Runner
	opt Options
}

var _ pkgmgr.Manager = (*Pacman)(nil)

// New returns a pacman backend that runs programs through run.
func New(run pkgmgr.Runner, opt Options) *Pacman {
	if opt.Root == "" {
		opt.Root = "/"
	}
	return &Pacman{run: run, opt: opt}
}

// Name returns "pacman".
func (p *Pacman) Name() string { return "pacman" }

// Present reports whether pacman is installed.
func (p *Pacman) Present() bool { return p.run.Exists("pacman") }

// path returns an absolute system path under the root.
func (p *Pacman) path(abs string) string { return filepath.Join(p.opt.Root, abs) }

// exitCode returns the exit code of a program that ran and failed, or -1 for other errors.
func exitCode(err error) int {
	var e interface{ ExitCode() int }
	if errors.As(err, &e) {
		return e.ExitCode()
	}
	return -1
}

// pacman runs pacman quietly. Queries exit with 1 when nothing matches (no orphans, no foreign
// packages), so that counts as empty output, not a failure.
func (p *Pacman) pacman(ctx context.Context, args ...string) ([]byte, error) {
	out, err := p.run.Output(ctx, "pacman", args...)
	if err != nil && exitCode(err) != 1 {
		return nil, fmt.Errorf("running pacman %s: %w", strings.Join(args, " "), err)
	}
	return out, nil
}

// pacmanC runs pacman in the C locale, for output whose words get parsed. Any exit code is
// accepted: these commands exit non-zero when some file isn't owned or was altered.
func (p *Pacman) pacmanC(ctx context.Context, args ...string) ([]byte, error) {
	out, err := p.run.Output(ctx, "env", append([]string{"LC_ALL=C", "pacman"}, args...)...)
	if err != nil && exitCode(err) <= 0 {
		return nil, fmt.Errorf("running pacman %s: %w", strings.Join(args, " "), err)
	}
	return out, nil
}

func (p *Pacman) names(ctx context.Context, args ...string) ([]string, error) {
	out, err := p.pacman(ctx, args...)
	if err != nil {
		return nil, err
	}
	return strings.Fields(string(out)), nil
}

// Packages lists explicitly installed packages with their repository, foreign ones, then
// dependencies that are only optionally needed (true orphans aren't listed). Like the prototype's
// capture_packages.
func (p *Pacman) Packages(ctx context.Context) ([]pkgmgr.Package, error) {
	// -Qq fails only when the database can't be read, so an empty answer from the filtered
	// queries below really means "none" and not "pacman is broken".
	if _, err := p.run.Output(ctx, "pacman", "-Qq"); err != nil {
		return nil, fmt.Errorf("reading the installed packages: %w", err)
	}
	sl, err := p.pacman(ctx, "-Sl")
	if err != nil {
		return nil, err
	}
	repoOf := parseSyncList(sl)
	queries := []struct {
		args    []string
		foreign bool
		asDeps  bool
	}{
		{[]string{"-Qqen"}, false, false},
		{[]string{"-Qqem"}, true, false},
		{[]string{"-Qqdttn"}, false, true},
		{[]string{"-Qqdttm"}, true, true},
	}
	orphanList, err := p.names(ctx, "-Qqdt")
	if err != nil {
		return nil, err
	}
	orphans := make(map[string]bool, len(orphanList))
	for _, o := range orphanList {
		orphans[o] = true
	}
	var pkgs []pkgmgr.Package
	for _, q := range queries {
		names, err := p.names(ctx, q.args...)
		if err != nil {
			return nil, err
		}
		for _, n := range names {
			if q.asDeps && orphans[n] {
				continue
			}
			pkg := pkgmgr.Package{Name: n, Foreign: q.foreign, AsDeps: q.asDeps}
			if !q.foreign {
				pkg.Repo = pkgmgr.UnknownRepo
				if r, ok := repoOf[n]; ok {
					pkg.Repo = r
				}
			}
			pkgs = append(pkgs, pkg)
		}
	}
	return pkgs, nil
}

// Installed lists every installed package (`pacman -Qq`).
func (p *Pacman) Installed(ctx context.Context) ([]string, error) {
	out, err := p.run.Output(ctx, "pacman", "-Qq")
	if err != nil {
		return nil, fmt.Errorf("reading the installed packages: %w", err)
	}
	return strings.Fields(string(out)), nil
}

// Versions returns `pacman -Q`: every installed package with its version.
func (p *Pacman) Versions(ctx context.Context) ([]byte, error) {
	out, err := p.run.Output(ctx, "pacman", "-Q")
	if err != nil {
		return nil, fmt.Errorf("reading the installed packages: %w", err)
	}
	return out, nil
}

// SyncPackages lists the packages the configured repositories offer (`pacman -Slq`).
func (p *Pacman) SyncPackages(ctx context.Context) ([]string, error) {
	return p.names(ctx, "-Slq")
}

// SyncRepos maps each package the configured repositories offer to its repository (`pacman -Sl`).
// When several repositories have a package, the first one listed wins, as it does for pacman.
func (p *Pacman) SyncRepos(ctx context.Context) (map[string]string, error) {
	out, err := p.pacman(ctx, "-Sl")
	if err != nil {
		return nil, err
	}
	return parseSyncList(out), nil
}

// OwnedFiles lists every path an installed package owns (`pacman -Qlq`), without trailing slashes.
func (p *Pacman) OwnedFiles(ctx context.Context) ([]string, error) {
	out, err := p.pacman(ctx, "-Qlq")
	if err != nil {
		return nil, err
	}
	var paths []string
	for _, line := range strings.Split(string(out), "\n") {
		if line = strings.TrimRight(line, "/"); line != "" {
			paths = append(paths, line)
		}
	}
	return paths, nil
}

// FilesOf lists the paths each package owns (`pacman -Ql`), as pacman prints them (folders end
// in a slash).
func (p *Pacman) FilesOf(ctx context.Context, pkgs ...string) (map[string][]string, error) {
	if len(pkgs) == 0 {
		return map[string][]string{}, nil
	}
	out, err := p.pacman(ctx, append([]string{"-Ql"}, pkgs...)...)
	if err != nil {
		return nil, err
	}
	return parseFileList(out), nil
}

// Owners maps each path a package owns to that package, asking pacman 200 paths at a time.
// pacman answers with resolved paths (/lib/x is /usr/lib/x, folders end in a slash), so answers
// are mapped back to the paths as given. Like the prototype's file_owners.
func (p *Pacman) Owners(ctx context.Context, paths []string) (map[string]string, error) {
	real := map[string][]string{}
	var queries []string
	for _, given := range paths {
		if given == "" {
			continue
		}
		r := p.realpath(given)
		if _, seen := real[r]; !seen {
			queries = append(queries, r)
		}
		real[r] = append(real[r], given)
	}
	owners := map[string]string{}
	for i := 0; i < len(queries); i += ownerBatch {
		batch := queries[i:min(i+ownerBatch, len(queries))]
		out, err := p.pacmanC(ctx, append([]string{"-Qo"}, batch...)...)
		if err != nil {
			return nil, err
		}
		for _, o := range parseOwners(out) {
			for _, given := range real[p.realpath(strings.TrimRight(o.path, "/"))] {
				owners[given] = o.pkg
			}
		}
	}
	return owners, nil
}

// realpath resolves symlinks in an absolute path under the root, like Python's os.path.realpath.
// A path that doesn't exist keeps its last part. Absolute symlinks are followed on the host, so
// fake trees in tests use relative ones.
func (p *Pacman) realpath(abs string) string {
	clean := filepath.Clean(abs)
	full := p.path(clean)
	r, err := filepath.EvalSymlinks(full)
	if err != nil {
		dir, derr := filepath.EvalSymlinks(filepath.Dir(full))
		if derr != nil {
			return clean
		}
		r = filepath.Join(dir, filepath.Base(full))
	}
	rel, err := filepath.Rel(p.opt.Root, r)
	if err != nil || rel == ".." || strings.HasPrefix(rel, "../") {
		return clean
	}
	return filepath.Join("/", rel)
}

// ModifiedConfigs lists the backup= files that differ from their package (`pacman -Qii`).
func (p *Pacman) ModifiedConfigs(ctx context.Context) ([]string, error) {
	out, err := p.pacmanC(ctx, "-Qii")
	if err != nil {
		return nil, err
	}
	var paths []string
	for _, b := range parseBackupFiles(out) {
		if b.status == "modified" {
			paths = append(paths, b.path)
		}
	}
	return paths, nil
}

// EditedFiles lists package files whose size or checksum differs from the package
// (`pacman -Qkk`), leaving out backup= files (ModifiedConfigs has those) and files where only the
// modification time differs.
//
// This reads and checksums every file of every package, which takes minutes. It's meant for the
// nightly capture only, never for the pacman hook.
func (p *Pacman) EditedFiles(ctx context.Context) ([]pkgmgr.EditedFile, error) {
	// pacman prints the altered files as warnings on stderr, so both streams are needed.
	out, err := p.run.Output(ctx, "env", "LC_ALL=C", "sh", "-c", "exec pacman -Qkk 2>&1")
	if err != nil && exitCode(err) <= 0 {
		return nil, fmt.Errorf("checking package files: %w", err)
	}
	return parseCheck(out), nil
}

// ConfiguredRepos lists the repositories in pacman.conf, in order (`pacman-conf --repo-list`).
func (p *Pacman) ConfiguredRepos(ctx context.Context) ([]string, error) {
	out, err := p.run.Output(ctx, "pacman-conf", "--repo-list")
	if err != nil {
		return nil, fmt.Errorf("reading the repository list from pacman.conf: %w", err)
	}
	return strings.Fields(string(out)), nil
}

// readFile reads a system file under the root.
func (p *Pacman) readFile(abs string) ([]byte, error) {
	return os.ReadFile(p.path(abs))
}

// lines splits output into lines without empty ones.
func lines(out []byte) []string {
	var ls []string
	for _, l := range bytes.Split(out, []byte("\n")) {
		if s := strings.TrimRight(string(l), "\r"); s != "" {
			ls = append(ls, s)
		}
	}
	return ls
}
