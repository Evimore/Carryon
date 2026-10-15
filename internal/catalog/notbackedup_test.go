package catalog

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// writeTree creates files with the given sizes under root.
func writeTree(t *testing.T, root string, files map[string]int) {
	t.Helper()
	for name, size := range files {
		p := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(strings.Repeat("x", size)), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func acf(appid, name, size string) string {
	s := "\"AppState\"\n{\n\t\"appid\"\t\t\"" + appid + "\"\n"
	if name != "" {
		s += "\t\"name\"\t\t\"" + name + "\"\n"
	}
	return s + "\t\"SizeOnDisk\"\t\t\"" + size + "\"\n\t\"InstalledDepots\"\n\t{\n\t\t\"name\"\t\t\"not this one\"\n\t}\n}\n"
}

// rooted moves the shipped catalog's patterns under root, so they match the fake home.
func rooted(t *testing.T, root string) ([]string, []Hint) {
	t.Helper()
	c, err := Load("../../catalog")
	if err != nil {
		t.Fatal(err)
	}
	defaults, err := ReadExcludes("../../catalog/excludes.default")
	if err != nil {
		t.Fatal(err)
	}
	prefix := QuotePath(root)
	var excludes []string
	for _, p := range append(defaults, c.Excludes()...) {
		excludes = append(excludes, prefix+p)
	}
	var hints []Hint
	for _, h := range c.Hints() {
		var pats []string
		for _, p := range h.Patterns {
			pats = append(pats, prefix+p)
		}
		h.Patterns = pats
		hints = append(hints, h)
	}
	return excludes, hints
}

func TestNotBackedUp(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "home/alice")
	steam := filepath.Join(home, ".local/share/Steam")
	library := filepath.Join(root, "games/SteamLibrary")
	writeTree(t, home, map[string]int{
		".cache/thumbnails/a.png":                                   1000,
		".cache/cloud/huge":                                         5000, // a mount point: never counted
		".config/remote/Cache/x":                                    10,   // inside a mount point: never found
		".config/chromium/Default/Cache/data":                       2048,
		".config/chromium/Default/Preferences":                      10,
		".local/share/Steam/steamapps/common/Made Up Game/game.bin": 3000,
		".local/share/Steam/steamapps/shadercache/100/x":            500,
		".rustup/toolchains/stable-x86_64-unknown-linux-gnu/f":      100,
		".rustup/toolchains/nightly-x86_64-unknown-linux-gnu/f":     0,
		".rustup/downloads/partial":                                 900, // hidden
		".cargo/registry/index/x":                                   100,
		".config/discord/app-0.0.309/f":                             700,
		".config/discord/settings.json":                             10,
		".npm/_cacache/index":                                       50, // a mount point: listed without a size
		"Documents/notes.txt":                                       10,
	})
	writeTree(t, root, map[string]int{"games/SteamLibrary/steamapps/common/Another/x": 10})
	files := map[string]string{
		filepath.Join(steam, "steamapps/appmanifest_100.acf"):   acf("100", "Made Up Game", "1610612736"),
		filepath.Join(steam, "steamapps/appmanifest_200.acf"):   acf("200", "Proton 9.0", "1073741824"),
		filepath.Join(steam, "steamapps/appmanifest_300.acf"):   acf("300", "", "0"),
		filepath.Join(library, "steamapps/appmanifest_400.acf"): acf("400", "another imaginary game", "52428800"),
		filepath.Join(steam, "steamapps/libraryfolders.vdf"): "\"libraryfolders\"\n{\n\t\"0\"\n\t{\n\t\t\"path\"\t\t\"" + steam +
			"\"\n\t}\n\t\"1\"\n\t{\n\t\t\"path\"\t\t\"" + library + "\"\n\t}\n}\n",
	}
	for p, s := range files {
		if err := os.WriteFile(p, []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	excludes, hints := rooted(t, root)

	got := NotBackedUp(ListInput{
		Host: "desktop", Date: time.Date(2026, 10, 15, 3, 0, 0, 0, time.UTC), Home: home,
		Excludes: excludes, Hints: hints,
		Mounts: []string{filepath.Join(home, ".cache/cloud"), filepath.Join(home, ".config/remote"), filepath.Join(home, ".npm/_cacache")},
	})
	want := `# Not in the backup: re-download after a restore

desktop, recorded 2026-10-15. These were left out on purpose: they re-download themselves or already live somewhere else. Your settings for all of them *are* backed up.

## Steam games: 3, 1.5 GB

Reinstall them from your Steam library; saves in Proton prefixes and Steam Cloud come back on their own.

- another imaginary game (50 MB, appid 400)
- appmanifest_300 (0 B, appid 300)
- Made Up Game (1.5 GB, appid 100)

Steam installs these itself when a game needs them: Proton 9.0

- **Caches and other regenerated data**, 3 KB: apps rebuild them as needed.
  ` + "`~/.cache, ~/.config/chromium/Default/Cache`" + `
- **Discord app files** (version 0.0.309), 700 B: the ` + "`discord`" + ` package downloads them on first start.
  ` + "`~/.config/discord/app-0.0.309`" + `
- **Steam shader caches**, 500 B: rebuilt by Steam and Proton while you play.
  ` + "`~/.local/share/Steam/steamapps/shadercache`" + `
- **Rust toolchains** (nightly-x86_64-unknown-linux-gnu, stable-x86_64-unknown-linux-gnu), 100 B: ` + "`rustup toolchain install <name>`" + ` (cargo does it on first use).
  ` + "`~/.rustup/toolchains`" + `
- **Cargo crate cache**, 100 B: cargo downloads crates again when you build.
  ` + "`~/.cargo/registry`" + `
- **npm cache**: npm refills it.
  ` + "`~/.npm/_cacache`" + `
`
	if got != want {
		t.Errorf("NotBackedUp:\n%s\nwant:\n%s", got, want)
	}
}

func TestNotBackedUpWithoutSteam(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "home/alice")
	writeTree(t, home, map[string]int{"go/pkg/mod/example.com/lib@v1.2.3/x.go": 10, "go/src/mine/main.go": 10})
	excludes, hints := rooted(t, root)
	got := NotBackedUp(ListInput{Host: "desktop", Home: home, Excludes: excludes, Hints: hints})
	if strings.Contains(got, "Steam") {
		t.Errorf("no Steam, but the list mentions it:\n%s", got)
	}
	if !strings.Contains(got, "- **Go module cache**, 10 B: go refills it.\n  `~/go/pkg/mod`\n") {
		t.Errorf("the Go module cache is missing:\n%s", got)
	}
}

func TestFindExcludedListsOnlyAnchoredPatterns(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "home/alice")
	writeTree(t, home, map[string]int{"code/app/node_modules/x": 1, ".cache/x": 1, ".cache2/x": 1, "keep/.cache/y": 1})
	prefix := QuotePath(root)
	found := findExcluded(home, []string{"node_modules", prefix + "/home/*/.cache", prefix + "/home/*/.cache*", "!" + prefix + "/home/alice/.cache2"}, nil)
	want := map[string][]string{prefix + "/home/*/.cache": {filepath.Join(home, ".cache")}}
	if !reflect.DeepEqual(found, want) {
		t.Errorf("found %q, want %q", found, want)
	}
}

func TestDiskUsage(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]int{"a/one": 100, "a/sub/two": 200, "a/mnt/big": 5000, "outside/big": 7000})
	if err := os.Link(filepath.Join(root, "a/one"), filepath.Join(root, "a/sub/one-again")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "outside"), filepath.Join(root, "a/link")); err != nil {
		t.Fatal(err)
	}
	mounts := []string{filepath.Join(root, "a/mnt")}
	if got := DiskUsage([]string{filepath.Join(root, "a")}, mounts); got != 300 {
		t.Errorf("DiskUsage = %d, want 300 (hard link once, no symlinks, no mounts)", got)
	}
	if got := DiskUsage([]string{filepath.Join(root, "a/mnt")}, mounts); got != 0 {
		t.Errorf("DiskUsage of a mount point = %d, want 0", got)
	}
	if got := DiskUsage([]string{filepath.Join(root, "a/one"), filepath.Join(root, "missing")}, nil); got != 100 {
		t.Errorf("DiskUsage of a file = %d, want 100", got)
	}
}

func TestSteamGamesSkipsDuplicateLibraries(t *testing.T) {
	home := t.TempDir()
	steam := filepath.Join(home, ".local/share/Steam")
	writeTree(t, steam, map[string]int{"steamapps/appmanifest_10.acf": 0})
	if err := os.WriteFile(filepath.Join(steam, "steamapps/appmanifest_10.acf"), []byte(acf("10", "Imaginary", "1024")), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(steam, filepath.Join(home, ".steam-link")); err != nil {
		t.Fatal(err)
	}
	vdf := `"libraryfolders" { "0" { "path" "` + filepath.Join(home, ".steam-link") + `" } "1" { "path" "/nonexistent/library" } }`
	if err := os.WriteFile(filepath.Join(steam, "steamapps/libraryfolders.vdf"), []byte(vdf), 0o644); err != nil {
		t.Fatal(err)
	}
	want := []Game{{Name: "Imaginary", AppID: "10", Size: 1024}}
	if got := SteamGames(home); !reflect.DeepEqual(got, want) {
		t.Errorf("SteamGames = %+v, want %+v", got, want)
	}
	if got := SteamGames(filepath.Join(home, "nobody")); got != nil {
		t.Errorf("SteamGames without Steam = %+v", got)
	}
}

func TestHuman(t *testing.T) {
	cases := []struct {
		n    int64
		want string
	}{
		{0, "0 B"},
		{1023, "1023 B"},
		{1024, "1 KB"},
		{1536, "2 KB"}, // halves round to even, like Python's format
		{2560, "2 KB"},
		{3 << 20, "3 MB"},
		{1 << 30, "1.0 GB"},
		{1342177280, "1.2 GB"}, // 1.25 GB
		{5 << 40, "5.0 TB"},
		{2000 << 40, "2000.0 TB"},
	}
	for _, c := range cases {
		if got := Human(c.n); got != c.want {
			t.Errorf("Human(%d) = %q, want %q", c.n, got, c.want)
		}
	}
}

func TestVersions(t *testing.T) {
	dir := t.TempDir()
	writeTree(t, dir, map[string]int{
		"app-1.9.0/x": 0, "app-1.10.2/x": 0, "app-1.2/x": 0, "app-0.9/x": 0,
		"one/v2.0.1-beta": 0, "one/readme": 0,
	})
	p := func(names ...string) []string {
		var out []string
		for _, n := range names {
			out = append(out, filepath.Join(dir, n))
		}
		return out
	}
	cases := []struct {
		paths []string
		want  []string
	}{
		{p("app-1.9.0", "app-1.10.2", "app-1.2", "app-0.9"), []string{"1.2", "1.9.0", "1.10.2"}}, // by number, not as text
		{p("one"), []string{"2.0.1"}}, // from the folder's entries
		{p("nothing-here"), nil},
	}
	for _, c := range cases {
		if got := versions(c.paths); !reflect.DeepEqual(got, c.want) {
			t.Errorf("versions(%q) = %q, want %q", c.paths, got, c.want)
		}
	}
}

func TestVDFValues(t *testing.T) {
	data := []byte(`// a comment "name" "not this"
"AppState"
{
	"appid"		"10"
	"name"		"A \"quoted\" game"
	"InstalledDepots"
	{
		"11" { "manifest" "123" }
		"name" "nested"
	}
	"installdir"		"C:\\games"
}`)
	want := [][2]string{{"appid", "10"}, {"name", `A "quoted" game`}, {"installdir", `C:\games`}}
	if got := vdfValues(data, 1); !reflect.DeepEqual(got, want) {
		t.Errorf("vdfValues = %q, want %q", got, want)
	}
}
