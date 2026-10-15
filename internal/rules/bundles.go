package rules

import (
	"bufio"
	"fmt"
	"io"
	"strings"

	"github.com/Evimore/carryon/internal/record"
)

// BundleRules are the user's rules from bundles.conf:
//
//	title <bundle> <text>          name shown in the restore checklist
//	when  <bundle> <rule>          when the bundle starts ticked
//	put   <bundle> <type> <glob>   move matching items into a bundle
//	drop  <type> <glob>            never record these
//
// <type> is an item type (pkg, aur, file, path...) or "*". Home paths can be written as ~/...
type BundleRules struct {
	Titles map[string]string // bundle -> title
	Whens  map[string]string // bundle -> when-rule
	Puts   []Put             // in file order: the first matching rule wins
	Drops  []Drop
}

// Put moves matching items into a bundle.
type Put struct {
	Bundle string
	Match  ItemMatch
}

// Drop keeps matching items out of the record.
type Drop struct {
	Match ItemMatch
}

// ItemMatch matches items by type and glob.
type ItemMatch struct {
	Type string // item type, or "*" for any
	Glob Glob
}

// Matches reports whether the item has the right type and its id, or its "~/..." path for home
// items, matches the glob.
func (m ItemMatch) Matches(it record.Item) bool {
	if m.Type != "*" && m.Type != it.Type {
		return false
	}
	if m.Glob.Match(it.ID) {
		return true
	}
	return it.Home != "" && m.Glob.Match("~/"+it.Home)
}

// ParseBundleRules reads bundles.conf. Lines it can't understand are skipped and reported in
// problems, prefixed with name and the line number; so are unknown when-rules.
func ParseBundleRules(r io.Reader, name string) (rules BundleRules, problems []string, err error) {
	rules = BundleRules{Titles: map[string]string{}, Whens: map[string]string{}}
	sc := bufio.NewScanner(r)
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		f := strings.Fields(line)
		switch {
		case f[0] == "title" && len(f) >= 3:
			rules.Titles[f[1]] = restAfter(line, 2)
		case f[0] == "when" && len(f) == 3:
			rules.Whens[f[1]] = f[2]
			if e := Check(f[2]); e != nil {
				problems = append(problems, fmt.Sprintf("%s:%d: %v", name, n, e))
			}
		case f[0] == "put" && len(f) == 4:
			rules.Puts = append(rules.Puts, Put{Bundle: f[1], Match: ItemMatch{Type: f[2], Glob: CompileGlob(f[3])}})
		case f[0] == "drop" && len(f) == 3:
			rules.Drops = append(rules.Drops, Drop{Match: ItemMatch{Type: f[1], Glob: CompileGlob(f[2])}})
		default:
			problems = append(problems, fmt.Sprintf("%s:%d: can't parse: %s", name, n, line))
		}
	}
	return rules, problems, sc.Err()
}

// restAfter returns line without its first n whitespace-separated fields.
func restAfter(line string, n int) string {
	s := line
	for i := 0; i < n; i++ {
		s = strings.TrimLeft(s, " \t")
		if j := strings.IndexAny(s, " \t"); j >= 0 {
			s = s[j:]
		} else {
			return ""
		}
	}
	return strings.TrimSpace(s)
}
