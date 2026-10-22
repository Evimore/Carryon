package restore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/Evimore/carryon/internal/bundle"
	"github.com/Evimore/carryon/internal/paths"
	"github.com/Evimore/carryon/internal/pkgmgr"
	"github.com/Evimore/carryon/internal/record"
	"github.com/Evimore/carryon/internal/rules"
	"github.com/Evimore/carryon/internal/state"
	"github.com/Evimore/carryon/internal/ui"
)

// An offer asks whether to restore one app's saved settings from the kept source: a restore
// entry in the inbox, whose Changes say what the plan would change. Restore never applies
// anything the user hasn't seen, so carryon-restore@.service applies an offer only while its plan
// is the one offered: the plan's fingerprint is kept next to the kept record (root-only, since it
// is made from file contents), and a plan that changed renews the offer instead.

// MaxChanges is how many lines an offer's Changes have at most; the last one says how many more.
const MaxChanges = 8

// noPackages stands for the package manager when planning an offer: an offer installs nothing,
// so it needs no package lists.
type noPackages struct{}

func (noPackages) Installed(context.Context) ([]string, error)        { return nil, nil }
func (noPackages) SyncPackages(context.Context) ([]string, error)     { return nil, nil }
func (noPackages) ConfiguredRepos(context.Context) ([]string, error)  { return nil, nil }
func (noPackages) Packages(context.Context) ([]pkgmgr.Package, error) { return nil, nil }
func (noPackages) RemoveCommand([]string) []string                    { return nil }

// installsSoftware reports whether restoring an item installs software: repositories, packages,
// AUR packages and language tools. Offers leave these to the user and pacman.
func installsSoftware(it record.Item) bool {
	switch it.Type {
	case record.TypeRepo, record.TypePkg, record.TypeAUR:
		return true
	}
	return isTool(it)
}

// AutoUsers matches the backup's users to users here without asking, as runs without a terminal
// do: by name, else the only user here. A backed-up user with no match gets nothing.
func AutoUsers(meta record.Meta, here []LocalUser) []UserMap {
	if m, err := MapUsers(meta, here, ui.Defaults{}, MapOptions{}); err == nil {
		return m
	}
	var out []UserMap
	for _, ru := range meta.AllUsers() {
		if i := slices.IndexFunc(here, func(u LocalUser) bool { return u.Name == ru.Name }); i >= 0 {
			out = append(out, UserMap{From: ru, To: here[i]})
		}
	}
	return out
}

// PlanOffer makes the plan an offer is about: what restoring bundleID's items other than
// software would change here, for the users AutoUsers matches. The pacman hook and
// carryon-restore@.service both plan this way, so they agree on what was offered. It returns the
// planner too, for applying the plan.
func PlanOffer(ctx context.Context, src *Source, here *Machine, bundleID string) (*Plan, *Planner, error) {
	if _, ok := src.Record.Bundles.Get(bundleID); !ok {
		return nil, nil, fmt.Errorf("the kept record of %s has no bundle %s", src.Machine(), bundleID)
	}
	m := *here
	m.Packages = noPackages{}
	if m.Users == nil {
		local, err := ReadUsers(m.root())
		if err != nil {
			return nil, nil, err
		}
		m.Users = local
	}
	pick := Picked{Bundles: []string{bundleID}}
	for _, it := range src.Record.Items {
		if it.Bundle == bundleID && installsSoftware(it) {
			pick.Deselected = append(pick.Deselected, choiceKey(it))
		}
	}
	pl := &Planner{Source: src, Here: &m, Users: AutoUsers(src.Record.Meta, m.Users)}
	p, err := pl.Make(ctx, pick)
	if err != nil {
		return nil, nil, err
	}
	// A home folder of a backed-up user nobody here receives can't be restored: don't offer it.
	var data []Step
	for _, s := range p.Steps[PhaseData] {
		if s.Item.Home != "" && s.User == nil {
			p.Skipped = append(p.Skipped, Skip{s.Item, "nobody here receives its user's files: restore it with sudo carryon restore --bundle " + bundleID + " --user OLD=NEW"})
			continue
		}
		data = append(data, s)
	}
	if len(data) == 0 {
		delete(p.Steps, PhaseData)
	} else {
		p.Steps[PhaseData] = data
	}
	return p, pl, nil
}

// applied are the phases whose steps change the machine: all but the files shown for review.
func (p *Plan) applied() []Step {
	var out []Step
	for _, ph := range Phases {
		if ph.Phase != PhaseReview {
			out = append(out, p.Steps[ph.Phase]...)
		}
	}
	return out
}

// Fingerprint identifies what applying the plan would change: the snapshot, and each step with
// where it goes and how (its diff included). Two plans with the same fingerprint do the same.
// It's made from file contents, so it's kept where only root reads it.
func (p *Plan) Fingerprint() string {
	h := sha256.New()
	if p.Snapshot != nil {
		fmt.Fprintf(h, "snapshot %s\n", p.Snapshot.ID)
	}
	for _, ph := range Phases {
		for _, s := range p.Steps[ph.Phase] {
			if ph.Phase == PhaseReview {
				continue
			}
			user := ""
			if s.User != nil {
				user = s.User.Name
			}
			d := sha256.Sum256([]byte(s.Diff))
			fmt.Fprintf(h, "%s\x00%s\x00%s\x00%s\x00%s\x00%s\x00%v\x00%x\n",
				ph.Phase, choiceKey(s.Item), user, s.Target, s.State, s.Item.SHA, s.Adapted, d)
		}
	}
	return hex.EncodeToString(h.Sum(nil))
}

// Changes says in short plain lines what applying the plan would change: places and counts,
// never contents, since offers are world-readable. At most max lines; the last says how many
// more there are.
func (p *Plan) Changes(max int) []string {
	var lines []string
	files := map[string][]Step{} // system files by folder
	var dirs []string
	for _, s := range p.Steps[PhaseFiles] {
		d := path.Dir(s.Item.ID)
		if _, ok := files[d]; !ok {
			dirs = append(dirs, d)
		}
		files[d] = append(files[d], s)
	}
	for _, d := range dirs {
		group := files[d]
		switch {
		case len(group) > 1:
			lines = append(lines, fmt.Sprintf("%d system files in %s", len(group), d))
		case group[0].State == StateNew:
			lines = append(lines, "adds "+group[0].Item.ID)
		default:
			lines = append(lines, "replaces "+group[0].Item.ID)
		}
	}
	for _, s := range p.Steps[PhaseData] {
		lines = append(lines, "restores "+s.Item.HomePath()+forUser(s))
	}
	for _, s := range p.Steps[PhaseAccount] {
		name := s.User.Name
		switch s.Item.Type {
		case record.TypeGroup:
			lines = append(lines, fmt.Sprintf("adds %s to the group %s", name, s.Item.ID))
		case record.TypeShell:
			lines = append(lines, fmt.Sprintf("sets %s's login shell to %s", name, s.Item.ID))
		case record.TypeLinger:
			lines = append(lines, fmt.Sprintf("keeps %s's user services running after they sign out", name))
		}
	}
	for _, s := range p.Steps[PhaseServices] {
		verb := "enables "
		if s.Item.Type == record.TypeMask {
			verb = "masks "
		}
		lines = append(lines, verb+s.Item.ID)
	}
	for _, s := range p.Steps[PhaseDirs] {
		lines = append(lines, "creates the folder "+homePath(s))
	}
	for _, s := range p.Steps[PhaseUserServices] {
		switch s.Item.Type {
		case record.TypeUsvc:
			lines = append(lines, "enables the user unit "+s.Item.ID+forUser(s))
		case record.TypeUmask:
			lines = append(lines, "masks the user unit "+s.Item.ID+forUser(s))
		case record.TypeUunit:
			if s.State == StateNew {
				lines = append(lines, "adds the user unit file "+s.Item.ID+forUser(s))
			} else {
				lines = append(lines, "replaces the user unit file "+s.Item.ID+forUser(s))
			}
		}
	}
	for _, s := range p.Steps[PhaseBoot] {
		lines = append(lines, "adds the kernel parameter "+s.Item.ID)
	}
	if max > 0 && len(lines) > max {
		more := len(lines) - (max - 1)
		lines = append(lines[:max-1], fmt.Sprintf("and %d more changes", more))
	}
	return lines
}

// forUser names the user here a per-user step is for.
func forUser(s Step) string {
	if s.User == nil {
		return ""
	}
	return " (" + s.User.Name + ")"
}

// homePath shows a step's target in its user's home as ~/...
func homePath(s Step) string {
	if s.User != nil {
		if rel, ok := strings.CutPrefix(s.Target, strings.TrimSuffix(s.User.Home, "/")+"/"); ok {
			return "~/" + rel + forUser(s)
		}
	}
	return s.Target
}

// offerPrint is what is kept of an offer, root-only: the fingerprint of its plan.
type offerPrint struct {
	Fingerprint string    `json:"fingerprint"`
	Offered     time.Time `json:"offered"`
}

func offersPath(stateDir string) string { return filepath.Join(KeptDir(stateDir), offersFile) }

// readOffers reads the kept fingerprints. A missing or damaged file has none.
func readOffers(stateDir string) map[string]offerPrint {
	out := map[string]offerPrint{}
	data, err := os.ReadFile(offersPath(stateDir))
	if err == nil {
		_ = json.Unmarshal(data, &out) // damaged: offers are made again
	}
	return out
}

// changeOffers changes the kept fingerprints.
func changeOffers(stateDir string, change func(map[string]offerPrint)) error {
	o := readOffers(stateDir)
	change(o)
	if len(o) == 0 {
		if err := os.Remove(offersPath(stateDir)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		return nil
	}
	data, err := json.MarshalIndent(o, "", "  ")
	if err != nil {
		return err
	}
	return writePrivate(offersPath(stateDir), append(data, '\n'))
}

// OfferOptions say what the pacman hook just did and how to look at this machine.
type OfferOptions struct {
	Dirs paths.Dirs
	Here *Machine
	// Installed are the packages the transaction installed.
	Installed []string
	// Facts returns this machine's facts, for bundles whose when-rule needs them. It's only called
	// then. Nil leaves such bundles out.
	Facts func() (rules.Facts, error)
	// Wait is how long to wait for the inbox lock: the hook must not hold pacman up.
	Wait     time.Duration
	Now      time.Time
	Location *time.Location // for the dates in hints; nil means the local one
}

// Offer offers to restore the saved settings of the apps just installed: for each app bundle of
// the kept source holding one of the packages, with something to change here and not restored,
// ignored or offered already, it adds a restore entry to the inbox. It returns a hint for each new
// offer, for pacman's output. With no kept source it does nothing.
func Offer(ctx context.Context, opt OfferOptions) ([]string, error) {
	if len(opt.Installed) == 0 || !HasKept(opt.Dirs.State) {
		return nil, nil
	}
	src, kept, err := OpenKept(opt.Dirs.State)
	if err != nil {
		return nil, err
	}
	in, err := state.ReadInbox(opt.Dirs)
	if err != nil && !errors.Is(err, state.ErrInboxDamaged) {
		return nil, err
	}
	installed := map[string]bool{}
	for _, p := range opt.Installed {
		installed[p] = true
	}
	app := map[string]string{} // bundle -> the package installed that brought it
	for _, it := range src.Record.Items {
		if (it.Type != record.TypePkg && it.Type != record.TypeAUR) || !installed[it.ID] || app[it.Bundle] != "" {
			continue
		}
		b := it.Bundle
		_, has := in.Find(state.EntryID(state.KindRestore, b))
		if !bundle.IsApp(b) || has || slices.Contains(in.IgnoredBundles, b) || slices.Contains(kept.Restored, b) {
			continue
		}
		app[b] = it.ID
	}
	if len(app) == 0 {
		return nil, nil
	}
	var facts *rules.Facts
	var offers []state.Entry
	prints := map[string]offerPrint{}
	for _, b := range src.Record.Bundles { // restore order
		pkg := app[b.ID]
		if pkg == "" || ctx.Err() != nil {
			continue
		}
		if w := strings.TrimSpace(b.When); w != "" && w != "always" {
			if facts == nil {
				if opt.Facts == nil {
					continue
				}
				f, err := opt.Facts()
				if err != nil {
					return nil, err
				}
				facts = &f
			}
			if on, _, _ := rules.Evaluate(b.When, *facts, src.Record.Meta); !on {
				continue
			}
		}
		p, _, err := PlanOffer(ctx, src, opt.Here, b.ID)
		if err != nil {
			return nil, err
		}
		changes := p.Changes(MaxChanges)
		if len(changes) == 0 {
			continue // this machine has its settings already
		}
		e := state.NewEntry(state.KindRestore, b.ID)
		e.App, e.By, e.Found, e.Changes = pkg, state.ByHook, opt.Now, changes
		offers = append(offers, e)
		prints[b.ID] = offerPrint{Fingerprint: p.Fingerprint(), Offered: opt.Now}
	}
	if len(offers) == 0 {
		return nil, ctx.Err()
	}
	// The fingerprints first: an entry without one can't be applied, only offered again.
	if err := changeOffers(opt.Dirs.State, func(o map[string]offerPrint) {
		for b, p := range prints {
			o[b] = p
		}
	}); err != nil {
		return nil, err
	}
	var added []state.Entry
	if _, err := state.UpdateInbox(opt.Dirs, opt.Wait, func(in *state.Inbox) error {
		added = nil
		for _, e := range offers {
			if _, has := in.Find(e.ID); has || slices.Contains(in.IgnoredBundles, e.Bundle) {
				continue
			}
			in.Put(e)
			added = append(added, e)
		}
		return nil
	}); err != nil {
		return nil, err
	}
	loc := opt.Location
	if loc == nil {
		loc = time.Local
	}
	var hints []string
	for _, e := range added {
		hints = append(hints, fmt.Sprintf("carryon: saved settings for %s (%s) — restore with: carryon restore --bundle %s",
			e.App, kept.Snapshot.Time.In(loc).Format("2 Jan"), e.Bundle))
	}
	return hints, nil
}

// ErrNoOffer means a bundle wasn't offered, so carryon-restore@.service won't restore it.
var ErrNoOffer = errors.New("no restore was offered for this bundle")

// ErrOfferChanged means the plan differs from what was offered. The offer was renewed.
var ErrOfferChanged = errors.New("the plan changed since it was offered")

// CheckOffer checks that p is the plan offered for bundleID: the inbox holds the offer and the
// fingerprint kept when it was made matches. When the plan changed, the offer is renewed with
// p's changes and ErrOfferChanged returned, so the user sees the new plan before anything is
// applied.
func CheckOffer(dirs paths.Dirs, bundleID string, p *Plan, now time.Time) error {
	id := state.EntryID(state.KindRestore, bundleID)
	in, err := state.ReadInbox(dirs)
	if err != nil {
		return err
	}
	e, ok := in.Find(id)
	have, printed := readOffers(dirs.State)[bundleID]
	if !ok || !printed {
		return fmt.Errorf("%w: see what it would change with: sudo carryon restore --bundle %s --plan", ErrNoOffer, bundleID)
	}
	fp := p.Fingerprint()
	if fp == have.Fingerprint {
		return nil
	}
	if err := changeOffers(dirs.State, func(o map[string]offerPrint) {
		o[bundleID] = offerPrint{Fingerprint: fp, Offered: now}
	}); err != nil {
		return err
	}
	e.Changes = p.Changes(MaxChanges)
	if _, err := state.UpdateInbox(dirs, state.InboxWait, func(in *state.Inbox) error {
		if _, ok := in.Find(id); ok {
			in.Put(e)
		}
		return nil
	}); err != nil {
		return err
	}
	return ErrOfferChanged
}

// SettleOffer notes that bundleID was restored from the kept source: it isn't offered again, and
// its offer leaves the inbox.
func SettleOffer(dirs paths.Dirs, bundleID string) error {
	if err := MarkRestored(dirs.State, []string{bundleID}); err != nil {
		return err
	}
	_, err := state.UpdateInbox(dirs, state.InboxWait, func(in *state.Inbox) error {
		in.Remove(state.EntryID(state.KindRestore, bundleID))
		return nil
	})
	return err
}

// CloseOffers takes restore offers out of the inbox: those of bundles, or every one when all is
// set, such as when no source is kept any more or another one is.
func CloseOffers(dirs paths.Dirs, bundles []string, all bool) error {
	if !all && len(bundles) == 0 {
		return nil
	}
	_, err := state.UpdateInbox(dirs, state.InboxWait, func(in *state.Inbox) error {
		keep := []state.Entry{}
		for _, e := range in.Entries {
			if e.Kind != state.KindRestore || !all && !slices.Contains(bundles, e.Bundle) {
				keep = append(keep, e)
			}
		}
		in.Entries = keep
		return nil
	})
	return err
}
