package restore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"

	"github.com/Evimore/carryon/internal/engine"
	"github.com/Evimore/carryon/internal/record"
)

// Phase is one step of applying a plan. Phases run in the order of Phases.
type Phase string

// The phases, in apply order (T15).
const (
	PhaseRepos        Phase = "repos"         // extra package repositories and their keys
	PhasePackages     Phase = "packages"      // one system update plus packages
	PhaseAUR          Phase = "aur"           // AUR packages, built as the user
	PhaseFiles        Phase = "files"         // system files from the record
	PhaseReview       Phase = "review"        // files shown as a diff and never copied
	PhaseData         Phase = "data"          // paths restored from the repository
	PhaseAccount      Phase = "account"       // groups, login shells, lingering
	PhaseServices     Phase = "services"      // system units enabled or masked
	PhaseDirs         Phase = "dirs"          // folders units need
	PhaseUserServices Phase = "user-services" // user unit files, enabled and masked user units
	PhaseBoot         Phase = "boot"          // kernel parameters
	PhaseTools        Phase = "tools"         // cargo, pipx, npm and Flatpak
)

// Phases lists every phase in apply order, with the label the plan shows.
var Phases = []struct {
	Phase Phase
	Label string
}{
	{PhaseRepos, "Package repos"}, {PhasePackages, "Packages"}, {PhaseAUR, "AUR"},
	{PhaseFiles, "System files"}, {PhaseReview, "Review by hand"}, {PhaseData, "From backup"},
	{PhaseAccount, "Account"}, {PhaseServices, "Services"}, {PhaseDirs, "Folders"},
	{PhaseUserServices, "User units"}, {PhaseBoot, "Kernel params"}, {PhaseTools, "Language tools"},
}

// File states.
const (
	StateNew     = "new"     // nothing at that path here
	StateReplace = "replace" // something different is there
	StateSame    = "same"
)

// Step is one thing the plan would do.
type Step struct {
	Item record.Item
	// User is who the item is for here, for per-user items: the recorded user's match.
	User *LocalUser
	// State is StateNew or StateReplace for files and user unit files.
	State string
	// Why says why a file in PhaseReview isn't copied.
	Why string
	// Target is where a file, folder or home path goes here (with the user's home adapted).
	Target string
	// Adapted says the file's contents or link were rewritten for this machine's users.
	Adapted bool
	// Diff is the change from this machine's version to the backup's (diff -u), for files that
	// exist here.
	Diff string
}

// Skip is a picked item the plan leaves out, and why.
type Skip struct {
	Item record.Item
	Why  string
}

// Plan is what restoring the picked items would change on this machine.
type Plan struct {
	From        string           // the source machine's name
	Snapshot    *engine.Snapshot // nil for a record on disk
	Onto        string           // this machine's hostname
	Bundles     int              // bundles picked
	OfBundles   int              // bundles in the record
	Users       []UserMap        // who receives each recorded user's files
	Steps       map[Phase][]Step
	Skipped     []Skip
	Picked      int      // items picked
	NotBackedUp []string // what the source's not-backed-up list says to re-download, in short

	mainUser string // the record's main user, whose items name no user in format 1
}

// Todo is how many steps the plan has.
func (p *Plan) Todo() int {
	n := 0
	for _, s := range p.Steps {
		n += len(s)
	}
	return n
}

// Done is how many picked items need no change.
func (p *Plan) Done() int { return p.Picked - p.Todo() - len(p.Skipped) }

// reviewOnly are files with disk UUIDs or the kernel command line: shown as a diff, never copied.
var reviewOnly = regexp.MustCompile(`^/etc/(fstab|crypttab|default/limine|kernel/cmdline|default/grub|sdboot-manage\.conf)$`)

// ReviewOnly reports whether a recorded file is only ever shown, never copied: files naming
// disks by UUID (fstab, crypttab, bootloader configs) and /etc/kernel/cmdline.
func ReviewOnly(path string) bool { return reviewOnly.MatchString(path) }

// reviewOnlyWhy is said of review-only files.
const reviewOnlyWhy = "disks have other IDs on every install, so this is never copied; kernel parameters are added on their own"

// storageHooks are initramfs hooks a machine can need to find its root filesystem.
var storageHooks = []string{"bcache", "btrfs", "encrypt", "lvm2", "mdadm", "mdadm_udev", "resume", "sd-encrypt", "zfs"}

var hooksLine = regexp.MustCompile(`(?m)^\s*HOOKS=\(([^)]*)\)`)

func hooksOf(text string) []string {
	m := hooksLine.FindStringSubmatch(text)
	if m == nil {
		return nil
	}
	return strings.Fields(m[1])
}

// Planner makes plans from one source onto this machine.
type Planner struct {
	Source *Source
	Here   *Machine
	Users  []UserMap

	adapt *Adapter
}

// Make works out what restoring the picked items would change here. It only reads: nothing on
// the machine changes. Ported from make_plan.
func (pl *Planner) Make(ctx context.Context, picked Picked) (*Plan, error) {
	src, here := pl.Source, pl.Here
	pl.adapt = NewAdapter(pl.Users)
	rec := src.Record
	sel := picked.Select(rec.Items)
	p := &Plan{
		From: src.Machine(), Snapshot: src.Snapshot, Onto: here.Hostname,
		Bundles: len(picked.Bundles), OfBundles: len(rec.Bundles), Users: pl.Users,
		Steps: map[Phase][]Step{}, Picked: len(sel), mainUser: rec.Meta.User,
	}
	add := func(ph Phase, s Step) { p.Steps[ph] = append(p.Steps[ph], s) }

	installed, err := here.Packages.Installed(ctx)
	if err != nil {
		return nil, err
	}
	sync, err := here.Packages.SyncPackages(ctx)
	if err != nil {
		return nil, err
	}
	liveRepos, err := here.Packages.ConfiguredRepos(ctx)
	if err != nil {
		return nil, err
	}
	isInstalled, inSync := setOf(installed), setOf(sync)
	newRepos := map[string]bool{}
	for _, it := range sel {
		if it.Type == record.TypeRepo && !slices.Contains(liveRepos, it.ID) {
			newRepos[it.ID] = true
		}
	}
	kparams := here.kernelParams()
	var targets []LocalUser
	for _, m := range pl.Users {
		targets = append(targets, m.To)
	}
	var tools map[string]bool
	if slices.ContainsFunc(sel, isTool) {
		tools = here.tools(ctx, targets)
	}

	for _, it := range sel {
		u := pl.userOf(it)
		switch it.Type {
		case record.TypeRepo:
			if newRepos[it.ID] {
				add(PhaseRepos, Step{Item: it})
			}
		case record.TypePkg:
			switch {
			case isInstalled[it.ID]:
			case inSync[it.ID] || newRepos[it.Repo]:
				add(PhasePackages, Step{Item: it})
			default:
				p.Skipped = append(p.Skipped, Skip{it, "not in any repository"})
			}
		case record.TypeAUR:
			if !isInstalled[it.ID] {
				add(PhaseAUR, Step{Item: it})
			}
		case record.TypeFile:
			if it.TooBig {
				p.Skipped = append(p.Skipped, Skip{it, "too big to keep in the record; copy it from the backup by hand"})
				continue
			}
			st := pl.fileStep(it)
			if st.State == StateSame {
				continue
			}
			switch {
			case ReviewOnly(it.ID):
				st.Why = reviewOnlyWhy
				add(PhaseReview, st)
			default:
				if why := pl.bootRisk(it); why != "" {
					st.Why = why
					add(PhaseReview, st)
				} else {
					add(PhaseFiles, st)
				}
			}
		case record.TypePath:
			st := Step{Item: it, User: u, Target: it.ID}
			if it.Home != "" && u != nil {
				st.Target = filepath.Join(u.Home, it.Home)
			}
			add(PhaseData, st)
		case record.TypeGroup:
			if u != nil && !slices.Contains(u.Groups, it.ID) {
				add(PhaseAccount, Step{Item: it, User: u})
			}
		case record.TypeShell:
			if u != nil && it.ID != u.Shell {
				add(PhaseAccount, Step{Item: it, User: u})
			}
		case record.TypeLinger:
			if u != nil && !here.lexists("/var/lib/systemd/linger/"+u.Name) {
				add(PhaseAccount, Step{Item: it, User: u})
			}
		case record.TypeMkdir:
			dir := it.ID
			if it.Rel != "" && u != nil {
				dir = filepath.Join(u.Home, it.Rel)
			}
			if !here.isDir(dir) {
				add(PhaseDirs, Step{Item: it, User: u, Target: dir})
			}
		case record.TypeSvc, record.TypeMask:
			want := map[string]string{record.TypeSvc: "enabled", record.TypeMask: "masked"}[it.Type]
			if here.unitState(ctx, it.ID) != want {
				add(PhaseServices, Step{Item: it})
			}
		case record.TypeUsvc, record.TypeUunit, record.TypeUmask:
			if u == nil {
				continue
			}
			if st, todo := pl.userUnitStep(it, u); todo {
				add(PhaseUserServices, st)
			}
		case record.TypeKparam:
			if !kparams[it.ID] {
				add(PhaseBoot, Step{Item: it})
			}
		case record.TypeCargo, record.TypePipx, record.TypeNpm, record.TypeFlatpak:
			who := ""
			if it.Type == record.TypeCargo || it.Type == record.TypePipx || (it.Type == record.TypeFlatpak && it.Scope == "user") {
				if u == nil {
					continue
				}
				who = u.Name
			}
			if !tools[toolKey(it.Type, it.ID, who)] {
				add(PhaseTools, Step{Item: it, User: u})
			}
		}
	}
	p.NotBackedUp = notBackedUp(src.Dir)
	return p, nil
}

func isTool(it record.Item) bool {
	switch it.Type {
	case record.TypeCargo, record.TypePipx, record.TypeNpm, record.TypeFlatpak:
		return true
	}
	return false
}

func setOf(names []string) map[string]bool {
	m := make(map[string]bool, len(names))
	for _, n := range names {
		m[n] = true
	}
	return m
}

// userOf returns the user here who gets a per-user item: the match of the item's user, or in
// format 1 records, of the record's only user.
func (pl *Planner) userOf(it record.Item) *LocalUser {
	if it.Type == record.TypeFlatpak && it.Scope != "user" {
		return nil // installed for everyone
	}
	name := it.User
	if name == "" {
		switch it.Type {
		case record.TypeGroup, record.TypeShell, record.TypeLinger, record.TypeUsvc, record.TypeUunit,
			record.TypeUmask, record.TypeCargo, record.TypePipx, record.TypeFlatpak:
		case record.TypeMkdir:
			if it.Rel == "" {
				return nil
			}
		case record.TypePath:
			if it.Home == "" {
				return nil
			}
		default:
			return nil
		}
		if pl.Source.Record.Meta.Format >= record.FormatCurrent && it.Type != record.TypeMkdir && it.Type != record.TypePath {
			return nil // format 2 names the user of every per-user item
		}
		name = pl.Source.Record.Meta.User
	}
	for i := range pl.Users {
		if pl.Users[i].From.Name == name {
			return &pl.Users[i].To
		}
	}
	return nil
}

// saved returns the backup's copy of a recorded file, adapted for this machine's users.
func (pl *Planner) saved(it record.Item) ([]byte, bool, error) {
	raw, err := os.ReadFile(filepath.Join(pl.Source.Dir, FilesDir, strings.TrimPrefix(it.ID, "/")))
	if err != nil {
		return nil, false, err
	}
	out := pl.adapt.Bytes(raw)
	return out, !bytes.Equal(out, raw), nil
}

// fileStep compares a recorded file with this machine's. Ported from file_state.
func (pl *Planner) fileStep(it record.Item) Step {
	here := pl.Here
	st := Step{Item: it, Target: it.ID}
	if it.Link != "" {
		want := pl.adapt.String(it.Link)
		st.Adapted = want != it.Link
		if got, ok := here.readlink(it.ID); ok && got == want {
			st.State = StateSame
		} else if here.lexists(it.ID) {
			st.State = StateReplace
		} else {
			st.State = StateNew
		}
		return st
	}
	saved, adapted, err := pl.saved(it)
	st.Adapted = adapted
	if !here.lexists(it.ID) {
		st.State = StateNew
		return st
	}
	if err != nil {
		st.State = StateReplace
		st.Diff = fmt.Sprintf("(the backup's copy can't be read: %v)\n", err)
		return st
	}
	live, lerr := os.ReadFile(here.path(it.ID))
	if lerr == nil && !here.isSymlink(it.ID) && sha256.Sum256(live) == sha256.Sum256(saved) {
		st.State = StateSame
		return st
	}
	st.State = StateReplace
	if lerr != nil {
		st.Diff = fmt.Sprintf("(this machine's %s can't be read: %v)\n", it.ID, lerr)
	} else {
		st.Diff = UnifiedDiff(it.ID+" (this machine)", it.ID+" (backup)", live, saved)
	}
	return st
}

// bootRisk says why copying an initramfs config could leave this machine unbootable: it lacks
// storage hooks this machine uses. "" if it can't. Ported from boot_risk.
func (pl *Planner) bootRisk(it record.Item) string {
	if !strings.HasPrefix(it.ID, "/etc/mkinitcpio.conf") {
		return ""
	}
	raw, err := os.ReadFile(filepath.Join(pl.Source.Dir, FilesDir, strings.TrimPrefix(it.ID, "/")))
	if err != nil {
		return ""
	}
	saved := hooksOf(string(raw))
	if len(saved) == 0 {
		return ""
	}
	live := map[string]bool{}
	files := []string{pl.Here.path("/etc/mkinitcpio.conf")}
	drop, _ := filepath.Glob(pl.Here.path("/etc/mkinitcpio.conf.d/*.conf"))
	sort.Strings(drop)
	for _, f := range append(files, drop...) {
		if b, err := os.ReadFile(f); err == nil {
			for _, h := range hooksOf(string(b)) {
				live[h] = true
			}
		}
	}
	var lost []string
	for _, h := range storageHooks {
		if live[h] && !slices.Contains(saved, h) {
			lost = append(lost, h)
		}
	}
	if len(lost) == 0 {
		return ""
	}
	return fmt.Sprintf("this machine needs initramfs hooks the backup lacks (%s): copying it could stop it booting", strings.Join(lost, ", "))
}

// userUnitStep compares a user unit item with the user's units here.
func (pl *Planner) userUnitStep(it record.Item, u *LocalUser) (Step, bool) {
	here := pl.Here
	udir := filepath.Join(u.Home, ".config/systemd/user")
	st := Step{Item: it, User: u, Target: filepath.Join(udir, it.ID)}
	switch it.Type {
	case record.TypeUsvc:
		for _, pat := range []string{"*.wants", "*.requires"} {
			if m, _ := filepath.Glob(filepath.Join(here.path(udir), pat, it.ID)); len(m) > 0 {
				return st, false
			}
		}
		return st, true
	case record.TypeUmask:
		t, ok := here.readlink(st.Target)
		return st, !(ok && t == "/dev/null")
	}
	// uunit: format 1 keeps unit files in user-units/, format 2 in user-units/<user>/.
	srcPath := filepath.Join(pl.Source.Dir, UserUnitsDir, it.ID)
	if it.User != "" {
		srcPath = filepath.Join(pl.Source.Dir, UserUnitsDir, it.User, it.ID)
	}
	raw, err := os.ReadFile(srcPath)
	if err != nil {
		st.State = StateReplace
		st.Diff = fmt.Sprintf("(the backup's copy can't be read: %v)\n", err)
		return st, true
	}
	want := pl.adapt.Bytes(raw)
	st.Adapted = !bytes.Equal(want, raw)
	fi, err := os.Stat(here.path(st.Target))
	if err != nil || !fi.Mode().IsRegular() {
		st.State = StateNew
		return st, true
	}
	live, err := os.ReadFile(here.path(st.Target))
	if err == nil && bytes.Equal(live, want) {
		return st, false
	}
	st.State = StateReplace
	if err == nil {
		st.Diff = UnifiedDiff(st.Target+" (this machine)", st.Target+" (backup)", live, want)
	}
	return st, true
}

var (
	steamHeading = regexp.MustCompile(`^## (.+): (\d+), (.+)$`)
	hintLine     = regexp.MustCompile(`^- \*\*(.+?)\*\*`)
)

// notBackedUp reads the headings of the source's not-backed-up list: what to re-download.
func notBackedUp(dir string) []string {
	b, err := os.ReadFile(filepath.Join(dir, NotBackedUpMD))
	if err != nil {
		return nil
	}
	var out []string
	for _, l := range strings.Split(string(b), "\n") {
		if m := steamHeading.FindStringSubmatch(l); m != nil {
			out = append(out, fmt.Sprintf("%s %s (%s)", m[2], m[1], m[3]))
		} else if m := hintLine.FindStringSubmatch(l); m != nil {
			out = append(out, m[1])
		}
	}
	return out
}
