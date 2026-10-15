package catalog

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode"
	"unicode/utf8"
)

// ListInput is what NotBackedUp needs.
type ListInput struct {
	Host     string    // the machine's name
	Date     time.Time // when the list was made
	Home     string    // the home folder to look in
	Excludes []string  // the effective exclude list
	Hints    []Hint    // Catalog.Hints
	Mounts   []string  // mount points: never walked into (a cloud drive would be crawled)
}

const (
	cachesLabel = "Caches and other regenerated data"
	cachesHow   = "apps rebuild them as needed"
)

// steamTools are Steam "games" that are really tools Steam installs when a game needs them.
var steamTools = regexp.MustCompile(`(?i)^(proton|steam linux runtime|steamworks common|steamvr)`)

var versionRe = regexp.MustCompile(`\d+(?:\.\d+){1,3}`)

// NotBackedUp writes not-backed-up.md: what in the home folder is left out of the backup on
// purpose, grouped by the catalog's hints, with sizes and how each part comes back. Excluded paths
// without a hint are listed as caches. Installed Steam games are listed one by one from Steam's own
// manifests. Ported from the prototype's redownload_list.
func NotBackedUp(in ListInput) string {
	mounts := map[string]bool{}
	for _, m := range in.Mounts {
		mounts[filepath.Clean(m)] = true
	}
	home := filepath.Clean(in.Home)
	found := findExcluded(home, in.Excludes, mounts)

	hintOf := map[string]*Hint{}
	var steamHint *Hint
	for i := range in.Hints {
		h := &in.Hints[i]
		for _, p := range h.Patterns {
			if _, ok := hintOf[p]; !ok {
				hintOf[p] = h
			}
		}
		if h.Details == DetailsSteamGames && steamHint == nil {
			steamHint = h
		}
	}

	type group struct {
		label, how, details string
		paths               []string
		bytes               int64
		sized               bool
	}
	var groups []*group
	byLabel := map[string]*group{}
	for _, pat := range in.Excludes {
		paths := found[pat]
		h := hintOf[pat]
		if len(paths) == 0 || (h != nil && (h.Hidden || h.Details == DetailsSteamGames)) {
			continue
		}
		label, how, details := cachesLabel, cachesHow, DetailsNone
		if h != nil {
			label, how, details = h.Label, h.How, h.Details
		}
		g := byLabel[label]
		if g == nil {
			g = &group{label: label, how: how, details: details}
			byLabel[label] = g
			groups = append(groups, g)
		}
		g.paths = append(g.paths, paths...)
	}
	for _, g := range groups {
		var local []string
		for _, p := range g.paths {
			if !mounts[p] {
				local = append(local, p)
			}
		}
		if len(local) > 0 {
			g.bytes, g.sized = DiskUsage(local, in.Mounts), true
		}
	}
	sort.SliceStable(groups, func(i, j int) bool { return groups[i].bytes > groups[j].bytes })

	lines := []string{
		"# Not in the backup: re-download after a restore", "",
		fmt.Sprintf("%s, recorded %s. These were left out on purpose: they re-download themselves or "+
			"already live somewhere else. Your settings for all of them *are* backed up.", in.Host, in.Date.Format("2006-01-02")),
		"",
	}
	if steamHint != nil {
		if games := SteamGames(home); len(games) > 0 {
			var real, tools []Game
			var total int64
			for _, g := range games {
				if steamTools.MatchString(g.Name) {
					tools = append(tools, g)
				} else {
					real = append(real, g)
					total += g.Size
				}
			}
			lines = append(lines, fmt.Sprintf("## %s: %d, %s", steamHint.Label, len(real), Human(total)), "",
				sentence(steamHint.How), "")
			for _, g := range real {
				lines = append(lines, fmt.Sprintf("- %s (%s, appid %s)", g.Name, Human(g.Size), g.AppID))
			}
			if len(tools) > 0 {
				names := make([]string, len(tools))
				for i, g := range tools {
					names[i] = g.Name
				}
				lines = append(lines, "", "Steam installs these itself when a game needs them: "+strings.Join(names, ", "))
			}
			lines = append(lines, "")
		}
	}
	for _, g := range groups {
		extra := ""
		switch g.details {
		case DetailsEntries:
			var names []string
			for _, p := range g.paths {
				names = append(names, readDirNames(p, -1)...)
			}
			extra = " (" + strings.Join(names, ", ") + ")"
		case "", DetailsVersions:
			if v := versions(g.paths); len(v) > 0 {
				extra = " (version " + strings.Join(v, ", ") + ")"
			}
		}
		size := ""
		if g.sized {
			size = ", " + Human(g.bytes)
		}
		lines = append(lines, fmt.Sprintf("- **%s**%s%s: %s.", g.label, extra, size, g.how),
			"  `"+where(g.paths, home)+"`")
	}
	return strings.Join(lines, "\n") + "\n"
}

// findExcluded walks home and returns, for each anchored pattern, the paths it leaves out (sorted).
// It never follows symlinks or walks into a mount point, and it only walks where an anchored pattern
// could match: patterns that match at any depth ("node_modules") would need the whole home walked,
// so what only they leave out isn't listed.
func findExcluded(home string, excludes []string, mounts map[string]bool) map[string][]string {
	m, err := NewMatcher(excludes)
	if err != nil {
		return nil
	}
	var anchored []Pattern
	for _, p := range m.Patterns() {
		if p.Anchored() && !p.Negated() {
			anchored = append(anchored, p)
		}
	}
	found := map[string][]string{}
	if m.Excluded(home) {
		return found
	}
	var walk func(dir string)
	walk = func(dir string) {
		for _, name := range readDirNames(dir, -1) {
			p := filepath.Join(dir, name)
			comps := splitPath(p)
			if m.matchComps(comps) {
				for _, pat := range anchored {
					if pat.match(comps) {
						found[pat.text] = append(found[pat.text], p)
						break
					}
				}
				continue
			}
			if mounts[p] {
				continue
			}
			fi, err := os.Lstat(p)
			if err != nil || !fi.IsDir() {
				continue
			}
			for _, pat := range anchored {
				if pat.childMayMatch(comps) {
					walk(p)
					break
				}
			}
		}
	}
	walk(home)
	return found
}

// readDirNames returns the sorted names in dir, at most n of them (n < 0: all). Errors give none.
func readDirNames(dir string, n int) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var names []string
	for _, e := range entries {
		if n >= 0 && len(names) == n {
			break
		}
		names = append(names, e.Name())
	}
	return names
}

// versions finds version numbers in the folders' names and, for up to three folders, in the
// names of their first six entries. It returns the highest three, lowest first.
func versions(paths []string) []string {
	names := make([]string, 0, len(paths))
	for _, p := range paths {
		names = append(names, filepath.Base(p))
	}
	if len(paths) <= 3 {
		for _, p := range paths {
			if fi, err := os.Lstat(p); err == nil && fi.IsDir() {
				names = append(names, readDirNames(p, 6)...)
			}
		}
	}
	seen := map[string]bool{}
	var vs []string
	for _, v := range versionRe.FindAllString(strings.Join(names, " "), -1) {
		if !seen[v] {
			seen[v] = true
			vs = append(vs, v)
		}
	}
	sort.Slice(vs, func(i, j int) bool { return versionLess(vs[i], vs[j]) })
	if len(vs) > 3 {
		vs = vs[len(vs)-3:]
	}
	return vs
}

// versionLess compares versions number by number, so 1.10 comes after 1.9. The prototype compared
// them as strings.
func versionLess(a, b string) bool {
	as, bs := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < len(as) && i < len(bs); i++ {
		x, _ := strconv.ParseUint(as[i], 10, 64)
		y, _ := strconv.ParseUint(bs[i], 10, 64)
		if x != y {
			return x < y
		}
	}
	if len(as) != len(bs) {
		return len(as) < len(bs)
	}
	return a < b
}

// where shows up to three paths, with the home folder as "~".
func where(paths []string, home string) string {
	shown := paths
	if len(shown) > 3 {
		shown = shown[:3]
	}
	out := make([]string, len(shown))
	for i, p := range shown {
		if rel, err := filepath.Rel(home, p); err == nil && !strings.HasPrefix(rel, "..") {
			p = "~/" + rel
		}
		out[i] = p
	}
	s := strings.Join(out, ", ")
	if len(paths) > 3 {
		s += fmt.Sprintf(" +%d more", len(paths)-3)
	}
	return s
}

func sentence(s string) string {
	r, n := utf8.DecodeRuneInString(s)
	return string(unicode.ToUpper(r)) + s[n:] + "."
}

// Game is an installed Steam game.
type Game struct {
	Name  string
	AppID string
	Size  int64 // bytes on disk, as Steam reports it
}

// SteamGames lists the Steam games installed for the user with this home folder, from the
// appmanifest_*.acf files in ~/.local/share/Steam/steamapps and in the extra libraries listed in
// libraryfolders.vdf. It reads only those small files, so libraries on other drives are fine.
func SteamGames(home string) []Game {
	steam := filepath.Join(home, ".local/share/Steam")
	libs := []string{steam}
	if vdf, err := os.ReadFile(filepath.Join(steam, "steamapps/libraryfolders.vdf")); err == nil {
		for _, kv := range vdfValues(vdf, 2) { // "libraryfolders" { "1" { "path" "..." } }
			if kv[0] == "path" {
				libs = append(libs, kv[1])
			}
		}
	}
	seen := map[string]bool{}
	var games []Game
	for _, lib := range libs {
		key := filepath.Clean(lib)
		if real, err := filepath.EvalSymlinks(key); err == nil {
			key = real
		}
		if seen[key] {
			continue
		}
		seen[key] = true
		apps := filepath.Join(lib, "steamapps")
		for _, name := range readDirNames(apps, -1) {
			if !strings.HasPrefix(name, "appmanifest_") || !strings.HasSuffix(name, ".acf") {
				continue
			}
			acf := filepath.Join(apps, name)
			data, err := os.ReadFile(acf)
			if err != nil {
				continue
			}
			var g Game
			for _, kv := range vdfValues(data, 1) { // "AppState" { "name" "..." }
				switch kv[0] {
				case "name":
					g.Name = kv[1]
				case "appid":
					g.AppID = kv[1]
				case "SizeOnDisk":
					g.Size, _ = strconv.ParseInt(kv[1], 10, 64)
				}
			}
			if g.Name == "" {
				g.Name = strings.TrimSuffix(name, ".acf")
			}
			games = append(games, g)
		}
	}
	sort.SliceStable(games, func(i, j int) bool {
		a, b := strings.ToLower(games[i].Name), strings.ToLower(games[j].Name)
		if a != b {
			return a < b
		}
		return games[i].AppID < games[j].AppID
	})
	return games
}

// vdfValues returns the "key" "value" pairs at one nesting depth of a Valve KeyValues file (.vdf,
// .acf), where depth 1 is inside the outermost braces. The prototype searched the whole text, so a
// nested key could stand in for a missing top-level one.
func vdfValues(data []byte, depth int) [][2]string {
	var out [][2]string
	level := 0
	var pending *string // a key waiting for its value
	for i := 0; i < len(data); i++ {
		switch c := data[i]; c {
		case '{':
			level++
			pending = nil
		case '}':
			level--
			pending = nil
		case '/':
			if i+1 < len(data) && data[i+1] == '/' { // comment to the end of the line
				for i < len(data) && data[i] != '\n' {
					i++
				}
			}
		case '"':
			var b strings.Builder
			for i++; i < len(data) && data[i] != '"'; i++ {
				if data[i] == '\\' && i+1 < len(data) {
					i++
				}
				b.WriteByte(data[i])
			}
			s := b.String()
			switch {
			case pending == nil:
				pending = &s
			case level == depth:
				out = append(out, [2]string{*pending, s})
				pending = nil
			default:
				pending = nil
			}
		}
	}
	return out
}

// DiskUsage adds up the size of the files in paths and everything below them, like "du -sb" but
// counting files only. It doesn't follow symlinks, counts hard-linked files once, skips what it
// can't read and never walks into a mount point.
func DiskUsage(paths, mounts []string) int64 {
	skip := map[string]bool{}
	for _, m := range mounts {
		skip[filepath.Clean(m)] = true
	}
	type inode struct{ dev, ino uint64 }
	counted := map[inode]bool{}
	var total int64
	for _, root := range paths {
		_ = filepath.WalkDir(filepath.Clean(root), func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if d.IsDir() {
				if skip[p] {
					return filepath.SkipDir
				}
				return nil
			}
			if !d.Type().IsRegular() {
				return nil
			}
			fi, err := d.Info()
			if err != nil {
				return nil
			}
			if st, ok := fi.Sys().(*syscall.Stat_t); ok && st.Nlink > 1 {
				k := inode{uint64(st.Dev), uint64(st.Ino)}
				if counted[k] {
					return nil
				}
				counted[k] = true
			}
			total += fi.Size()
			return nil
		})
	}
	return total
}

// Human formats a size the way the prototype did: whole numbers up to MB, one decimal from GB.
func Human(n int64) string {
	f := float64(n)
	for _, unit := range []string{"B", "KB", "MB", "GB", "TB"} {
		if f < 1024 || unit == "TB" {
			if unit == "B" || unit == "KB" || unit == "MB" {
				return fmt.Sprintf("%.0f %s", f, unit)
			}
			return fmt.Sprintf("%.1f %s", f, unit)
		}
		f /= 1024
	}
	return ""
}
