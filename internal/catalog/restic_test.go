package catalog

import (
	"bufio"
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// The files of the tree the cross-check backs up, relative to its root.
var crossCheckTree = []string{
	"home/alice/.cache/thumbnails/a.png",
	"home/alice/.config/chromium/Default/Cache/data_1",
	"home/alice/.config/chromium/Default/Preferences",
	"home/alice/.config/Code/Code Cache/js/x",
	"home/alice/.config/Cache/top",
	"home/alice/.local/share/Steam/steamapps/common/Made Up Game/game.bin",
	"home/alice/.local/share/Steam/steamapps/appmanifest_100.acf",
	"home/alice/.local/share/Steam/steamapps/compatdata/100/pfx/drive_c/windows/system32/x.dll",
	"home/alice/.local/share/Steam/steamapps/compatdata/100/pfx/drive_c/users/steamuser/save.dat",
	"home/alice/projects/app/node_modules/pkg/index.js",
	"home/alice/projects/app/src/main.go",
	"home/alice/projects/app/src/main_test.go",
	"home/alice/foo/x/y/bar/file",
	"home/alice/foo/bar",
	"home/alice/foobar/file",
	"home/alice/notes.txt",
	"home/alice/notes.txt~",
	"home/alice/a[1]/file",
	"home/alice/$weird/file",
	"home/alice/Documents/keep/file",
	"home/alice/Documents/drop/file",
	"home/alice/Documents/top.txt",
	"home/bob/.cache/x",
	"home/bob/bin/tool",
	"root/.cache/y",
	"srv/data/file",
}

// TestMatcherAgreesWithRestic backs up a small tree with real restic (as a dry run) for each
// pattern list and checks that restic saves exactly what the matcher keeps. Absolute patterns start
// with $ROOT, the tree's root, which also tests variable expansion. Skipped without restic.
func TestMatcherAgreesWithRestic(t *testing.T) {
	restic, err := exec.LookPath("restic")
	if err != nil {
		t.Skip("restic isn't installed")
	}
	if testing.Short() {
		t.Skip("runs restic")
	}
	shipped, err := os.ReadFile("../../catalog/excludes.default")
	if err != nil {
		t.Fatal(err)
	}
	cat, err := Load("../../catalog")
	if err != nil {
		t.Fatal(err)
	}
	var realistic []string
	for _, line := range strings.Split(string(shipped), "\n") {
		if strings.HasPrefix(line, "/") {
			realistic = append(realistic, "$ROOT"+line)
		}
	}
	for _, p := range cat.Excludes() {
		realistic = append(realistic, "$ROOT"+p)
	}

	cases := []struct {
		name string
		pats []string
	}{
		{"anchored star", []string{"$ROOT/home/*/.cache"}},
		{"double star", []string{"$ROOT/home/*/.config/**/Cache", "$ROOT/home/*/.config/**/Code Cache"}},
		{"double star relative", []string{"foo/**/bar"}},
		{"double star anchored", []string{"$ROOT/**/bar"}},
		{"leading double star", []string{"**/.cache"}},
		{"unanchored name", []string{"node_modules"}},
		{"unanchored wildcard", []string{"*.go", "*~"}},
		{"unanchored run", []string{"alice/projects"}},
		{"trailing slash", []string{"projects/", "notes.txt/"}},
		{"negation", []string{"$ROOT/home/alice/Documents/*", "!$ROOT/home/alice/Documents/keep"}},
		{"negation inside excluded folder", []string{"$ROOT/home/alice/Documents", "!$ROOT/home/alice/Documents/keep"}},
		{"negation of files", []string{"*.go", "!main_test.go"}},
		{"negation then exclude again", []string{"$ROOT/home/*", "!$ROOT/home/bob", "$ROOT/home/bob/bin"}},
		{"escaped brackets", []string{`$ROOT/home/alice/a\[1\]`}},
		{"brackets as a set", []string{"$ROOT/home/[ab]*/.cache"}},
		{"literal dollar", []string{"$$weird"}},
		{"proton windows", []string{"$ROOT/home/*/.local/share/Steam/steamapps/compatdata/*/pfx/drive_c/windows"}},
		{"source itself", []string{"$ROOT"}},
		{"double stars counted", []string{"$ROOT/home/**/x/**/**"}},
		{"shipped defaults and catalog", realistic},
	}

	root := t.TempDir()
	for _, f := range crossCheckTree {
		p := filepath.Join(root, f)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	var all []string // every file and folder inside root
	_ = filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err == nil && p != root {
			all = append(all, p)
		}
		return nil
	})

	work := t.TempDir()
	repo := filepath.Join(work, "repo")
	var env []string
	for _, e := range os.Environ() {
		if !strings.HasPrefix(e, "RESTIC_") {
			env = append(env, e)
		}
	}
	env = append(env, "ROOT="+root, "RESTIC_CACHE_DIR="+filepath.Join(work, "cache"))
	run := func(args ...string) ([]byte, error) {
		cmd := exec.Command(restic, append([]string{"-r", repo, "--insecure-no-password", "--no-cache"}, args...)...)
		cmd.Env = env
		return cmd.CombinedOutput()
	}
	if out, err := run("init"); err != nil {
		t.Fatalf("restic init: %v\n%s", err, out)
	}
	getenv := func(k string) string {
		if k == "ROOT" {
			return root
		}
		return os.Getenv(k)
	}

	for i, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel() // dry runs only take a shared lock on the repository
			file := filepath.Join(work, "excludes-"+string(rune('a'+i)))
			if err := os.WriteFile(file, []byte("# cross-check\n"+strings.Join(c.pats, "\n")+"\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			out, err := run("backup", "--dry-run", "-vv", "--exclude-file", file, root)
			if err != nil {
				t.Fatalf("restic backup: %v\n%s", err, out)
			}
			saved := resticSaved(out, root)

			data, _ := os.ReadFile(file)
			pats, err := ParseExcludes(data, getenv)
			if err != nil {
				t.Fatal(err)
			}
			m, err := NewMatcher(pats)
			if err != nil {
				t.Fatal(err)
			}
			var kept []string
			for _, p := range all {
				if !m.ExcludedFrom(root, p) {
					kept = append(kept, p)
				}
			}
			if strings.Join(kept, "\n") != strings.Join(saved, "\n") {
				t.Errorf("patterns %q:\nrestic saves:\n  %s\nthe matcher keeps:\n  %s", c.pats,
					strings.Join(rel(root, saved), "\n  "), strings.Join(rel(root, kept), "\n  "))
			}
		})
	}
}

// resticSaved reads the "new <path>, saved in ..." lines of a verbose dry run and returns the
// paths inside root, sorted like filepath.WalkDir sorts them.
func resticSaved(out []byte, root string) []string {
	var paths []string
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "new ") {
			continue
		}
		p := strings.TrimSpace(strings.TrimPrefix(line, "new "))
		if i := strings.LastIndex(p, ", saved in "); i >= 0 {
			p = p[:i]
		}
		p = strings.TrimSuffix(p, "/")
		if strings.HasPrefix(p, root+"/") {
			paths = append(paths, p)
		}
	}
	sort.Slice(paths, func(i, j int) bool { return walkLess(paths[i], paths[j]) })
	return paths
}

// walkLess orders paths the way filepath.WalkDir visits them: component by component.
func walkLess(a, b string) bool {
	as, bs := strings.Split(a, "/"), strings.Split(b, "/")
	for i := 0; i < len(as) && i < len(bs); i++ {
		if as[i] != bs[i] {
			return as[i] < bs[i]
		}
	}
	return len(as) < len(bs)
}

func rel(root string, paths []string) []string {
	out := make([]string, len(paths))
	for i, p := range paths {
		out[i] = strings.TrimPrefix(p, root+"/")
	}
	return out
}
