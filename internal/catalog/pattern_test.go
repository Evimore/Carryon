package catalog

import (
	"reflect"
	"strings"
	"testing"
)

// Each row was checked against restic 0.19.1's own filter code.
func TestPatternFollowsRestic(t *testing.T) {
	cases := []struct {
		pat, path string
		want      bool
	}{
		// A leading "/" anchors; without it the pattern matches at any depth.
		{"/bin", "/bin", true},
		{"/bin", "/bin/bash", true}, // a folder's match covers what is inside
		{"/bin", "/usr/bin/restic", false},
		{"bin", "/usr/bin/restic", true},
		{"foo", "/dir1/foo/dir2/file", true},
		{"foo", "/dir/foobar", false}, // whole components only
		{"foo", "/barfoo", false},
		{"a/b", "/x/a/b/c", true}, // a run of components, anywhere
		{"a/b", "/a/x/b", false},

		// "*" stays inside one component; prototype's fnmatch let it cross "/".
		{"/home/*/.cache", "/home/alice/.cache", true},
		{"/home/*/.cache", "/home/alice/.cache/thumbnails/x.png", true}, // fnmatch said no
		{"/home/*/.cache", "/home/alice/sub/.cache", false},
		{"/home/*", "/home", false},
		{"b*ash", "/bin/bash", true},
		{"b*ash", "/bin/ash", false},
		{"*.go", "/src/main.go", true},
		{"*.go", "/src/main.go.bak", false},
		{"*~", "/home/alice/notes.txt~", true},

		// "**" spans any number of components, including none.
		{"foo/**/bar", "/dir1/foo/dir2/bar/file", true},
		{"foo/**/bar", "/foo/bar/file", true},
		{"foo/**/bar", "/tmp/foo/bar", true},
		{"foo/**/bar", "/foo/x/y/z/bar", true},
		{"foo/**/bar", "/foo/barx", false},
		{"/home/*/.config/**/Cache", "/home/alice/.config/Cache", true},
		{"/home/*/.config/**/Cache", "/home/alice/.config/chromium/Default/Cache/data_1", true},
		{"/home/*/.config/**/Code Cache", "/home/alice/.config/Code/Code Cache", true},
		{"/home/*/.config/**/Cache", "/home/alice/.cache/Cache", false},
		{"/**", "/anything/at/all", true},
		{"**", "/a", true},
		{"/**/bar", "/bar", true},
		// restic's quirk: each other "**" still counts as one component when it checks whether the
		// pattern fits the path, so these don't match.
		{"/**/a*/**", "/a", false},
		{"/c/**/**/**", "/c", false},
		{"/c/**/**/**", "/c/d/e", true},

		// A trailing "/" is ignored, so it also matches files.
		{"projects/", "/home/alice/projects", true},
		{"notes.txt/", "/home/alice/notes.txt", true},

		// filepath.Match syntax inside a component.
		{"?ar", "/x/bar", true},
		{"?ar", "/x/ar", false},
		{"[ab]x", "/bx", true},
		{"[^ab]x", "/cx", true},
		{"[^ab]x", "/bx", false},
		{"[!ab]x", "/!x", true}, // "!" isn't negation inside brackets: the set holds "!", "a" and "b"
		{"[!ab]x", "/cx", false},
		{`/a\[1\]`, "/a[1]", true},
		{`/a\*`, "/a*", true},
		{`/a\*`, "/ab", false},
		{"/ABC", "/abc", false}, // case-sensitive

		// "." and ".." are cleaned away, like restic does.
		{"/a/./b/../c", "/a/c", true},
	}
	for _, c := range cases {
		p, err := ParsePattern(c.pat)
		if err != nil {
			t.Errorf("ParsePattern(%q): %v", c.pat, err)
			continue
		}
		if got := p.Match(c.path); got != c.want {
			t.Errorf("%q matches %q = %v, want %v", c.pat, c.path, got, c.want)
		}
	}
}

func TestParsePatternRejectsWhatResticRejects(t *testing.T) {
	for _, pat := range []string{"", "[", "/a/[b", `/a/\`, "foo/[]a]", "[a-]"} {
		if _, err := ParsePattern(pat); err == nil {
			t.Errorf("ParsePattern(%q) succeeded, want an error", pat)
		}
	}
	if _, err := NewMatcher([]string{"/ok", "[", "", "/also-ok", "a/[b"}); err == nil ||
		!strings.Contains(err.Error(), `"["`) || !strings.Contains(err.Error(), `"a/[b"`) {
		t.Errorf("NewMatcher error = %v, want both bad patterns named", err)
	}
}

func TestMatcherNegation(t *testing.T) {
	cases := []struct {
		name string
		pats []string
		path string
		want bool
	}{
		{"later ! takes back", []string{"/docs/*", "!/docs/keep"}, "/docs/keep", false},
		{"others stay out", []string{"/docs/*", "!/docs/keep"}, "/docs/drop", true},
		{"! before has no effect", []string{"!/docs/keep", "/docs/*"}, "/docs/keep", true},
		{"pattern again after !", []string{"/docs/*", "!/docs/keep", "/docs/keep"}, "/docs/keep", true},
		{"! of a file", []string{"*.go", "!main_test.go"}, "/src/main_test.go", false},
		{"! alone excludes nothing", []string{"!/docs"}, "/docs", false},
	}
	for _, c := range cases {
		m, err := NewMatcher(c.pats)
		if err != nil {
			t.Fatal(err)
		}
		if got := m.Match(c.path); got != c.want {
			t.Errorf("%s: Match(%q) = %v, want %v", c.name, c.path, got, c.want)
		}
	}
}

func TestExcludedLooksAtFoldersAbove(t *testing.T) {
	m, err := NewMatcher([]string{"/home/alice/Documents", "!/home/alice/Documents/keep", "tmp"})
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		source, path string
		want         bool
	}{
		// "!" can't bring back what is inside a left-out folder: restic never looks inside.
		{"/", "/home/alice/Documents/keep/file", true},
		{"/", "/home/alice/Documents", true},
		{"/", "/home/alice/notes.txt", false},
		{"/", "/home/alice/tmp/x", true},
		// Restic never leaves out a backup source or the folders above it.
		{"/home/alice/Documents", "/home/alice/Documents", false},
		{"/home/alice/Documents", "/home/alice/Documents/keep", false},
		// ...but a pattern matching the source still matches everything inside it.
		{"/home/alice/Documents", "/home/alice/Documents/other", true},
		{"/tmp/build", "/tmp/build/x", true}, // "tmp" matches the "/tmp" component
		{"/srv/build", "/srv/build/x", false},
		{"/srv/build", "/srv/build/tmp/x", true},
		{"/home/alice", "/srv", false}, // not inside the source
	}
	for _, c := range cases {
		if got := m.ExcludedFrom(c.source, c.path); got != c.want {
			t.Errorf("ExcludedFrom(%q, %q) = %v, want %v", c.source, c.path, got, c.want)
		}
	}
	if !m.Excluded("/home/alice/Documents/keep") {
		t.Error("Excluded should look at the folders above")
	}
}

func TestChildMayMatch(t *testing.T) {
	cases := []struct {
		pat, dir string
		want     bool
	}{
		{"/home/*/.cache", "/home", true},
		{"/home/*/.cache", "/home/alice", true},
		{"/home/*/.cache", "/srv", false},
		{"/home/*/.config/**/Cache", "/home/alice/.config/a/b/c", true},
		{"/home/*/.config/**/Cache", "/home/alice/Documents", false},
		{"node_modules", "/anywhere", true}, // not anchored: could be anywhere below
	}
	for _, c := range cases {
		p, _ := ParsePattern(c.pat)
		if got := p.childMayMatch(splitPath(c.dir)); got != c.want {
			t.Errorf("%q could match inside %q = %v, want %v", c.pat, c.dir, got, c.want)
		}
	}
}

func TestParseExcludes(t *testing.T) {
	env := map[string]string{"HOME": "/home/alice", "USER": "alice"}
	getenv := func(k string) string { return env[k] }
	file := "\xef\xbb\xbf# a comment\n\n  /home/*/.cache  \n\t# indented comment\n$HOME/Downloads\n" +
		"/home/${USER}/tmp\n/srv/$$literal\n/srv/$UNSET/x\n/home/alice/Code Cache\n!$HOME/keep\nfoo # not a comment\n"
	got, err := ParseExcludes([]byte(file), getenv)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"/home/*/.cache",
		"/home/alice/Downloads",
		"/home/alice/tmp",
		"/srv/$literal",
		"/srv//x", // unset variables expand to nothing, as in restic
		"/home/alice/Code Cache",
		"!/home/alice/keep",
		"foo # not a comment", // comments only on their own line
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ParseExcludes:\n got %q\nwant %q", got, want)
	}
}

func TestQuotePath(t *testing.T) {
	cases := []struct{ path, want string }{
		{"/mnt/backup", "/mnt/backup"},
		{"/mnt/a*b?[c]", `/mnt/a\*b\?\[c]`},
		{`/mnt/back\slash`, `/mnt/back\\slash`},
		{"/mnt/trailing ", "/mnt/trailing[ ]"},
		{"/mnt/$HOME", "/mnt/$HOME"}, // "$" is doubled when written to a file, not here
	}
	for _, c := range cases {
		got := QuotePath(c.path)
		if got != c.want {
			t.Errorf("QuotePath(%q) = %q, want %q", c.path, got, c.want)
		}
		p, err := ParsePattern(got)
		if err != nil {
			t.Errorf("QuotePath(%q) isn't a valid pattern: %v", c.path, err)
			continue
		}
		if !p.Match(c.path) || p.Match(c.path+"x") {
			t.Errorf("QuotePath(%q) = %q doesn't match exactly that path", c.path, got)
		}
	}
}
