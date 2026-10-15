package pacman

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/Evimore/carryon/internal/pkgmgr"
)

// Distro is the running distribution, from os-release.
type Distro struct {
	ID     string   // e.g. "cachyos"
	IDLike []string // e.g. ["arch"]
}

// Is reports whether the distribution is id or is based on it.
func (d Distro) Is(id string) bool {
	return d.ID == id || slices.Contains(d.IDLike, id)
}

// DetectDistro reads ID and ID_LIKE from /etc/os-release (or /usr/lib/os-release) under root.
// A system without either file gives an empty Distro and no error.
func DetectDistro(root string) (Distro, error) {
	for _, f := range []string{"etc/os-release", "usr/lib/os-release"} {
		data, err := readFileUnder(root, f)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return Distro{}, err
		}
		vals := parseOSRelease(data)
		d := Distro{ID: vals["ID"]}
		if like := strings.Fields(vals["ID_LIKE"]); len(like) > 0 {
			d.IDLike = like
		}
		return d, nil
	}
	return Distro{}, nil
}

// parseOSRelease reads os-release's KEY=value lines; values may be quoted.
func parseOSRelease(data []byte) map[string]string {
	vals := map[string]string{}
	for _, line := range lines(data) {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		if len(v) >= 2 && (v[0] == '"' || v[0] == '\'') && v[len(v)-1] == v[0] {
			if v[0] == '"' {
				if u, err := strconv.Unquote(v); err == nil {
					v = u
				} else {
					v = v[1 : len(v)-1]
				}
			} else {
				v = v[1 : len(v)-1]
			}
		}
		vals[k] = v
	}
	return vals
}

// archRepos are Arch Linux's repositories, which every Arch-based distribution keeps using:
// the current ones, their testing and staging versions, the desktop pre-release repositories, and
// the names Arch used before its 2023 repository merge (community, testing...).
var archRepos = regexp.MustCompile(`^((core|extra|multilib|community)(-testing|-staging)?|testing|staging|(gnome|kde)-unstable)$`)

// distroRepos are the repositories each distribution adds to Arch's, by os-release ID.
var distroRepos = map[string]*regexp.Regexp{
	// cachyos, cachyos-v3, cachyos-core-v3, cachyos-extra-znver4, cachyos-testing...
	"cachyos": regexp.MustCompile(`^cachyos.*$`),
	// endeavouros, plus the testing repository its developers offer
	"endeavouros": regexp.MustCompile(`^endeavouros(-testing-dev)?$`),
	// Manjaro mirrors Arch's names (core, extra, multilib, and community on older installs)
	"manjaro":     archRepos,
	"manjaro-arm": archRepos,
	// Garuda ships its own repository and Chaotic-AUR enabled out of the box
	"garuda": regexp.MustCompile(`^(garuda|chaotic-aur)$`),
}

// StandardRepos returns a test for repositories that are part of distribution d: Arch's
// repositories on every distribution, the distribution's own, and extra. Repositories that aren't
// standard are captured with their pacman.conf section so a restore can add them back.
//
// The prototype counted cachyos* as standard everywhere; here it's standard only on CachyOS, so
// CachyOS repositories added to plain Arch by hand are captured.
func StandardRepos(d Distro, extra []string) func(repo string) bool {
	var own []*regexp.Regexp
	for _, id := range append([]string{d.ID}, d.IDLike...) {
		if re, ok := distroRepos[id]; ok {
			own = append(own, re)
		}
	}
	return func(repo string) bool {
		if archRepos.MatchString(repo) || slices.Contains(extra, repo) {
			return true
		}
		for _, re := range own {
			if re.MatchString(repo) {
				return true
			}
		}
		return false
	}
}

// ExtraRepos lists the configured repositories that aren't the distribution's, each with its
// section from /etc/pacman.conf or /etc/pacman.d/*.conf. Like the prototype's capture_repos.
func (p *Pacman) ExtraRepos(ctx context.Context) ([]pkgmgr.Repo, error) {
	all, err := p.ConfiguredRepos(ctx)
	if err != nil {
		return nil, err
	}
	d, err := DetectDistro(p.opt.Root)
	if err != nil {
		return nil, fmt.Errorf("finding out which distribution this is: %w", err)
	}
	standard := StandardRepos(d, p.opt.ExtraStandardRepos)
	var custom []string
	for _, r := range all {
		if !standard(r) {
			custom = append(custom, r)
		}
	}
	if len(custom) == 0 {
		return nil, nil
	}
	defs, err := p.repoSections(custom)
	if err != nil {
		return nil, err
	}
	repos := make([]pkgmgr.Repo, 0, len(custom))
	for _, r := range custom {
		repo := pkgmgr.Repo{Name: r}
		if def, ok := defs[r]; ok {
			repo.Section = def.section
			if def.file != "/etc/pacman.conf" {
				repo.Conf = def.file
			}
		}
		repos = append(repos, repo)
	}
	return repos, nil
}

type repoDef struct{ file, section string }

// sectionHeader matches a "[name]" line, like the prototype's re.match(r"\s*\[([^\]]+)\]").
var sectionHeader = regexp.MustCompile(`^\s*\[([^\]]+)\]`)

// repoSections finds each repository's section in /etc/pacman.conf, then /etc/pacman.d/*.conf in
// name order. The first file defining a repository wins. A section runs from its header to the
// next one, comments included. Unreadable files are skipped.
func (p *Pacman) repoSections(names []string) (map[string]repoDef, error) {
	files := []string{"/etc/pacman.conf"}
	more, err := filepath.Glob(p.path("/etc/pacman.d/*.conf"))
	if err != nil {
		return nil, err
	}
	slices.Sort(more)
	for _, f := range more {
		rel, err := filepath.Rel(p.opt.Root, f)
		if err != nil {
			continue
		}
		files = append(files, filepath.Join("/", rel))
	}
	defs := map[string]repoDef{}
	for _, f := range files {
		data, err := p.readFile(f)
		if err != nil {
			continue
		}
		var order []string
		buf := map[string][]string{}
		sec := ""
		for _, line := range strings.Split(string(data), "\n") {
			if m := sectionHeader.FindStringSubmatch(line); m != nil {
				sec = m[1]
			}
			if slices.Contains(names, sec) {
				if _, ok := buf[sec]; !ok {
					order = append(order, sec)
				}
				buf[sec] = append(buf[sec], strings.TrimRight(line, "\r"))
			}
		}
		for _, r := range order {
			if _, ok := defs[r]; !ok {
				defs[r] = repoDef{f, strings.TrimSpace(strings.Join(buf[r], "\n")) + "\n"}
			}
		}
	}
	return defs, nil
}

func readFileUnder(root, rel string) ([]byte, error) {
	return os.ReadFile(filepath.Join(root, rel))
}
