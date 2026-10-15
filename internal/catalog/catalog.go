// Package catalog is what Carryon knows about apps: what to leave out of the backup, how the
// left-out parts come back, and what to tell the user after a restore. See docs/design.md,
// "Configuration", and catalog/README.md for the file format.
//
// It also holds the exclude matcher (restic's pattern rules), the effective exclude list and the
// not-backed-up list.
package catalog

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"regexp"
	"sort"
	"strings"

	"github.com/BurntSushi/toml"
)

// App is one catalog entry: one file in catalog/.
type App struct {
	ID         string    `toml:"id"`   // the file name without ".toml": steam, rust...
	Name       string    `toml:"name"` // what users call it: "Steam"
	Recognize  Recognize `toml:"recognize"`
	Excludes   []string  `toml:"excludes"`   // left out, listed as caches in not-backed-up.md
	Redownload []Hint    `toml:"redownload"` // left out, with how each part comes back
	Notes      []Note    `toml:"note"`       // shown after a restore

	File string `toml:"-"` // where it was loaded from
}

// Recognize says how to tell that an app is installed or has settings. Entries may use the "*",
// "?" and "[...]" wildcards of path.Match.
type Recognize struct {
	Packages []string `toml:"packages"` // package names
	Flatpaks []string `toml:"flatpaks"` // Flatpak app ids
	Paths    []string `toml:"paths"`    // settings folders, relative to the home folder
}

// Hint describes excluded data that comes back by itself.
type Hint struct {
	Patterns []string `toml:"patterns"` // exclude patterns (restic syntax) it covers
	Label    string   `toml:"label"`    // what it is: "Rust toolchains"
	How      string   `toml:"how"`      // how it comes back, in a sentence without the final period
	Details  string   `toml:"details"`  // what else to show in not-backed-up.md (see Details*)
	Hidden   bool     `toml:"hidden"`   // left out without being listed (not worth mentioning)
}

// What a hint shows besides its label, size and folders.
const (
	DetailsVersions   = "versions"    // version numbers found in the folder names (the default)
	DetailsEntries    = "entries"     // the names of the entries in the folders (Rust toolchains)
	DetailsSteamGames = "steam-games" // each installed game, from Steam's manifests
	DetailsNone       = "none"        // nothing
)

// Note is a message shown after a restore.
type Note struct {
	Text  string   `toml:"text"`
	Paths []string `toml:"paths"` // shown when one of these (relative to home) is restored; empty: any of the app's recognize.paths
}

// Catalog is a set of apps, sorted by id.
type Catalog struct {
	Apps []App
}

var idRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

// Load reads every *.toml file in dir. A file with a problem fails the whole load, and the error
// lists every problem found. If dir doesn't exist, the error wraps fs.ErrNotExist.
func Load(dir string) (*Catalog, error) {
	c, err := LoadFS(os.DirFS(dir))
	if err != nil {
		return nil, fmt.Errorf("reading the catalog in %s: %w", dir, err)
	}
	return c, nil
}

// LoadFS reads every *.toml file at the top of fsys.
func LoadFS(fsys fs.FS) (*Catalog, error) {
	names, err := fs.Glob(fsys, "*.toml")
	if err != nil {
		return nil, err
	}
	if _, err := fs.Stat(fsys, "."); err != nil {
		return nil, err
	}
	sort.Strings(names)
	c := &Catalog{}
	var errs []error
	seen := map[string]string{}
	for _, name := range names {
		data, err := fs.ReadFile(fsys, name)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		a, err := Parse(name, data)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if other, ok := seen[a.ID]; ok {
			errs = append(errs, fmt.Errorf("%s: id %q is already used by %s", name, a.ID, other))
			continue
		}
		seen[a.ID] = name
		c.Apps = append(c.Apps, *a)
	}
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	return c, nil
}

// Parse reads and checks one catalog file. name is the file name, used for the id check and in
// error messages.
func Parse(name string, data []byte) (*App, error) {
	var a App
	md, err := toml.Decode(string(data), &a)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	a.File = name
	var problems []string
	for _, k := range md.Undecoded() {
		problems = append(problems, fmt.Sprintf("unknown setting %q", k.String()))
	}
	problems = append(problems, a.check(strings.TrimSuffix(path.Base(name), ".toml"))...)
	if len(problems) > 0 {
		return nil, fmt.Errorf("%s: %s", name, strings.Join(problems, "; "))
	}
	return &a, nil
}

func (a *App) check(stem string) []string {
	var p []string
	add := func(format string, args ...any) { p = append(p, fmt.Sprintf(format, args...)) }
	switch {
	case !idRe.MatchString(a.ID):
		add("id %q must be lowercase letters, digits and dashes", a.ID)
	case a.ID != stem:
		add("id %q must match the file name (%s.toml)", a.ID, a.ID)
	}
	if strings.TrimSpace(a.Name) == "" {
		add("name is missing")
	}
	r := a.Recognize
	if len(r.Packages)+len(r.Flatpaks)+len(r.Paths) == 0 {
		add("recognize needs at least one package, Flatpak id or path")
	}
	for _, list := range [][]string{r.Packages, r.Flatpaks, r.Paths} {
		for _, s := range list {
			if _, err := path.Match(s, ""); err != nil || s == "" {
				add("recognize: %q isn't a valid name or wildcard", s)
			}
		}
	}
	for _, s := range r.Paths {
		if msg := checkHomePath(s); msg != "" {
			add("recognize.paths: %q %s", s, msg)
		}
	}
	for _, s := range a.Excludes {
		if msg := checkPattern(s); msg != "" {
			add("excludes: %q %s", s, msg)
		}
	}
	for i, h := range a.Redownload {
		where := fmt.Sprintf("redownload #%d", i+1)
		if len(h.Patterns) == 0 {
			add("%s: patterns is missing", where)
		}
		for _, s := range h.Patterns {
			if msg := checkPattern(s); msg != "" {
				add("%s: %q %s", where, s, msg)
			}
		}
		if !h.Hidden && (strings.TrimSpace(h.Label) == "" || strings.TrimSpace(h.How) == "") {
			add("%s: label and how are needed unless hidden = true", where)
		}
		switch h.Details {
		case "", DetailsVersions, DetailsEntries, DetailsSteamGames, DetailsNone:
		default:
			add("%s: details %q must be one of versions, entries, steam-games, none", where, h.Details)
		}
		if strings.HasSuffix(strings.TrimSpace(h.How), ".") {
			add("%s: how shouldn't end with a period (one is added)", where)
		}
	}
	for i, n := range a.Notes {
		if strings.TrimSpace(n.Text) == "" {
			add("note #%d: text is missing", i+1)
		}
		for _, s := range n.Paths {
			if msg := checkHomePath(s); msg != "" {
				add("note #%d: %q %s", i+1, s, msg)
			}
		}
	}
	return p
}

// checkPattern checks a catalog exclude: valid for restic, anchored (so it can't match by accident
// somewhere else), not negated, and free of variables (catalog files are the same for everyone).
func checkPattern(s string) string {
	pat, err := ParsePattern(s)
	switch {
	case err != nil:
		return "isn't a valid restic pattern"
	case pat.Negated():
		return "can't start with \"!\""
	case !pat.Anchored():
		return "must start with \"/\""
	case strings.Contains(s, "$"):
		return "can't use variables"
	}
	return ""
}

func checkHomePath(s string) string {
	switch {
	case s == "" || strings.HasPrefix(s, "/") || strings.HasPrefix(s, "~"):
		return "must be relative to the home folder, like \".config/app\""
	case path.Clean(s) != s || s == "." || strings.HasPrefix(s, "../") || s == "..":
		return "must be a clean path inside the home folder"
	}
	return ""
}

// App returns the app with this id, or nil.
func (c *Catalog) App(id string) *App {
	for i := range c.Apps {
		if c.Apps[i].ID == id {
			return &c.Apps[i]
		}
	}
	return nil
}

// Excludes returns every app's exclude patterns, including those of its hints, without duplicates.
func (c *Catalog) Excludes() []string {
	var out []string
	seen := map[string]bool{}
	for _, a := range c.Apps {
		for _, s := range a.allExcludes() {
			if !seen[s] {
				seen[s] = true
				out = append(out, s)
			}
		}
	}
	return out
}

func (a *App) allExcludes() []string {
	out := append([]string(nil), a.Excludes...)
	for _, h := range a.Redownload {
		out = append(out, h.Patterns...)
	}
	return out
}

// Hints returns every app's hints, in catalog order.
func (c *Catalog) Hints() []Hint {
	var out []Hint
	for _, a := range c.Apps {
		out = append(out, a.Redownload...)
	}
	return out
}

// Recognizes reports whether a package, Flatpak id or home path (relative to the home folder)
// belongs to the app. kind is "package", "flatpak" or "path". A path inside a recognized folder
// counts too.
func (a *App) Recognizes(kind, name string) bool {
	var list []string
	switch kind {
	case "package":
		list = a.Recognize.Packages
	case "flatpak":
		list = a.Recognize.Flatpaks
	case "path":
		list = a.Recognize.Paths
	}
	for _, pat := range list {
		if ok, _ := path.Match(pat, name); ok {
			return true
		}
		if kind == "path" && withinWildcard(pat, name) {
			return true
		}
	}
	return false
}

// withinWildcard reports whether name is inside a folder matched by pat.
func withinWildcard(pat, name string) bool {
	n := strings.Count(pat, "/") + 1
	comps := strings.Split(name, "/")
	if len(comps) <= n {
		return false
	}
	ok, _ := path.Match(pat, strings.Join(comps[:n], "/"))
	return ok
}

// AppNote is a post-restore note with the app it belongs to.
type AppNote struct {
	App  string // the app's name
	Text string
}

// NotesFor returns the notes for a restore of these home paths (relative to the home folder, like
// ".local/share/kwalletd"). A note applies when a restored path is one of the note's paths (or of
// the app's recognize.paths when the note has none), inside one, or a folder containing one.
func (c *Catalog) NotesFor(restored []string) []AppNote {
	var out []AppNote
	for _, a := range c.Apps {
		for _, n := range a.Notes {
			paths := n.Paths
			if len(paths) == 0 {
				paths = a.Recognize.Paths
			}
			if overlaps(paths, restored) {
				out = append(out, AppNote{App: a.Name, Text: n.Text})
			}
		}
	}
	return out
}

func overlaps(pats, restored []string) bool {
	for _, r := range restored {
		r = path.Clean(r)
		for _, p := range pats {
			if ok, _ := path.Match(p, r); ok || withinWildcard(p, r) || containsWildcard(r, p) {
				return true
			}
		}
	}
	return false
}

// containsWildcard reports whether the folder dir contains something pat matches.
func containsWildcard(dir, pat string) bool {
	d := strings.Split(dir, "/")
	pc := strings.Split(pat, "/")
	if len(d) >= len(pc) {
		return false
	}
	ok, _ := path.Match(strings.Join(pc[:len(d)], "/"), dir)
	return ok
}
