package rules

import (
	"regexp"
	"strings"
)

// Glob is a shell-style pattern with the semantics of Python's fnmatch.fnmatchcase, which the
// prototype's bundle rules used: "*" matches any run of characters including "/", "?" matches one
// character, "[seq]" and "[!seq]" match a character in or not in seq. Matching is case-sensitive.
type Glob struct {
	pattern string
	re      *regexp.Regexp
}

// CompileGlob compiles a pattern. It never fails: a "[" without a closing "]" matches itself.
func CompileGlob(pattern string) Glob {
	return Glob{pattern: pattern, re: regexp.MustCompile(`^(?s:` + translate(pattern) + `)$`)}
}

// Match reports whether name matches the whole pattern.
func (g Glob) Match(name string) bool { return g.re.MatchString(name) }

// String returns the pattern as written.
func (g Glob) String() string { return g.pattern }

// translate turns a glob into a regular expression, following CPython's fnmatch.translate.
func translate(pat string) string {
	var b strings.Builder
	rs := []rune(pat)
	n := len(rs)
	for i := 0; i < n; {
		c := rs[i]
		i++
		switch c {
		case '*':
			for i < n && rs[i] == '*' { // "**" is the same as "*"
				i++
			}
			b.WriteString(".*")
		case '?':
			b.WriteString(".")
		case '[':
			j := i
			if j < n && rs[j] == '!' {
				j++
			}
			if j < n && rs[j] == ']' {
				j++
			}
			for j < n && rs[j] != ']' {
				j++
			}
			if j >= n {
				b.WriteString(`\[`)
				continue
			}
			stuff := strings.ReplaceAll(string(rs[i:j]), `\`, `\\`)
			i = j + 1
			switch {
			case stuff == "":
				b.WriteString(`[^\x00-\x{10FFFF}]`) // an empty set never matches
			case stuff == "!":
				b.WriteString(".")
			default:
				if stuff[0] == '!' {
					stuff = "^" + stuff[1:]
				} else if stuff[0] == '^' || stuff[0] == '[' {
					stuff = `\` + stuff
				}
				b.WriteString("[" + stuff + "]")
			}
		default:
			b.WriteString(regexp.QuoteMeta(string(c)))
		}
	}
	return b.String()
}
