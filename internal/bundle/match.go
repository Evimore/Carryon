package bundle

import (
	"context"
	"path"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/Evimore/carryon/internal/record"
)

var (
	unitSuffix   = regexp.MustCompile(`\.(service|socket|timer|path|mount|automount|target|slice)$`)
	stemSuffix   = regexp.MustCompile(`-(bin|git|appimage)$`)
	appKeySuffix = regexp.MustCompile(`-(bin|git|appimage|launcher)$`)
	notAlnum     = regexp.MustCompile(`[^a-z0-9]`)
	execLine     = regexp.MustCompile(`(?m)^Exec=(.+)$`)
)

// runes counts characters like Python's len.
func runes(s string) int { return utf8.RuneCountInString(s) }

// stemOf is the name an app bundle is known by: a unit or package name without its unit suffix or
// -bin/-git/-appimage, lower case, cut at the first "-" when what's left is at least 4 long
// ("rclone-mount.service" -> "rclone", "old-helper" -> "old-helper").
func stemOf(name string) string {
	base := strings.ToLower(stemSuffix.ReplaceAllString(unitSuffix.ReplaceAllString(name, ""), ""))
	head, _, _ := strings.Cut(base, "-")
	if runes(head) >= 4 {
		return head
	}
	return base
}

// stemMatches reports whether an item belongs to the app bundle named s.
func stemMatches(it record.Item, s string, repoOf map[string]string) bool {
	i := strings.ToLower(it.ID)
	switch it.Type {
	case record.TypePkg, record.TypeAUR:
		return i == s || strings.HasPrefix(i, s+"-") || repoOf[it.ID] == s
	case record.TypeRepo:
		return i == s
	case record.TypeSvc, record.TypeUsvc, record.TypeUunit, record.TypeMask, record.TypeUmask:
		return strings.HasPrefix(i, s)
	case record.TypeFile:
		return strings.Contains(i, s)
	case record.TypePath:
		if it.Home != "" {
			return strings.Contains(strings.ToLower(path.Base(it.Home)), s)
		}
		if it.Extra {
			return strings.Contains(strings.ToLower(path.Base(it.ID)), s)
		}
	}
	return false
}

// appKey is the name settings folders and packages are compared by: lower case, without
// -bin/-git/-appimage/-launcher, letters and digits only ("OpenRGB" and "openrgb-bin" -> "openrgb").
func appKey(name string) string {
	return notAlnum.ReplaceAllString(appKeySuffix.ReplaceAllString(strings.ToLower(name), ""), "")
}

// stem is a file name without its last extension, like Python's PurePath.stem.
func stem(p string) string {
	name := path.Base(p)
	if i := strings.LastIndexByte(name, '.'); i > 0 && i < len(name)-1 {
		return name[:i]
	}
	return name
}

// autostartOwner returns the package owning the program an autostart entry launches, or "".
func autostartOwner(ctx context.Context, m Machine, desktopFile string) string {
	b, err := m.ReadFile(desktopFile)
	if err != nil {
		return ""
	}
	match := execLine.FindStringSubmatch(string(b))
	if match == nil {
		return ""
	}
	args, ok := shellSplit(match[1])
	if !ok {
		return ""
	}
	prog := ""
	for _, a := range args {
		if a != "env" && !strings.Contains(a, "=") {
			prog = a
			break
		}
	}
	if prog == "" {
		return ""
	}
	if !strings.HasPrefix(prog, "/") {
		if prog = m.LookPath(prog); prog == "" {
			return ""
		}
	}
	return m.Owners(ctx, []string{prog})[prog]
}

// shellSplit splits a command line into words like Python's shlex.split: quotes group words and
// backslashes escape outside single quotes. ok is false for an unterminated quote.
func shellSplit(s string) (words []string, ok bool) {
	var cur strings.Builder
	inWord := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			if inWord {
				words = append(words, cur.String())
				cur.Reset()
				inWord = false
			}
		case c == '\\':
			if i+1 >= len(s) {
				return nil, false
			}
			i++
			cur.WriteByte(s[i])
			inWord = true
		case c == '\'':
			j := strings.IndexByte(s[i+1:], '\'')
			if j < 0 {
				return nil, false
			}
			cur.WriteString(s[i+1 : i+1+j])
			i += j + 1
			inWord = true
		case c == '"':
			i++
			for ; i < len(s) && s[i] != '"'; i++ {
				if s[i] == '\\' && i+1 < len(s) && (s[i+1] == '"' || s[i+1] == '\\') {
					i++
				}
				cur.WriteByte(s[i])
			}
			if i >= len(s) {
				return nil, false
			}
			inWord = true
		default:
			cur.WriteByte(c)
			inWord = true
		}
	}
	if inWord {
		words = append(words, cur.String())
	}
	return words, true
}

// capitalize is Python's str.capitalize: the first letter upper case, the rest lower case.
func capitalize(s string) string {
	r, n := utf8.DecodeRuneInString(s)
	if n == 0 {
		return s
	}
	return strings.ToUpper(string(r)) + strings.ToLower(s[n:])
}
