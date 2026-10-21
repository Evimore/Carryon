package detect

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/Evimore/carryon/internal/bundle"
	"github.com/Evimore/carryon/internal/capture"
	"github.com/Evimore/carryon/internal/catalog"
	"github.com/Evimore/carryon/internal/dest"
	"github.com/Evimore/carryon/internal/paths"
	"github.com/Evimore/carryon/internal/record"
	"github.com/Evimore/carryon/internal/state"
	"github.com/Evimore/carryon/internal/sysexec"
	"github.com/Evimore/carryon/internal/ui"
)

// The scans look for folders in /var/lib, /var/opt and /srv that hold data no package owns, such
// as /var/lib/jellyfin filling up after Jellyfin first ran, and suggest backing them up. The
// nightly backup looks at every folder; the pacman hook only at the folders of the packages it was
// given, within its time limit. See docs/design.md, "Noticing new things".

// ScanRoots are the folders whose subfolders a scan looks at.
var ScanRoots = []string{"/var/lib", "/var/opt", "/srv"}

// privateDir holds the state folders of units with DynamicUser=yes (/var/lib/foo is a link to
// /var/lib/private/foo), so a scan looks at its subfolders instead of the folder itself.
const privateDir = "/var/lib/private"

// maxUnsure is how many files of a folder the pacman hook asks the package manager about, so it
// stays quick: one `pacman -Qo` call.
const maxUnsure = 200

// gib is a gibibyte, the unit of detect.large_path_gb.
const gib = 1 << 30

// Manager is what a scan asks the package manager. pkgmgr.Manager provides it.
type Manager interface {
	Installed(ctx context.Context) ([]string, error)
	OwnedFiles(ctx context.Context) ([]string, error)
	FilesOf(ctx context.Context, pkgs ...string) (map[string][]string, error)
	Owners(ctx context.Context, paths []string) (map[string]string, error)
}

// ScanOptions configure a scan. Paths are system paths; Root says where they are found.
type ScanOptions struct {
	// Root is where files are read from: "/" (or empty) for the real system, a fake tree in tests.
	Root string
	// Runner runs systemctl, to read the units' User=, DynamicUser= and StateDirectory=. Nil
	// leaves the units out of guessing a folder's app.
	Runner sysexec.Runner
	// Manager is the package manager.
	Manager Manager
	// Items are the record's: its packages, with their bundles, and its enabled units.
	Items []record.Item
	// Covered are the folders the backup covers. Folders in them are already backed up.
	Covered []string
	// Excluded is the effective exclude list. Nil leaves nothing out.
	Excluded func(path string) bool
	// Mounts is the mount table, without the nested btrfs subvolumes the backup takes in. Folders
	// that are other filesystems are left out, and sizes don't count what is mounted inside.
	Mounts dest.MountTable
	// Ignore is detect.ignore: these folders and anything inside them are never suggested.
	Ignore []string
	// Known are the known system folders (ReadKnownFolders): patterns as path.Match takes them.
	Known []string
	// LargeGB is detect.large_path_gb: bigger folders are marked large.
	LargeGB float64
	// All looks at every folder. Otherwise only the folders of the packages in Only are looked
	// at: the ones they own files in, or named like them.
	All  bool
	Only []string
}

// Findings are what a scan found.
type Findings struct {
	// Found are the suggestions, sorted by path, with their app, bundle and size. When and by
	// what they were found is left for Suggest.
	Found []state.Entry
	// All says the scan looked at every folder, so a suggestion it didn't find again is stale.
	All bool
}

// ReadKnownFolders reads the list of known system folders: one path or path.Match pattern per
// line; blank lines and lines starting with # are skipped, and so is anything not absolute. It
// ships as data, in the share folder, so it can grow without a new binary.
func ReadKnownFolders(file string) ([]string, error) {
	f, err := os.Open(file)
	if err != nil {
		return nil, fmt.Errorf("reading the list of known system folders: %w", err)
	}
	defer f.Close()
	var out []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") || !strings.HasPrefix(line, "/") {
			continue
		}
		out = append(out, path.Clean(line))
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("reading %s: %w", file, err)
	}
	return out, nil
}

// Scan looks for folders holding data no package owns and works out, for each, its probable app
// and bundle and its size. It stops when ctx is done.
func Scan(ctx context.Context, opt ScanOptions) (Findings, error) {
	s := &scanner{opt: opt, root: opt.Root}
	if s.root == "" {
		s.root = "/"
	}
	res := Findings{All: opt.All}
	if opt.Manager == nil {
		return res, errors.New("detect: a package manager is needed")
	}
	cands := s.candidates()
	if len(cands) == 0 {
		return res, nil
	}
	owned := map[string]bool{}
	var only []string
	if opt.All {
		files, err := opt.Manager.OwnedFiles(ctx)
		if err != nil {
			return res, fmt.Errorf("listing the files packages own: %w", err)
		}
		for _, f := range files {
			if underAny(f, ScanRoots) {
				owned[f] = true
			}
		}
	} else {
		var err error
		if only, err = s.installed(ctx); err != nil || len(only) == 0 {
			return res, err
		}
		files, err := opt.Manager.FilesOf(ctx, only...)
		if err != nil {
			return res, fmt.Errorf("listing the files of %s: %w", strings.Join(only, ", "), err)
		}
		for _, list := range files {
			for _, f := range list {
				owned[strings.TrimRight(f, "/")] = true
			}
		}
		cands = s.ofPackages(cands, only, owned)
	}

	var found []string
	for _, c := range cands {
		ok, err := s.holdsData(ctx, c, owned, opt.All)
		if err != nil {
			return res, err
		}
		if ok {
			found = append(found, c)
		}
	}
	if len(found) == 0 {
		return res, nil
	}
	apps, err := s.apps(ctx, found, only)
	if err != nil {
		return res, err
	}
	for _, f := range found {
		if err := ctx.Err(); err != nil {
			return res, err
		}
		e := state.NewEntry(state.KindPath, f)
		e.App = apps[f]
		e.Bundle = s.bundleOf(e.App)
		if e.Size, err = s.size(ctx, f); err != nil {
			return res, err
		}
		e.Large = e.Size > int64(opt.LargeGB*gib)
		res.Found = append(res.Found, e)
	}
	return res, nil
}

// size measures folder p without walking into mounts. It returns when ctx is done, leaving the
// measuring to finish on its own.
func (s *scanner) size(ctx context.Context, p string) (int64, error) {
	got := make(chan int64, 1)
	go func() { got <- catalog.DiskUsage([]string{s.real(p)}, s.mountsUnder(p)) }()
	select {
	case n := <-got:
		return n, nil
	case <-ctx.Done():
		return 0, ctx.Err()
	}
}

// scanner is one scan in progress.
type scanner struct {
	opt  ScanOptions
	root string
}

// real is where a system path is found under the root.
func (s *scanner) real(p string) string { return filepath.Join(s.root, p) }

// system is the system path of a real path under the root.
func (s *scanner) system(real string) string {
	rel, err := filepath.Rel(s.root, real)
	if err != nil {
		return real
	}
	return filepath.Join("/", rel)
}

// candidates lists the subfolders of the scan roots that could be suggested: not a known system
// folder, ignored, already backed up, left out by the excludes or another filesystem. Symlinks
// aren't followed. They are sorted.
func (s *scanner) candidates() []string {
	var out []string
	for _, r := range ScanRoots {
		for _, name := range subfolders(s.real(r)) {
			p := path.Join(r, name)
			if p == privateDir {
				for _, sub := range subfolders(s.real(p)) {
					out = append(out, path.Join(p, sub))
				}
				continue
			}
			out = append(out, p)
		}
	}
	mountPoints := map[string]bool{}
	for _, m := range s.opt.Mounts {
		mountPoints[filepath.Clean(m.MountPoint)] = true
	}
	var keep []string
	for _, p := range out {
		switch {
		case s.known(p), underAny(p, s.opt.Ignore), underAny(p, s.opt.Covered),
			s.opt.Excluded != nil && s.opt.Excluded(p), mountPoints[p]:
			continue
		}
		keep = append(keep, p)
	}
	sort.Strings(keep)
	return keep
}

// subfolders lists the names of the folders in dir, not following symlinks.
func subfolders(dir string) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() {
			names = append(names, e.Name())
		}
	}
	return names
}

// known reports whether p is a known system folder or inside one.
func (s *scanner) known(p string) bool {
	for _, k := range s.opt.Known {
		if ok, _ := path.Match(k, p); ok || Under(p, k) {
			return true
		}
	}
	return false
}

// installed is the packages of Only that are installed: the ones a removal took away have no
// folders left to look at.
func (s *scanner) installed(ctx context.Context) ([]string, error) {
	if len(s.opt.Only) == 0 {
		return nil, nil
	}
	all, err := s.opt.Manager.Installed(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing the installed packages: %w", err)
	}
	have := map[string]bool{}
	for _, p := range all {
		have[p] = true
	}
	var out []string
	for _, p := range s.opt.Only {
		if have[p] && !slices.Contains(out, p) {
			out = append(out, p)
		}
	}
	return out, nil
}

// ofPackages keeps the candidates that are the folders of pkgs: they own the folder or a file in
// it (owned holds their files), or the folder is named like one of them.
func (s *scanner) ofPackages(cands, pkgs []string, owned map[string]bool) []string {
	var out []string
	for _, c := range cands {
		mine := false
		for f := range owned {
			if Under(f, c) {
				mine = true
				break
			}
		}
		for _, p := range pkgs {
			if mine {
				break
			}
			mine = bundle.NameMatch(c, p) != bundle.NoMatch
		}
		if mine {
			out = append(out, c)
		}
	}
	return out
}

// mountsUnder lists the real paths of the mount points inside folder p, which a scan doesn't
// walk into.
func (s *scanner) mountsUnder(p string) []string {
	var out []string
	for _, m := range s.opt.Mounts.Under(p) {
		out = append(out, s.real(m.MountPoint))
	}
	return out
}

// errFound stops a walk at the first file no package owns.
var errFound = errors.New("found")

// holdsData reports whether folder p holds a file no package owns. owned are files packages own;
// when complete, it lists every one, else the files it doesn't list are asked about, up to
// maxUnsure of them. Empty folders hold no data. Mounts inside p aren't walked into.
func (s *scanner) holdsData(ctx context.Context, p string, owned map[string]bool, complete bool) (bool, error) {
	skip := map[string]bool{}
	for _, m := range s.mountsUnder(p) {
		skip[m] = true
	}
	var unsure []string
	err := filepath.WalkDir(s.real(p), func(file string, d fs.DirEntry, err error) error {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		if err != nil {
			return nil // unreadable: what can be read decides
		}
		if d.IsDir() {
			if skip[file] {
				return filepath.SkipDir
			}
			return nil
		}
		sys := s.system(file)
		switch {
		case owned[sys]:
			return nil
		case complete:
			return errFound
		}
		unsure = append(unsure, sys)
		if len(unsure) >= maxUnsure {
			return filepath.SkipAll
		}
		return nil
	})
	switch {
	case errors.Is(err, errFound):
		return true, nil
	case err != nil:
		return false, err
	case len(unsure) == 0:
		return false, nil
	}
	owners, err := s.opt.Manager.Owners(ctx, unsure)
	if err != nil {
		return false, fmt.Errorf("asking which package owns the files in %s: %w", p, err)
	}
	for _, f := range unsure {
		if owners[f] == "" {
			return true, nil
		}
	}
	return false, nil
}

// apps works out the probable app of each folder: the package owning the folder itself, else the
// package of the unit whose StateDirectory= it is, else a package named like it, else the package
// of the unit whose User= owns it. only are packages to match by name besides the record's.
func (s *scanner) apps(ctx context.Context, folders, only []string) (map[string]string, error) {
	owners, err := s.opt.Manager.Owners(ctx, folders)
	if err != nil {
		return nil, fmt.Errorf("asking which package owns %s: %w", strings.Join(folders, ", "), err)
	}
	byDir, byUser := s.units(ctx)
	accounts := map[uint32]string{}
	if users, err := capture.Accounts(s.root); err == nil {
		for _, u := range users {
			if _, dup := accounts[uint32(u.UID)]; !dup {
				accounts[uint32(u.UID)] = u.Name
			}
		}
	}
	out := map[string]string{}
	for _, f := range folders {
		app := owners[f]
		if app == "" {
			app = byDir[f]
		}
		if app == "" {
			app = s.byName(f, only)
		}
		if app == "" {
			if name := accounts[ownerUID(s.real(f))]; name != "" && name != "root" {
				app = byUser[name]
			}
		}
		out[f] = app
	}
	return out, nil
}

// units reads the record's enabled units that come with a package and maps their state folders
// (StateDirectory=, under /var/lib/private with DynamicUser=yes) and their users (User=) to that
// package. Without systemctl, they map nothing: units are only a hint.
func (s *scanner) units(ctx context.Context) (byDir, byUser map[string]string) {
	byDir, byUser = map[string]string{}, map[string]string{}
	pkgOf := map[string]string{}
	var units []string
	for _, it := range s.opt.Items {
		if it.Type == record.TypeSvc && it.Pkg != "" {
			units = append(units, it.ID)
			pkgOf[it.ID] = it.Pkg
		}
	}
	if s.opt.Runner == nil || len(units) == 0 {
		return byDir, byUser
	}
	sort.Strings(units)
	props, err := capture.UnitProps(ctx, s.opt.Runner, units, "User", "DynamicUser", "StateDirectory")
	if err != nil {
		return byDir, byUser
	}
	for _, u := range units {
		p, pkg := props[u], pkgOf[u]
		for _, dir := range strings.Fields(p["StateDirectory"]) {
			dir, _, _ = strings.Cut(dir, ":") // StateDirectory=foo:bar makes bar a link to foo
			top, _, _ := strings.Cut(strings.Trim(dir, "/"), "/")
			if top == "" {
				continue
			}
			for _, d := range []string{path.Join("/var/lib", top), path.Join(privateDir, top)} {
				if byDir[d] == "" {
					byDir[d] = pkg
				}
			}
		}
		if user := p["User"]; user != "" && user != "root" && byUser[user] == "" {
			byUser[user] = pkg
		}
	}
	return byDir, byUser
}

// byName finds the package named most like folder f, among the record's packages and only.
// Packages installed on purpose win over dependencies, then shorter names.
func (s *scanner) byName(f string, only []string) string {
	type cand struct {
		name     string
		match    int
		explicit bool
	}
	seen := map[string]bool{}
	var cands []cand
	for _, it := range s.opt.Items {
		if (it.Type == record.TypePkg || it.Type == record.TypeAUR) && !seen[it.ID] {
			seen[it.ID] = true
			cands = append(cands, cand{it.ID, bundle.NameMatch(f, it.ID), !it.AsDeps})
		}
	}
	for _, p := range only {
		if !seen[p] {
			seen[p] = true
			cands = append(cands, cand{p, bundle.NameMatch(f, p), false})
		}
	}
	best := cand{}
	for _, c := range cands {
		if c.match == bundle.NoMatch {
			continue
		}
		switch {
		case best.name == "",
			c.match != best.match && c.match > best.match,
			c.match == best.match && c.explicit != best.explicit && c.explicit,
			c.match == best.match && c.explicit == best.explicit &&
				(len(c.name) < len(best.name) || len(c.name) == len(best.name) && c.name < best.name):
			best = c
		}
	}
	return best.name
}

// bundleOf is the bundle a folder of app's would join: the app bundle the record already puts
// the package in, or else "app-" and its name, the bundle grouping makes for it.
func (s *scanner) bundleOf(app string) string {
	if app == "" {
		return ""
	}
	for _, it := range s.opt.Items {
		if (it.Type == record.TypePkg || it.Type == record.TypeAUR) && it.ID == app &&
			it.Bundle != "" && !bundle.IsBuiltin(it.Bundle) {
			return it.Bundle
		}
	}
	return "app-" + app
}

// ownerUID is the user ID owning a file, or 0 when it can't be read.
func ownerUID(real string) uint32 {
	fi, err := os.Lstat(real)
	if err != nil {
		return 0
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return st.Uid
	}
	return 0
}

// SuggestOptions say what to do with a scan's findings.
type SuggestOptions struct {
	Dirs paths.Dirs
	// Mode is detect.new_paths: "ask", "auto" or "off".
	Mode string
	// By says what found them: state.ByHook, ByBackup...
	By string
	// Wait is how long to wait for the inbox lock. The pacman hook waits only a moment.
	Wait time.Duration
	// Out receives what "auto" did, as `carryon add` says it. Nil discards it.
	Out io.Writer
	Now func() time.Time
}

// Outcome is what Suggest did.
type Outcome struct {
	New      []state.Entry // "ask": the suggestions the inbox didn't have yet
	Added    []state.Entry // "auto": the folders now backed up
	Stale    int           // suggestions taken out because a full scan didn't find them again
	SetAside string        // where a damaged inbox was set aside
}

// Suggest does what detect.new_paths says with a scan's findings: "ask" puts them in the inbox
// (and, after a scan of everything, takes out the folder suggestions it didn't find again: backed
// up, ignored, owned or gone since), "auto" backs them up as `carryon add` would, and "off" does
// nothing. A busy inbox returns state.ErrInboxBusy: the next scan finds them again.
func Suggest(f Findings, opt SuggestOptions) (Outcome, error) {
	var res Outcome
	if opt.Now == nil {
		opt.Now = time.Now
	}
	if opt.Out == nil {
		opt.Out = io.Discard
	}
	now := opt.Now()
	switch opt.Mode {
	case "ask":
		if len(f.Found) == 0 && !f.All {
			return res, nil
		}
		if !f.All && !newTo(opt.Dirs, f.Found) {
			return res, nil // nothing new: don't wait for the lock
		}
		setAside, err := state.UpdateInbox(opt.Dirs, opt.Wait, func(in *state.Inbox) error {
			res.New, res.Stale = nil, 0
			found := map[string]bool{}
			for _, e := range f.Found {
				found[e.ID] = true
				e.Found, e.By = now, opt.By
				if in.Put(e) {
					res.New = append(res.New, e)
				}
			}
			if f.All {
				for _, e := range slices.Clone(in.Entries) {
					if e.Kind == state.KindPath && !found[e.ID] {
						in.Remove(e.ID)
						res.Stale++
					}
				}
			}
			return nil
		})
		res.SetAside = setAside
		return res, err
	case "auto":
		var errs []error
		for _, e := range f.Found {
			e.Found, e.By = now, opt.By
			if err := Accept(opt.Out, opt.Dirs, e.Path, e, opt.Wait); err != nil {
				errs = append(errs, fmt.Errorf("backing up %s: %w", e.Path, err))
				continue
			}
			res.Added = append(res.Added, e)
		}
		return res, errors.Join(errs...)
	}
	return res, nil
}

// newTo reports whether some of found aren't in the inbox, or are but have changed.
func newTo(dirs paths.Dirs, found []state.Entry) bool {
	in, err := state.ReadInbox(dirs)
	if err != nil {
		return true
	}
	for _, e := range found {
		have, ok := in.Find(e.ID)
		if !ok || have.App != e.App || have.Bundle != e.Bundle || have.Size != e.Size || have.Large != e.Large {
			return true
		}
	}
	return false
}

// Describe says in a few words what Carryon knows of a found folder: "probably jellyfin-server,
// 1.2 GiB, large".
func Describe(e state.Entry) string {
	var parts []string
	if e.App != "" {
		parts = append(parts, "probably "+e.App)
	}
	if e.Size > 0 {
		parts = append(parts, ui.Bytes(uint64(e.Size)))
	}
	if e.Large {
		parts = append(parts, "large")
	}
	return strings.Join(parts, ", ")
}

// scanStep is the pacman hook's look at the folders of the packages it was given. It prints one
// line per new suggestion, or per folder "auto" backs up, and notes the latter in the hook's run.
// It stops a little before the hook's time limit, so the hook's run is still recorded. A busy
// inbox is a suggestion skipped, not a problem: the nightly backup finds it again.
func scanStep(ctx context.Context, opt HookOptions, out *hintWriter, run *state.Run) {
	if opt.Scan == nil || len(opt.Packages) == 0 || ctx.Err() != nil {
		return
	}
	sctx, cancel := context.WithCancel(ctx)
	if deadline, ok := ctx.Deadline(); ok {
		margin := min(scanMargin, time.Until(deadline)/4)
		sctx, cancel = context.WithDeadline(ctx, deadline.Add(-margin))
	}
	defer cancel()
	type result struct {
		res Outcome
		err error
	}
	got := make(chan result, 1)
	go func() {
		res, err := safeScan(sctx, opt.Scan, opt.Packages)
		got <- result{res, err}
	}()
	var r result
	select {
	case r = <-got:
	case <-sctx.Done():
	}
	switch {
	case ctx.Err() != nil:
		return // Hook says so
	case sctx.Err() != nil:
		out.hint("Carryon stopped looking for new data folders, so pacman isn't held up. The nightly backup looks instead.")
		return
	}
	for _, e := range r.res.New {
		out.hint("Carryon: %s holds new data (%s). Back it up with: sudo carryon add %s", e.Path, Describe(e), e.ID)
	}
	for _, e := range r.res.Added {
		note := AddedNote(e)
		out.hint("Carryon: %s", note)
		run.Notes = append(run.Notes, note)
	}
	if r.err != nil && !errors.Is(r.err, state.ErrInboxBusy) {
		out.hint("Carryon couldn't look for new data folders: %v. The nightly backup looks again.", r.err)
	}
}

// scanMargin is how long before the hook's time limit its scan stops, so the rest of the hook
// can finish.
const scanMargin = 10 * time.Second

// AddedNote says, for pacman's output and the run history, that "auto" now backs up a found
// folder.
func AddedNote(e state.Entry) string {
	what := Describe(e)
	if what != "" {
		what = " (" + what + ")"
	}
	return fmt.Sprintf("now backing up %s%s, found holding new data (detect.new_paths = \"auto\")", e.Path, what)
}

// safeScan runs scan, turning a panic into an error.
func safeScan(ctx context.Context, scan func(context.Context, []string) (Outcome, error), pkgs []string) (res Outcome, err error) {
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("a bug stopped it: %v", p)
		}
	}()
	return scan(ctx, pkgs)
}
