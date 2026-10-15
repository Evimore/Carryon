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
	skip := map[string]bool{}
	for _, s := range in.Skip {
		skip[strings.TrimSpace(s)] = true
	}
	var sections []section
	var all []string
	seen := map[string]bool{}
	addSection := func(title string, pats []string, skippable bool) {
		s := section{title: title}
		for _, p := range pats {
			if p == "" || seen[p] || (skippable && skip[p]) {
				continue
			}
			seen[p] = true
			s.pats = append(s.pats, p)
			all = append(all, p)
		}
		sections = append(sections, s)
	}
	addSection("Defaults (/usr/share/carryon/excludes.default)", in.Defaults, true)
	addSection("From the app catalog (/usr/share/carryon/catalog)", in.Catalog, true)
	var auto []string
	for _, p := range in.Automatic {
		if !filepath.IsAbs(p) {
			return nil, fmt.Errorf("can't leave out %q: the path must be absolute", p)
		}
		auto = append(auto, QuotePath(filepath.Clean(p)))
	}
	addSection("Mount points and the backup's own folder", auto, false)
	addSection("Yours (excludes.txt)", in.User, false)

	var errs []error
	for _, p := range all {
		if strings.ContainsAny(p, "\r\n") {
			errs = append(errs, fmt.Errorf("exclude pattern %q spans several lines", p))
		} else if _, err := ParsePattern(p); err != nil {
			errs = append(errs, err)
		}
	}
	if len(errs) > 0 {
		return nil, fmt.Errorf("checking exclude patterns: %w", errors.Join(errs...))
	}
	if err := writeExcludes(dst, sections); err != nil {
		return nil, err
	}
	return all, nil
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
