package catalog

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"testing/fstest"
)

// The shipped catalog must load, and its entries must say what the roadmap asks of them.
func TestShippedCatalog(t *testing.T) {
	c, err := Load("../../catalog")
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"steam", "discord", "spotify", "claude-desktop", "rust", "npm", "gradle", "go", "dart", "chromium", "kwallet"} {
		if c.App(id) == nil {
			t.Errorf("the catalog has no %q entry", id)
		}
	}
	defaults, err := ReadExcludes("../../catalog/excludes.default")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewMatcher(defaults); err != nil {
		t.Errorf("excludes.default: %v", err)
	}
	inDefaults := map[string]bool{}
	for _, p := range defaults {
		inDefaults[p] = true
	}
	for _, p := range c.Excludes() {
		if inDefaults[p] {
			t.Errorf("%q is both a default and in the catalog", p)
		}
	}

	// Everything the prototype left out is still left out, by the defaults or the catalog.
	legacy, err := ReadExcludes("../../legacy/excludes.txt")
	if err != nil {
		t.Fatal(err)
	}
	m, err := NewMatcher(append(defaults, c.Excludes()...))
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range legacy {
		probe := strings.ReplaceAll(strings.ReplaceAll(p, "**", "x"), "*", "alice")
		if !m.Match(probe) {
			t.Errorf("the prototype's %q (%s) is no longer left out", p, probe)
		}
	}

	steam := c.App("steam")
	if !steam.Recognizes("package", "steam") || !steam.Recognizes("flatpak", "com.valvesoftware.Steam") ||
		!steam.Recognizes("path", ".local/share/Steam/userdata") {
		t.Error("Steam isn't recognized by its package, Flatpak or folder")
	}
	if !c.App("chromium").Recognizes("package", "electron33") {
		t.Error("package wildcards don't work")
	}
	if c.App("go").Recognizes("path", "gopher") {
		t.Error(`"go" shouldn't recognize "gopher"`)
	}
}

func TestKWalletNote(t *testing.T) {
	c, err := Load("../../catalog")
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		restored []string
		want     bool
	}{
		{[]string{".local/share/kwalletd"}, true},
		{[]string{".local/share/kwalletd/kdewallet.kwl"}, true},
		{[]string{".local"}, true}, // a folder containing it
		{[]string{".config/kwalletrc", ".local/share/Steam"}, false},
		{nil, false},
	}
	for _, tc := range cases {
		var got []AppNote
		for _, n := range c.NotesFor(tc.restored) {
			if n.App == "KDE Wallet" {
				got = append(got, n)
			}
		}
		if (len(got) > 0) != tc.want {
			t.Errorf("NotesFor(%q) = %v, want the KDE Wallet note: %v", tc.restored, got, tc.want)
		}
		if len(got) > 0 && !strings.Contains(got[0].Text, "opens with the login password it was created with") {
			t.Errorf("KDE Wallet note = %q", got[0].Text)
		}
	}
}

func TestNoteWithoutPathsUsesRecognizePaths(t *testing.T) {
	c := &Catalog{Apps: []App{{ID: "x", Name: "X", Recognize: Recognize{Paths: []string{".config/x"}},
		Notes: []Note{{Text: "sign in again"}}}}}
	if got := c.NotesFor([]string{".config/x"}); len(got) != 1 || got[0].App != "X" {
		t.Errorf("NotesFor = %v", got)
	}
	if got := c.NotesFor([]string{".config/y"}); len(got) != 0 {
		t.Errorf("NotesFor = %v, want none", got)
	}
}

func TestParseValidation(t *testing.T) {
	const good = `
id = "app"
name = "App"
excludes = ["/home/*/.app/cache"]

[recognize]
packages = ["app"]

[[redownload]]
label = "App files"
how = "the app downloads them again"
patterns = ["/home/*/.app/files"]
`
	if _, err := Parse("app.toml", []byte(good)); err != nil {
		t.Fatalf("a good file fails: %v", err)
	}
	cases := []struct {
		name, file, want string
	}{
		{"bad toml", "id = ", "app.toml"},
		{"unknown key", good + "\ncolour = 1\n", `unknown setting "redownload.colour"`},
		{"unknown top key", "colour = 1\n" + good, `unknown setting "colour"`},
		{"id and file differ", strings.Replace(good, `id = "app"`, `id = "other"`, 1), "must match the file name"},
		{"bad id", strings.Replace(good, `id = "app"`, `id = "My App"`, 1), "lowercase"},
		{"no name", strings.Replace(good, `name = "App"`, "", 1), "name is missing"},
		{"nothing to recognize", strings.Replace(good, `packages = ["app"]`, "", 1), "at least one package"},
		{"absolute recognize path", strings.Replace(good, `packages = ["app"]`, `paths = ["/home/alice/.app"]`, 1), "relative to the home folder"},
		{"tilde path", strings.Replace(good, `packages = ["app"]`, `paths = ["~/.app"]`, 1), "relative to the home folder"},
		{"escaping path", strings.Replace(good, `packages = ["app"]`, `paths = ["../x"]`, 1), "clean path"},
		{"bad wildcard", strings.Replace(good, `packages = ["app"]`, `packages = ["app["]`, 1), "valid name or wildcard"},
		{"unanchored exclude", strings.Replace(good, `"/home/*/.app/cache"`, `"cache"`, 1), `must start with "/"`},
		{"negated exclude", strings.Replace(good, `"/home/*/.app/cache"`, `"!/home/*/.app/cache"`, 1), `can't start with "!"`},
		{"invalid exclude", strings.Replace(good, `"/home/*/.app/cache"`, `"/home/[/x"`, 1), "valid restic pattern"},
		{"variable", strings.Replace(good, `"/home/*/.app/cache"`, `"/home/$USER/x"`, 1), "variables"},
		{"hint without patterns", strings.Replace(good, `patterns = ["/home/*/.app/files"]`, "", 1), "patterns is missing"},
		{"hint without how", strings.Replace(good, `how = "the app downloads them again"`, "", 1), "label and how"},
		{"how with period", strings.Replace(good, `again"`, `again."`, 1), "period"},
		{"bad details", strings.Replace(good, `patterns = ["/home/*/.app/files"]`, `patterns = ["/home/*/.app/files"]`+"\ndetails = \"all\"", 1), "details"},
		{"empty note", good + "\n[[note]]\ntext = \"\"\n", "text is missing"},
	}
	for _, c := range cases {
		_, err := Parse("app.toml", []byte(c.file))
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: error = %v, want it to mention %q", c.name, err, c.want)
		}
	}
	hidden := strings.Replace(good, `label = "App files"`+"\n"+`how = "the app downloads them again"`, "hidden = true", 1)
	if _, err := Parse("app.toml", []byte(hidden)); err != nil {
		t.Errorf("a hidden hint needs no label: %v", err)
	}
}

func TestLoadFS(t *testing.T) {
	app := func(id string) *fstest.MapFile {
		return &fstest.MapFile{Data: []byte("id = \"" + id + "\"\nname = \"N\"\n[recognize]\npackages = [\"p\"]\n")}
	}
	c, err := LoadFS(fstest.MapFS{"b.toml": app("b"), "a.toml": app("a"), "README.md": {Data: []byte("#")}})
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Apps) != 2 || c.Apps[0].ID != "a" || c.Apps[0].File != "a.toml" {
		t.Errorf("apps = %+v, want a then b", c.Apps)
	}

	_, err = LoadFS(fstest.MapFS{"a.toml": app("a"), "b.toml": app("x"), "c.toml": {Data: []byte("id = ")}})
	if err == nil || !strings.Contains(err.Error(), "b.toml") || !strings.Contains(err.Error(), "c.toml") {
		t.Errorf("error = %v, want both bad files named", err)
	}

	_, err = Load(filepath.Join(t.TempDir(), "missing"))
	if !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("missing folder: error = %v, want fs.ErrNotExist", err)
	}
}

func TestCatalogExcludes(t *testing.T) {
	c := &Catalog{Apps: []App{
		{ID: "a", Excludes: []string{"/x", "/y"}, Redownload: []Hint{{Patterns: []string{"/z", "/x"}}}},
		{ID: "b", Excludes: []string{"/y", "/w"}},
	}}
	if got, want := c.Excludes(), []string{"/x", "/y", "/z", "/w"}; !reflect.DeepEqual(got, want) {
		t.Errorf("Excludes = %q, want %q", got, want)
	}
}

func TestEffectiveExcludes(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "excludes")
	got, err := EffectiveExcludes(dst, ExcludeInputs{
		Defaults:  []string{"/home/*/.cache", "/home/*/.local/share/Trash", "/root/.cache"},
		Skip:      []string{"/home/*/.local/share/Trash", " /home/*/.local/share/Steam/steamapps/common "},
		Catalog:   []string{"/home/*/.local/share/Steam/steamapps/common", "/home/*/.cargo/registry", "/root/.cache"},
		Automatic: []string{"/mnt/backup/", "/home/alice/Cloud [Drive]", "/srv/$money"},
		User:      []string{"node_modules", "!/home/bob/.cache", "/srv/$money"},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"/home/*/.cache", "/root/.cache",
		"/home/*/.cargo/registry",
		"/mnt/backup", `/home/alice/Cloud \[Drive]`, "/srv/$money",
		"node_modules", "!/home/bob/.cache",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("EffectiveExcludes:\n got %q\nwant %q", got, want)
	}

	// restic reads the file back as the same list, whatever the environment says.
	data, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	back, err := ParseExcludes(data, func(string) string { return "/surprise" })
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(back, want) {
		t.Errorf("the file reads back as\n %q\nwant %q\nfile:\n%s", back, want, data)
	}
	m, err := NewMatcher(back)
	if err != nil {
		t.Fatal(err)
	}
	for path, excluded := range map[string]bool{
		"/home/alice/Cloud [Drive]/file":                       true,
		"/home/alice/Cloud D":                                  false,
		"/home/bob/.cache/file":                                false, // the user's "!" comes after the default
		"/home/alice/.cache/file":                              true,
		"/home/alice/.local/share/Trash":                       false, // switched off
		"/home/alice/.local/share/Steam/steamapps/common/Game": false, // catalog pattern switched off
	} {
		if got := m.Excluded(path); got != excluded {
			t.Errorf("Excluded(%q) = %v, want %v", path, got, excluded)
		}
	}
	if fi, err := os.Stat(dst); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("exclude file mode = %v, %v", fi.Mode(), err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Errorf("left temporary files behind: %v", entries)
	}
}

func TestExcludeList(t *testing.T) {
	in := ExcludeInputs{
		Defaults:  []string{"/home/*/.cache", "/home/*/.local/share/Trash"},
		Skip:      []string{"/home/*/.local/share/Trash"},
		Catalog:   []string{"/home/*/.cargo/registry", "/home/*/.cache"},
		Automatic: []string{"/home/alice/Cloud [Drive]/"},
		User:      []string{"node_modules", "!/home/bob/.cache"},
		From: map[string]string{
			"/home/*/.cargo/registry":    "the Rust entry of the app catalog (rust.toml)",
			"/home/*/.cache":             "never used: the default comes first",
			"/home/alice/Cloud [Drive]/": "an rclone mount inside a home folder",
			"!/home/bob/.cache":          "bob's own settings",
		},
	}
	want := []Exclude{
		{"/home/*/.cache", FromDefaults, "the default excludes in /usr/share/carryon/excludes.default", false},
		{"/home/*/.cargo/registry", FromCatalog, "the Rust entry of the app catalog (rust.toml)", false},
		{`/home/alice/Cloud \[Drive]`, FromAutomatic, "an rclone mount inside a home folder", false},
		{"node_modules", FromUser, "your excludes in excludes.txt", false},
		{"!/home/bob/.cache", FromUser, "bob's own settings", false},
	}
	if got := in.List(false); !reflect.DeepEqual(got, want) {
		t.Errorf("List(false):\n got %+v\nwant %+v", got, want)
	}
	withSkipped := in.List(true)
	if len(withSkipped) != len(want)+1 || withSkipped[1] != (Exclude{"/home/*/.local/share/Trash", FromDefaults,
		"the default excludes in /usr/share/carryon/excludes.default", true}) {
		t.Errorf("List(true) = %+v", withSkipped)
	}
	got, err := EffectiveExcludes(filepath.Join(t.TempDir(), "excludes"), in)
	if err != nil {
		t.Fatal(err)
	}
	for i, e := range want {
		if got[i] != e.Pattern {
			t.Errorf("EffectiveExcludes = %q, List = %+v", got, want)
			break
		}
	}
}

func TestEffectiveExcludesErrors(t *testing.T) {
	dst := filepath.Join(t.TempDir(), "excludes")
	if _, err := EffectiveExcludes(dst, ExcludeInputs{Automatic: []string{"relative/mount"}}); err == nil {
		t.Error("a relative automatic path should fail")
	}
	_, err := EffectiveExcludes(dst, ExcludeInputs{User: []string{"/ok", "/bad[", "two\nlines"}})
	if err == nil || !strings.Contains(err.Error(), "/bad[") || !strings.Contains(err.Error(), "several lines") {
		t.Errorf("error = %v, want both bad patterns named", err)
	}
	if _, err := os.Stat(dst); !errors.Is(err, fs.ErrNotExist) {
		t.Error("nothing should be written when a pattern is bad")
	}
}
