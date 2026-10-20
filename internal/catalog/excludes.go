package catalog

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ExcludeInputs are the parts of the effective exclude list.
type ExcludeInputs struct {
	Defaults  []string // the package's default patterns (excludes.default)
	Skip      []string // default or catalog patterns the user switched off, as written there
	Catalog   []string // the catalog's patterns (Catalog.Excludes)
	Automatic []string // paths, not patterns: mount points, the repository's own folder
	User      []string // the user's own patterns (excludes.txt)
	// From says where a catalog or user pattern, or an automatic path as given, comes from, for
	// people, when its part's name doesn't say enough: "the Steam entry of the app catalog",
	// "alice's own settings".
	From map[string]string
}

// Where an exclude pattern comes from.
const (
	FromDefaults  = "defaults"  // excludes.default
	FromCatalog   = "catalog"   // the app catalog
	FromAutomatic = "automatic" // a mount point or the repository's own folder
	FromUser      = "user"      // excludes.txt or a user's own settings
)

// Exclude is one pattern of the effective exclude list, with where it comes from.
type Exclude struct {
	Pattern string `json:"pattern"`
	Kind    string `json:"from"`              // one of the From* constants
	From    string `json:"where"`             // for people: the file, app or mount it comes from
	Skipped bool   `json:"skipped,omitempty"` // switched off in backup.skip_default_excludes
}

// List is the effective exclude list in order, with where each pattern comes from. Switched-off
// patterns are left out unless withSkipped is set; then they are listed with Skipped set, so
// people can see why a default doesn't apply. Duplicates are dropped. Patterns aren't checked:
// EffectiveExcludes does that.
func (in ExcludeInputs) List(withSkipped bool) []Exclude {
	skip := map[string]bool{}
	for _, s := range in.Skip {
		skip[strings.TrimSpace(s)] = true
	}
	var out []Exclude
	seen := map[string]bool{}
	add := func(kind, title string, pats []string, skippable bool, from func(string) string) {
		for _, p := range pats {
			if p == "" || seen[p] {
				continue
			}
			skipped := skippable && skip[p]
			if skipped && !withSkipped {
				continue
			}
			seen[p] = true
			where := title
			if f := from(p); f != "" {
				where = f
			}
			out = append(out, Exclude{Pattern: p, Kind: kind, From: where, Skipped: skipped})
		}
	}
	byPattern := func(p string) string { return in.From[p] }
	add(FromDefaults, "the default excludes in /usr/share/carryon/excludes.default", in.Defaults, true, func(string) string { return "" })
	add(FromCatalog, "the app catalog in /usr/share/carryon/catalog", in.Catalog, true, byPattern)
	var auto []string
	autoFrom := map[string]string{}
	for _, p := range in.Automatic {
		q := p
		if filepath.IsAbs(p) {
			q = QuotePath(filepath.Clean(p))
		}
		auto = append(auto, q)
		if _, ok := autoFrom[q]; !ok {
			autoFrom[q] = in.From[p]
		}
	}
	add(FromAutomatic, "a mount point or the backup's own folder", auto, false, func(q string) string { return autoFrom[q] })
	add(FromUser, "your excludes in excludes.txt", in.User, false, byPattern)
	return out
}

// EffectiveExcludes builds the exclude list for a run: the defaults and the catalog's patterns
// minus the switched-off ones, then the automatic paths (matched literally), then the user's
// patterns. The user's come last so a "!" pattern can take back a default. Duplicates are dropped.
//
// It writes the list to dst, an exclude file for restic, and returns it for Carryon's own
// matching (NewMatcher), so restic and Carryon use the same list. Patterns should already have their
// variables expanded (ReadExcludes does that); a "$" left in a pattern is written as "$$" so restic
// reads it back unchanged.
func EffectiveExcludes(dst string, in ExcludeInputs) ([]string, error) {
	for _, p := range in.Automatic {
		if !filepath.IsAbs(p) {
			return nil, fmt.Errorf("can't leave out %q: the path must be absolute", p)
		}
	}
	titles := map[string]string{
		FromDefaults:  "Defaults (/usr/share/carryon/excludes.default)",
		FromCatalog:   "From the app catalog (/usr/share/carryon/catalog)",
		FromAutomatic: "Mount points and the backup's own folder",
		FromUser:      "Yours (excludes.txt)",
	}
	var sections []section
	var all []string
	for _, e := range in.List(false) {
		if len(sections) == 0 || sections[len(sections)-1].title != titles[e.Kind] {
			sections = append(sections, section{title: titles[e.Kind]})
		}
		s := &sections[len(sections)-1]
		s.pats = append(s.pats, e.Pattern)
		all = append(all, e.Pattern)
	}
	if err := CheckPatterns(all); err != nil {
		return nil, err
	}
	if err := writeExcludes(dst, sections); err != nil {
		return nil, err
	}
	return all, nil
}

// CheckPatterns checks that restic can read every pattern.
func CheckPatterns(pats []string) error {
	var errs []error
	for _, p := range pats {
		if strings.ContainsAny(p, "\r\n") {
			errs = append(errs, fmt.Errorf("exclude pattern %q spans several lines", p))
		} else if _, err := ParsePattern(p); err != nil {
			errs = append(errs, err)
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("checking exclude patterns: %w", errors.Join(errs...))
	}
	return nil
}

type section struct {
	title string
	pats  []string
}

func writeExcludes(dst string, sections []section) error {
	var b strings.Builder
	b.WriteString("# Written by Carryon for each backup; changes here are lost. Edit excludes.txt instead.\n")
	for _, s := range sections {
		if len(s.pats) == 0 {
			continue
		}
		fmt.Fprintf(&b, "\n# %s\n", s.title)
		for _, p := range s.pats {
			b.WriteString(strings.ReplaceAll(p, "$", "$$"))
			b.WriteByte('\n')
		}
	}
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".excludes-*")
	if err != nil {
		return fmt.Errorf("writing the exclude list: %w", err)
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.WriteString(b.String()); err != nil {
		tmp.Close()
		return fmt.Errorf("writing the exclude list: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("writing the exclude list: %w", err)
	}
	if err := os.Rename(tmp.Name(), dst); err != nil {
		return fmt.Errorf("writing the exclude list: %w", err)
	}
	return nil
}
