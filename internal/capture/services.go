package capture

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/Evimore/carryon/internal/record"
)

// unitProps reads LoadState and FragmentPath of units with one `systemctl show`.
func (c *capturer) unitProps(ctx context.Context, units []string) map[string]map[string]string {
	props := map[string]map[string]string{}
	if len(units) == 0 {
		return props
	}
	args := append([]string{"show", "-p", "LoadState", "-p", "FragmentPath", "--"}, units...)
	out, err := c.opt.Runner.Output(ctx, "systemctl", args...)
	if err != nil && len(out) == 0 {
		c.warn("can't read the services' unit files: %v", err)
		return props
	}
	// systemctl prints one block per unit, in the order asked, separated by blank lines
	blocks := strings.Split(strings.TrimSpace(string(out)), "\n\n")
	for i, b := range blocks {
		if i >= len(units) {
			break
		}
		p := map[string]string{}
		for _, ln := range strings.Split(b, "\n") {
			if k, v, ok := strings.Cut(ln, "="); ok {
				p[k] = v
			}
		}
		props[units[i]] = p
	}
	return props
}

// captureServices records enabled and masked system units with their owning package, then each
// user's enabled and masked user units, their unit files and drop-ins, and lingering. Ported from
// capture_services.
func (c *capturer) captureServices(ctx context.Context) error {
	var enabled, masked []string
	out, err := c.opt.Runner.Output(ctx, "systemctl", "list-unit-files", "--no-legend", "--no-pager", "--state=enabled,masked")
	if err != nil && len(out) == 0 {
		c.warn("can't list the enabled services: %v", err)
	}
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) < 2 || strings.HasPrefix(f[0], "carryon") || strings.HasPrefix(f[0], "sysbackup") || strings.Contains(f[0], "@.") {
			continue
		}
		if f[1] == "masked" {
			masked = append(masked, f[0])
		} else {
			enabled = append(enabled, f[0])
		}
	}
	// enabled template instances (getty@tty2.service) only show up as links
	links, _ := filepath.Glob(c.path("/etc/systemd/system/*.wants/*@?*"))
	for _, l := range links {
		if name := filepath.Base(l); !strings.Contains(name, "@.") {
			enabled = append(enabled, name)
		}
	}
	enabled = sortedSet(enabled)
	props := c.unitProps(ctx, enabled)
	var units, fragments []string
	for _, u := range enabled {
		// an enable link whose unit is gone (its package was removed) is a leftover, not something to restore
		if p, ok := props[u]; !ok || p["LoadState"] == "not-found" {
			continue
		}
		units = append(units, u)
		fragments = append(fragments, props[u]["FragmentPath"])
	}
	owners, err := c.opt.Packages.Owners(ctx, fragments)
	if err != nil {
		return err
	}
	for i, u := range units {
		c.add(record.Item{Type: record.TypeSvc, ID: u, UnitFile: fragments[i], Pkg: owners[fragments[i]]})
	}
	for _, u := range sortedSet(masked) {
		c.add(record.Item{Type: record.TypeMask, ID: u})
	}

	for _, u := range c.res.Users {
		if err := c.captureUserUnits(u); err != nil {
			return err
		}
		if _, err := os.Lstat(c.path(filepath.Join("/var/lib/systemd/linger", u.Name))); err == nil {
			c.add(record.Item{Type: record.TypeLinger, ID: u.Name})
		}
	}
	return nil
}

// UserUnitDir is where users keep their own units, relative to their home.
const UserUnitDir = ".config/systemd/user"

// captureUserUnits records one user's enabled and masked units and copies their unit files and
// drop-ins into user-units/<user>/.
//
// The folder belongs to the user and Carryon reads it as root, so it only reads what is really
// inside the home folder: not a unit folder that leads elsewhere through a symlink, no symlinked
// files, and only files the user owns.
func (c *capturer) captureUserUnits(u User) error {
	dir := c.path(filepath.Join(u.Home, UserUnitDir))
	fi, err := os.Stat(dir)
	if err != nil || !fi.IsDir() {
		return nil
	}
	realDir, err1 := filepath.EvalSymlinks(dir)
	realHome, err2 := filepath.EvalSymlinks(c.path(u.Home))
	if err1 != nil || err2 != nil || !strings.HasPrefix(realDir+"/", realHome+"/") {
		c.warn("not reading %s's units: ~/%s leads outside their home folder", u.Name, UserUnitDir)
		return nil
	}

	var wants []string
	for _, pat := range []string{"*.wants/*", "*.requires/*"} {
		m, _ := filepath.Glob(filepath.Join(realDir, pat))
		for _, p := range m {
			wants = append(wants, filepath.Base(p))
		}
	}
	for _, name := range sortedSet(wants) {
		c.add(record.Item{Type: record.TypeUsvc, ID: name, User: u.Name})
	}
	entries, _ := os.ReadDir(realDir)
	for _, e := range entries {
		if e.Type()&fs.ModeSymlink != 0 {
			if t, err := os.Readlink(filepath.Join(realDir, e.Name())); err == nil && t == "/dev/null" {
				c.add(record.Item{Type: record.TypeUmask, ID: e.Name(), User: u.Name})
			}
		}
	}

	stage := filepath.Join(c.stage, UserUnitsDir, u.Name)
	if err := os.MkdirAll(stage, 0o700); err != nil {
		return err
	}
	return filepath.WalkDir(realDir, func(full string, d fs.DirEntry, err error) error {
		if err != nil || !d.Type().IsRegular() {
			return nil
		}
		rel, _ := filepath.Rel(realDir, full)
		parent := filepath.Base(filepath.Dir(full))
		if full != realDir && (strings.HasSuffix(parent, ".wants") || strings.HasSuffix(parent, ".requires")) {
			return nil
		}
		fi, err := d.Info()
		if err != nil {
			return nil
		}
		if uid, _ := c.owner(full, fi); uid != u.UID && uid != 0 {
			c.warn("not reading ~%s/%s/%s: it belongs to another user", u.Name, UserUnitDir, rel)
			return nil
		}
		data, err := c.readFile(full)
		if err != nil {
			c.skip(filepath.Join(u.Home, UserUnitDir, rel))
			return nil
		}
		dst := filepath.Join(stage, rel)
		if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
			return err
		}
		if err := os.WriteFile(dst, data, 0o600); err != nil {
			return err
		}
		sum := sha256.Sum256(data)
		c.add(record.Item{Type: record.TypeUunit, ID: filepath.ToSlash(rel), SHA: hex.EncodeToString(sum[:]), User: u.Name})
		return nil
	})
}

var (
	execLine = regexp.MustCompile(`(?m)^\s*Exec[A-Za-z]*=[-@:+!|]*(.+)$`)
)

// unitNeeds records what hand-made units need that reinstalling packages wouldn't bring back:
// packages their Exec lines run that only came in as dependencies (fuse3, libnotify...), and
// folders they use (rclone mount points, log folders). Units a package provides are left out.
// Ported from unit_needs.
func (c *capturer) unitNeeds(ctx context.Context) error {
	type unitText struct {
		unit, text string
		user       *User
	}
	var texts []unitText
	for _, it := range c.items {
		switch {
		case it.Type == record.TypeSvc && it.Pkg == "" && it.UnitFile != "":
			if b, err := c.readFile(c.path(it.UnitFile)); err == nil {
				texts = append(texts, unitText{it.ID, string(b), nil})
			}
		case it.Type == record.TypeUunit:
			u := c.user(it.User)
			if b, err := os.ReadFile(filepath.Join(c.stage, UserUnitsDir, it.User, it.ID)); err == nil && u != nil {
				texts = append(texts, unitText{it.ID, string(b), u})
			}
		}
	}

	type need struct{ path, unit string }
	var progs, dirs []need
	seenProg, seenDir := map[string]bool{}, map[string]bool{}
	addNeed := func(list *[]need, seen map[string]bool, p, unit string) {
		if !seen[p] {
			seen[p] = true
			*list = append(*list, need{p, unit})
		}
	}
	for _, t := range texts {
		home := "/root" // %h in a system unit
		if t.user != nil {
			home = t.user.Home
		}
		for _, m := range execLine.FindAllStringSubmatch(t.text, -1) {
			args := strings.Fields(m[1])
			if len(args) == 0 {
				continue
			}
			addNeed(&progs, seenProg, strings.ReplaceAll(args[0], "%h", home), t.unit)
			if path.Base(args[0]) == "rclone" && len(args) > 3 && args[1] == "mount" {
				addNeed(&dirs, seenDir, strings.ReplaceAll(args[3], "%h", home), t.unit) // rclone mount REMOTE: MOUNTPOINT
			}
			for n, arg := range args {
				if v, ok := strings.CutPrefix(arg, "--log-file="); ok {
					addNeed(&dirs, seenDir, path.Dir(strings.ReplaceAll(v, "%h", home)), t.unit)
				} else if arg == "--log-file" && n+1 < len(args) {
					addNeed(&dirs, seenDir, path.Dir(strings.ReplaceAll(args[n+1], "%h", home)), t.unit)
				}
			}
		}
	}

	have := map[string]bool{}
	for _, it := range c.items {
		if it.Type == record.TypePkg || it.Type == record.TypeAUR {
			have[it.ID] = true
		}
	}
	var abs []string
	for _, p := range progs {
		if strings.HasPrefix(p.path, "/") {
			abs = append(abs, p.path)
		}
	}
	owners, err := c.opt.Packages.Owners(ctx, abs)
	if err != nil {
		return err
	}
	for _, p := range progs {
		pkg := owners[p.path]
		if pkg == "" || have[pkg] {
			continue
		}
		have[pkg] = true
		if repo, ok := c.res.RepoOf[pkg]; ok {
			c.add(record.Item{Type: record.TypePkg, ID: pkg, Repo: repo, AsDeps: true, ForUnit: p.unit})
		} else {
			c.add(record.Item{Type: record.TypeAUR, ID: pkg, AsDeps: true, ForUnit: p.unit})
		}
	}
	for _, d := range dirs {
		if u, rel, ok := c.userOf(d.path); ok {
			c.add(record.Item{Type: record.TypeMkdir, ID: d.path, Rel: rel, ForUnit: d.unit, User: u.Name})
		} else if strings.HasPrefix(d.path, "/") {
			c.add(record.Item{Type: record.TypeMkdir, ID: d.path, ForUnit: d.unit})
		}
	}
	return nil
}

// user returns the captured user with that name.
func (c *capturer) user(name string) *User {
	for i := range c.res.Users {
		if c.res.Users[i].Name == name {
			return &c.res.Users[i]
		}
	}
	return nil
}

// sortedSet returns the distinct strings in s, sorted.
func sortedSet(s []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, x := range s {
		if !seen[x] {
			seen[x] = true
			out = append(out, x)
		}
	}
	sort.Strings(out)
	return out
}
