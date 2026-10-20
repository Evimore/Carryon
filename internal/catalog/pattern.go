package catalog

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Exclude patterns use restic's rules (restic's internal/filter and "Excluding files" in its
// documentation), so Carryon and restic agree on what a pattern leaves out. The prototype matched
// with Python's fnmatch and turned "**" into "*", so the two disagreed: "/home/*/.cache" didn't
// match files inside ~/.cache, and "*" crossed "/".
//
// The rules:
//   - A pattern is split into path components; each component matches one component of the path,
//     with filepath.Match syntax ("*", "?", "[a-z]", "[^a-z]", "\" escapes). "*" never crosses "/".
//   - A component "**" matches any number of components, including none.
//   - A leading "/" anchors the pattern at the root. Without it, the pattern matches at any depth.
//   - A pattern matches a path when it matches the path's first components (anchored) or any run
//     of consecutive components (not anchored), so "/bin" matches "/bin/bash" and "foo" matches
//     "/a/foo/b". Once a folder is left out, so is everything in it.
//   - A trailing "/" is ignored: "foo/" also matches a file named foo.
//   - A pattern starting with "!" takes back an earlier pattern's match for that path. It can't
//     bring back anything inside a folder that is already left out, since restic never looks inside.
//   - In exclude files, lines are trimmed, empty lines and lines starting with "#" are skipped, and
//     environment variables are expanded ("$HOME", "${HOME}"); "$$" is a literal "$".

// Pattern is one parsed exclude pattern.
type Pattern struct {
	text    string // as written
	parts   []part
	negated bool
}

type part struct {
	text   string
	any    bool // "**": any number of components
	simple bool // no wildcards: compare as a string
}

// ParsePattern parses a restic exclude pattern. It fails for empty patterns and for patterns
// restic rejects (such as an unclosed "[").
func ParsePattern(s string) (Pattern, error) {
	if s == "" {
		return Pattern{}, errors.New("empty exclude pattern")
	}
	p := Pattern{text: s}
	body := s
	if body[0] == '!' {
		p.negated = true
		body = body[1:]
	}
	for _, c := range splitPath(filepath.Clean(body)) {
		// An empty component only comes from the pattern "/", which restic treats like "/**".
		pt := part{text: c, any: c == "**" || c == "", simple: !strings.ContainsAny(c, `\[]*?`)}
		if !pt.any && !pt.simple {
			if _, err := filepath.Match(c, c); err != nil {
				return Pattern{}, fmt.Errorf("exclude pattern %q: %q isn't a valid wildcard", s, c)
			}
		}
		p.parts = append(p.parts, pt)
	}
	return p, nil
}

// String returns the pattern as written.
func (p Pattern) String() string { return p.text }

// Negated reports whether the pattern starts with "!".
func (p Pattern) Negated() bool { return p.negated }

// Anchored reports whether the pattern starts at the root ("/...").
func (p Pattern) Anchored() bool { return p.parts[0].text == "/" }

// Match reports whether the pattern matches path itself (the "!" is ignored). It doesn't look at the
// folders above path; use Matcher.Excluded for that.
func (p Pattern) Match(path string) bool {
	return p.match(splitPath(filepath.Clean(path)))
}

func (p Pattern) match(comps []string) bool { return matchParts(p.parts, comps) }

var star = part{text: "*"}

// matchParts follows restic's procedure. The first "**" is replaced by none, one, two... "*"
// components, as long as the pattern, still counting each other "**" as one component, is no longer
// than the path. That limit is restic's, and it matters: "/**/a*/**" doesn't match "/a".
func matchParts(parts []part, comps []string) bool {
	for pos, p := range parts {
		if !p.any {
			continue
		}
		rest := parts[pos+1:]
		for n := 0; pos+n+len(rest) <= len(comps); n++ {
			expanded := make([]part, 0, pos+n+len(rest))
			expanded = append(expanded, parts[:pos]...)
			for range n {
				expanded = append(expanded, star)
			}
			expanded = append(expanded, rest...)
			if matchParts(expanded, comps) {
				return true
			}
		}
		return false
	}
	return matchFixed(parts, comps)
}

// matchFixed matches a pattern without "**": anchored ones against the first components, others
// against any run of consecutive components after the root.
func matchFixed(parts []part, comps []string) bool {
	if len(parts) == 0 || len(parts) > len(comps) {
		return false
	}
	first, last := 0, len(comps)-len(parts)
	if parts[0].text == "/" {
		last = 0
	} else if comps[0] == "/" {
		first = 1
	}
next:
	for off := first; off <= last; off++ {
		for i, p := range parts {
			if !matchComponent(p, comps[off+i]) {
				continue next
			}
		}
		return true
	}
	return false
}

func matchComponent(p part, comp string) bool {
	if p.simple {
		return p.text == comp
	}
	ok, _ := filepath.Match(p.text, comp) // patterns are checked in ParsePattern
	return ok
}

// childMayMatch reports whether the pattern could match something inside the folder comps.
func (p Pattern) childMayMatch(comps []string) bool {
	if !p.Anchored() {
		return true
	}
	for i, c := range comps {
		if i == len(p.parts) || p.parts[i].any {
			return true
		}
		if !matchComponent(p.parts[i], c) {
			return false
		}
	}
	return true
}

// splitPath splits a cleaned path into components; an absolute path starts with "/".
func splitPath(p string) []string {
	comps := strings.Split(filepath.ToSlash(p), "/")
	if comps[0] == "" {
		comps[0] = "/"
	}
	return comps
}

// Matcher decides what a list of exclude patterns leaves out, the way restic does.
type Matcher struct {
	pats       []Pattern
	hasNegated bool
}

// NewMatcher parses patterns in order. Empty patterns are skipped, as restic does.
func NewMatcher(patterns []string) (*Matcher, error) {
	m := &Matcher{}
	var errs []error
	for _, s := range patterns {
		if s == "" {
			continue
		}
		p, err := ParsePattern(s)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		m.pats = append(m.pats, p)
		m.hasNegated = m.hasNegated || p.negated
	}
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	return m, nil
}

// Patterns returns the parsed patterns, in order.
func (m *Matcher) Patterns() []Pattern { return m.pats }

// Match reports whether the patterns leave out path itself: the last pattern that matches it
// decides, where a "!" pattern only takes back earlier matches. Folders above path aren't looked at.
func (m *Matcher) Match(path string) bool {
	return m.matchComps(splitPath(filepath.Clean(path)))
}

func (m *Matcher) matchComps(comps []string) bool {
	matched := false
	for _, p := range m.pats {
		if p.negated {
			if matched && p.match(comps) {
				matched = false
			}
		} else if !matched && p.match(comps) {
			matched = true
			if !m.hasNegated {
				break
			}
		}
	}
	return matched
}

// Excluded reports whether a backup of "/" leaves out path: path or one of the folders above it
// is matched. path must be absolute.
func (m *Matcher) Excluded(path string) bool {
	return m.ExcludedFrom("/", path)
}

// ExcludedFrom reports whether a backup of source leaves out path, which is source or inside it.
// Like restic, it never leaves out source itself or the folders above it: only what is inside.
// Both paths must be absolute.
func (m *Matcher) ExcludedFrom(source, path string) bool {
	src := splitPath(filepath.Clean(source))
	comps := splitPath(filepath.Clean(path))
	if src[len(src)-1] == "" { // source "/"
		src = src[:1]
	}
	if len(comps) <= len(src) {
		return false
	}
	for i := len(src) + 1; i <= len(comps); i++ {
		if m.matchComps(comps[:i]) {
			return true
		}
	}
	return false
}

// Verdict says whether a backup leaves out a path, and which patterns decided it.
type Verdict struct {
	Excluded bool
	Pattern  string // the pattern leaving it out
	At       string // what that pattern matched: the path itself or a folder above it
	// TakenBack is a "!" pattern matching the path that took back Over, an earlier pattern. When
	// Excluded is still set, it couldn't help: a folder above the path is left out.
	TakenBack, Over string
}

// Explain is ExcludedFrom, saying which patterns decide.
func (m *Matcher) Explain(source, path string) Verdict {
	src := splitPath(filepath.Clean(source))
	comps := splitPath(filepath.Clean(path))
	if src[len(src)-1] == "" { // source "/"
		src = src[:1]
	}
	var v Verdict
	if len(comps) <= len(src) {
		return v
	}
	for i := len(src) + 1; i <= len(comps); i++ {
		matched, by, back, over := m.decide(comps[:i])
		if back != nil {
			v.TakenBack, v.Over = back.text, over.text
		}
		if matched {
			v.Excluded, v.Pattern, v.At = true, by.text, joinPath(comps[:i])
			break
		}
	}
	if v.Excluded {
		if _, _, back, over := m.decide(comps); back != nil {
			v.TakenBack, v.Over = back.text, over.text
		}
	}
	return v
}

// decide is matchComps, returning the pattern that matched and the last "!" pattern that took
// back an earlier one (over).
func (m *Matcher) decide(comps []string) (matched bool, by, back, over *Pattern) {
	for i := range m.pats {
		p := &m.pats[i]
		if p.negated {
			if matched && p.match(comps) {
				matched, back, over, by = false, p, by, nil
			}
		} else if !matched && p.match(comps) {
			matched, by = true, p
		}
	}
	return matched, by, back, over
}

// joinPath turns components back into a path.
func joinPath(comps []string) string {
	if comps[0] == "/" {
		return "/" + strings.Join(comps[1:], "/")
	}
	return strings.Join(comps, "/")
}

// ParseExcludes reads an exclude file the way restic's --exclude-file does: lines are trimmed,
// empty lines and lines starting with "#" are skipped, and environment variables are expanded with
// getenv ("$$" is a literal "$"). getenv is usually os.Getenv.
func ParseExcludes(data []byte, getenv func(string) string) ([]string, error) {
	data = bytes.TrimPrefix(data, []byte("\xef\xbb\xbf"))
	expand := func(name string) string {
		if name == "$" {
			return "$"
		}
		return getenv(name)
	}
	var pats []string
	sc := bufio.NewScanner(bytes.NewReader(data))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		pats = append(pats, os.Expand(line, expand))
	}
	return pats, sc.Err()
}

// ReadExcludes reads an exclude file with ParseExcludes, expanding variables from the environment.
func ReadExcludes(path string) ([]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading exclude patterns: %w", err)
	}
	pats, err := ParseExcludes(data, os.Getenv)
	if err != nil {
		return nil, fmt.Errorf("reading exclude patterns from %s: %w", path, err)
	}
	return pats, nil
}

// QuotePath turns a path into a pattern that matches exactly that path, by escaping wildcard
// characters. Write the result to an exclude file with "$" doubled (WriteExcludes does that).
func QuotePath(path string) string {
	var b strings.Builder
	for _, r := range path {
		if strings.ContainsRune(`\*?[`, r) {
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	s := b.String()
	// restic trims each line of an exclude file; a final space would be lost.
	if n := len(s); n > 0 && (s[n-1] == ' ' || s[n-1] == '\t') {
		s = s[:n-1] + "[" + s[n-1:] + "]"
	}
	return s
}
