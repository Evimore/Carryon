// Package why explains whether and how a path is backed up: the backed-up folder holding it, the
// exclude pattern or filesystem leaving it out, the record item holding it with its bundle, the
// rule that put it there and whether the bundle starts ticked here, and what comes back by itself.
// It answers carryon why. See docs/design.md, "Interfaces".
//
// Explain only puts together what it's given; the command reads the machine.
package why

import (
	"context"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/Evimore/carryon/internal/backup"
	"github.com/Evimore/carryon/internal/bundle"
	"github.com/Evimore/carryon/internal/catalog"
	"github.com/Evimore/carryon/internal/record"
	"github.com/Evimore/carryon/internal/rules"
	"github.com/Evimore/carryon/internal/state"
)

// Input is what Explain needs to know.
type Input struct {
	// Path is the path to explain: absolute and clean.
	Path string
	// Root is where the system's files are read from: "/" (or empty) for the real system.
	Root string
	// Targets are the folders a backup covers (backup.Targets).
	Targets []backup.Target
	// Excludes is the effective exclude list, with the switched-off patterns marked
	// (catalog.ExcludeInputs.List(true)).
	Excludes []catalog.Exclude
	// Hints are the catalog's notes on excluded data that comes back by itself.
	Hints []catalog.Hint
	// Crossing says how a backup of a target treats the filesystems on the way to a path inside
	// it (backup.CrossingTo). Nil means it crosses them all.
	Crossing func(target, path string) backup.Crossing
	// Record is the system record, or nil when it couldn't be read (without root).
	Record *record.Record
	// RecordError says why the record couldn't be read as root; empty without root.
	RecordError string
	// RecordDir is the record's folder, for grouping again. Empty skips the checks that read it.
	RecordDir string
	// Rules are bundles.conf's rules, to group the record's items again.
	Rules rules.BundleRules
	// Machine answers grouping's questions about this machine. Nil knows nothing.
	Machine bundle.Machine
	// Here are this machine's facts, to tell whether a bundle starts ticked. Nil skips that.
	Here *rules.Facts
	// Owner is the package owning Path, or "" when none does or it isn't known.
	Owner string
	// OtherUsers are the accounts with a home that backup.users leaves out, by name.
	OtherUsers map[string]string
	// Ignored are the folders never suggested for the backup (detect.ignore).
	Ignored []string
	// Inbox are the suggestions waiting in carryon inbox.
	Inbox []state.Entry
}

// Answer is what Explain found. Its JSON is carryon why --json's output.
type Answer struct {
	Path     string `json:"path"`
	Exists   bool   `json:"exists"`
	BackedUp bool   `json:"backed_up"` // the file itself is in the backup's snapshots
	// Folder is the backed-up folder holding the path.
	Folder *backup.Target `json:"folder,omitempty"`
	// Excluded is the exclude pattern leaving the path out.
	Excluded *Excluded `json:"excluded,omitempty"`
	// TakenBack is a "!" pattern taking back an exclude for the path.
	TakenBack *TakenBack `json:"taken_back,omitempty"`
	// CacheDir is a folder above the path marked as a cache with a CACHEDIR.TAG file.
	CacheDir string `json:"cache_dir,omitempty"`
	// LeftOut is another filesystem holding the path, which the backup doesn't cross into.
	LeftOut *backup.LeftOut `json:"left_out,omitempty"`
	// Subvolume is the nested btrfs subvolume holding the path, backed up as a folder of its own.
	Subvolume string `json:"subvolume,omitempty"`
	// Comes back by itself after a restore, when it's left out.
	Redownload *Redownload `json:"redownload,omitempty"`
	// Items are the record items holding the path.
	Items []Item `json:"items"`
	// Package is the package owning the path, when the record keeps no changes of it: it comes
	// back with the package.
	Package *Package `json:"package,omitempty"`
	// OtherUser is whose home the path is in, when backup.users leaves that user out.
	OtherUser string `json:"other_user,omitempty"`
	// Ignored is the detect.ignore folder holding the path: Carryon never suggests backing it up.
	Ignored string `json:"ignored,omitempty"`
	// Waiting is the inbox suggestion to back up the path or a folder holding it.
	Waiting *Suggestion `json:"waiting,omitempty"`
	// RecordRead says the record could be read; without it the items and bundles are unknown.
	RecordRead bool `json:"record_read"`
	// RecordError says why it couldn't be read as root.
	RecordError string `json:"record_error,omitempty"`
}

// Excluded is an exclude pattern leaving a path out.
type Excluded struct {
	Pattern string `json:"pattern"`
	At      string `json:"at"`    // what it matches: the path or a folder above it
	Kind    string `json:"from"`  // catalog.FromDefaults, FromCatalog, FromAutomatic or FromUser
	From    string `json:"where"` // for people
}

// TakenBack is a "!" pattern matching the path that takes back an earlier pattern.
type TakenBack struct {
	Pattern  string `json:"pattern"`
	From     string `json:"where"`
	Over     string `json:"over"` // the pattern it takes back
	OverFrom string `json:"over_where"`
	// Helps is false when the path is still left out, because a folder above it is.
	Helps bool `json:"helps"`
}

// Redownload is a catalog hint for data that comes back by itself.
type Redownload struct {
	Label string `json:"label"`
	How   string `json:"how"`
}

// Item is a record item holding the path, with its bundle and why it's there.
type Item struct {
	Type   string        `json:"type"`
	ID     string        `json:"id"`
	User   string        `json:"user,omitempty"`
	Bundle string        `json:"bundle"`
	Title  string        `json:"title"`
	When   string        `json:"when"`
	Reason bundle.Reason `json:"reason"`
	// Ticked says whether the bundle starts ticked when restoring here; nil when unknown.
	Ticked *bool `json:"ticked,omitempty"`
	// Unticked says why it doesn't.
	Unticked string `json:"unticked,omitempty"`
	// Regrouped is the bundle the item goes to when grouped again with today's rules, when that
	// differs from the record's: the next capture moves it. "-" means it's dropped.
	Regrouped string `json:"regrouped,omitempty"`

	describe string
}

// Suggestion is an inbox entry offering to back up a folder.
type Suggestion struct {
	ID   string `json:"id"`
	Path string `json:"path"`
}

// Package is the package a path comes back with.
type Package struct {
	Name     string `json:"name"`
	InRecord bool   `json:"in_record"`
	Bundle   string `json:"bundle,omitempty"`
}

// Explain says whether and how in.Path is backed up.
func Explain(ctx context.Context, in Input) Answer {
	p := filepath.Clean(in.Path)
	a := Answer{Path: p, Items: []Item{}, RecordRead: in.Record != nil, RecordError: in.RecordError}
	if _, err := os.Lstat(onDisk(in.Root, p)); err == nil {
		a.Exists = true
	}
	for i, t := range in.Targets {
		if within(p, t.Path) {
			a.Folder = &in.Targets[i]
			break
		}
	}
	if a.Folder != nil {
		a.inFolder(in)
	} else {
		for name, home := range in.OtherUsers {
			if within(p, home) && (a.OtherUser == "" || name < a.OtherUser) {
				a.OtherUser = name
			}
		}
	}
	if !a.BackedUp {
		for _, d := range in.Ignored {
			if d = filepath.Clean(d); filepath.IsAbs(d) && within(p, d) && (a.Ignored == "" || len(d) < len(a.Ignored)) {
				a.Ignored = d
			}
		}
		for _, e := range in.Inbox {
			if e.Kind == state.KindPath && e.Path != "" && within(p, e.Path) && (a.Waiting == nil || len(e.Path) < len(a.Waiting.Path)) {
				a.Waiting = &Suggestion{ID: e.ID, Path: e.Path}
			}
		}
	}
	if in.Record != nil {
		a.items(ctx, in)
	}
	if in.Owner != "" && !a.hasFile() {
		pkg := &Package{Name: in.Owner}
		if in.Record != nil {
			for _, it := range in.Record.Items {
				if (it.Type == record.TypePkg || it.Type == record.TypeAUR) && it.ID == in.Owner {
					pkg.InRecord, pkg.Bundle = true, it.Bundle
				}
			}
		}
		a.Package = pkg
	}
	return a
}

// inFolder works out what a backup of the folder holding the path leaves out on the way to it:
// whichever exclude pattern, cache folder or other filesystem comes first.
func (a *Answer) inFolder(in Input) {
	source := a.Folder.Path
	var pats []string
	from := map[string]catalog.Exclude{}
	for _, e := range in.Excludes {
		if !e.Skipped {
			pats = append(pats, e.Pattern)
			from[e.Pattern] = e
		}
	}
	excludedAt := ""
	if m, err := catalog.NewMatcher(pats); err == nil {
		v := m.Explain(source, a.Path)
		if v.Excluded {
			e := from[v.Pattern]
			a.Excluded = &Excluded{Pattern: v.Pattern, At: v.At, Kind: e.Kind, From: e.From}
			excludedAt = v.At
		}
		if v.TakenBack != "" {
			a.TakenBack = &TakenBack{Pattern: v.TakenBack, From: from[v.TakenBack].From,
				Over: v.Over, OverFrom: from[v.Over].From, Helps: !v.Excluded}
		}
	}
	cacheAt := cacheDir(in.Root, source, a.Path)
	var left *backup.LeftOut
	if in.Crossing != nil {
		c := in.Crossing(source, a.Path)
		left, a.Subvolume = c.LeftOut, c.Subvolume
	}
	// A backup walks down from the folder and stops at the first of these.
	switch {
	case excludedAt != "" && (cacheAt == "" || depth(excludedAt) <= depth(cacheAt)) && (left == nil || depth(excludedAt) <= depth(left.Path)):
	case cacheAt != "" && (left == nil || depth(cacheAt) <= depth(left.Path)):
		a.Excluded, a.CacheDir = nil, cacheAt
	case left != nil:
		a.Excluded, a.LeftOut = nil, left
	}
	if a.Excluded != nil {
		for _, h := range in.Hints {
			for _, hp := range h.Patterns {
				if hp == a.Excluded.Pattern && !h.Hidden && a.Redownload == nil {
					a.Redownload = &Redownload{Label: h.Label, How: h.How}
				}
			}
		}
	}
	if a.Excluded != nil || a.CacheDir != "" || a.LeftOut != nil {
		a.Subvolume = ""
		if a.TakenBack != nil && a.Excluded == nil {
			a.TakenBack.Helps = false
		}
		return
	}
	a.BackedUp = true
}

// items finds the record items holding the path, and why each is in its bundle.
func (a *Answer) items(ctx context.Context, in Input) {
	rec := in.Record
	repoOf := map[string]string{}
	for _, it := range rec.Items {
		if it.Type == record.TypePkg && it.Repo != "" && it.Repo != "?" {
			repoOf[it.ID] = it.Repo
		}
	}
	regrouped, _, reasons := bundle.Explain(ctx, bundle.Input{
		Items: rec.Items, Rules: in.Rules, Host: rec.Meta.Host, RepoOf: repoOf,
		RecordDir: in.RecordDir, Machine: in.Machine,
	})
	type key struct{ typ, id, user string }
	now := map[key]int{}
	for i, it := range regrouped {
		now[key{it.Type, it.ID, it.User}] = i
	}
	for _, it := range rec.Items {
		if !holds(it, a.Path, in.Owner) {
			continue
		}
		b, _ := rec.Bundles.Get(it.Bundle)
		got := Item{Type: it.Type, ID: it.ID, User: it.User, Bundle: it.Bundle, Title: b.Title, When: b.When,
			describe: strings.Join(strings.Fields(it.Describe()), " ")}
		if b.Title == "" {
			got.Title = it.Bundle
		}
		if i, ok := now[key{it.Type, it.ID, it.User}]; ok {
			got.Reason = reasons[i]
			if regrouped[i].Bundle != it.Bundle {
				got.Regrouped = regrouped[i].Bundle
			}
		} else {
			got.Regrouped = "-"
		}
		if in.Here != nil && b.When != "" {
			on, why, _ := rules.Evaluate(b.When, *in.Here, rec.Meta)
			got.Ticked = &on
			if !on {
				got.Unticked = why
			}
		}
		a.Items = append(a.Items, got)
	}
}

// holds reports whether a record item holds the path: the file itself, a backed-up path it is
// in, or the package owning it.
func holds(it record.Item, p, owner string) bool {
	switch it.Type {
	case record.TypeFile, record.TypeMkdir:
		return it.ID == p
	case record.TypePath:
		if !strings.ContainsAny(it.ID, "*?[") {
			return within(p, it.ID)
		}
		for q := p; ; q = path.Dir(q) {
			if ok, _ := path.Match(it.ID, q); ok {
				return true
			}
			if q == "/" || q == "." {
				return false
			}
		}
	case record.TypePkg, record.TypeAUR:
		return owner != "" && it.ID == owner
	}
	return false
}

func (a *Answer) hasFile() bool {
	for _, it := range a.Items {
		if it.Type == record.TypeFile {
			return true
		}
	}
	return false
}

// cacheTag is how a CACHEDIR.TAG file starts (https://bford.info/cachedir/).
const cacheTag = "Signature: 8a477f597d28d172789f06886806bc55"

// cacheDir returns the first folder from source down to p (p included) marked as a cache, which
// restic's --exclude-caches leaves out, or "".
func cacheDir(root, source, p string) string {
	rel, err := filepath.Rel(source, p)
	if err != nil || rel == "." {
		return ""
	}
	cur := source
	for _, name := range strings.Split(rel, "/") {
		cur = filepath.Join(cur, name)
		f, err := os.Open(filepath.Join(onDisk(root, cur), "CACHEDIR.TAG"))
		if err != nil {
			continue
		}
		buf := make([]byte, len(cacheTag))
		n, _ := f.Read(buf)
		f.Close()
		if string(buf[:n]) == cacheTag {
			return cur
		}
	}
	return ""
}

func onDisk(root, p string) string {
	if root == "" {
		root = "/"
	}
	return filepath.Join(root, p)
}

// within reports whether p is dir or inside it.
func within(p, dir string) bool {
	return p == dir || dir == "/" || strings.HasPrefix(p, strings.TrimSuffix(dir, "/")+"/")
}

func depth(p string) int { return strings.Count(filepath.Clean(p), "/") }
