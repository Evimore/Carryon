package restore

import (
	"context"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"github.com/Evimore/carryon/internal/capture"
	"github.com/Evimore/carryon/internal/catalog"
	"github.com/Evimore/carryon/internal/pkgmgr"
	"github.com/Evimore/carryon/internal/record"
	"github.com/Evimore/carryon/internal/sysexec"
	"github.com/Evimore/carryon/internal/ui"
)

// Installer is what applying a plan needs from the package manager. pkgmgr.Manager provides it.
type Installer interface {
	// Install installs packages in one run, attached to the terminal.
	Install(ctx context.Context, pkgs []string, opt pkgmgr.InstallOptions) error
	// InstallEach installs packages one at a time and returns the ones that failed.
	InstallEach(ctx context.Context, pkgs []string, asDeps bool) []string
	// TrustKeys imports an armored key file and trusts the given keys, returning those it couldn't.
	TrustKeys(ctx context.Context, keyFile string, fingerprints []string) ([]string, error)
}

// Applier applies a plan to this machine, phase by phase, in the order of Phases. A step that
// fails is reported and the rest still applies. Ported from apply_plan.
//
// Everything it changes goes through Here: files under Here.Root, programs through Here.Run (as
// root) or the user's runner (AUR builds, user units, cargo, pipx, a user's Flatpak apps).
type Applier struct {
	Source   *Source
	Here     *Machine
	Packages Installer
	// Repo is the backup, for the data phase. Nil makes that phase fail with a note.
	Repo Repo
	// Ask asks before installing packages one by one and before restoring home files while their
	// user is signed in to a desktop. ui.Defaults answers for runs without a terminal.
	Ask ui.Prompter
	Log *ui.Logger
	// Out receives what the user must read: files to review by hand.
	Out io.Writer
	// Yes lets pacman and the AUR helper go ahead without asking (--yes).
	Yes bool
	// Catalog has the notes shown after a restore. Nil means none.
	Catalog *catalog.Catalog
	// Chown changes the owner of a path without following symlinks. Nil means os.Lchown.
	Chown func(path string, uid, gid int) error
	// TempDir is where data from the backup is put before it's copied into homes. Empty means
	// /var/tmp, or the system's temp folder.
	TempDir string

	plan     *Plan
	pl       *Planner // for the backup's copies of files, adapted, and the boot-risk check
	ids      ids
	problems []string
	regen    bool // the initramfs and boot entries need making again
}

// Result is what applying a plan did.
type Result struct {
	// Problems are the steps that failed, each saying what to do. Empty means everything applied.
	Problems []string
	// Notes are the catalog's notes for what was restored, such as KDE Wallet's password.
	Notes []catalog.AppNote
	// NotBackedUp is where the list of what to re-download was saved, or "".
	NotBackedUp string
}

// Apply applies every step of the plan. It never returns early: each failure becomes a problem in
// the result, and the steps after it still run.
func (a *Applier) Apply(ctx context.Context, p *Plan) *Result {
	a.plan, a.problems, a.regen = p, nil, false
	a.pl = &Planner{Source: a.Source, Here: a.Here, Users: p.Users, adapt: NewAdapter(p.Users)}
	a.ids = readIDs(a.Here.root())
	if a.Log == nil {
		a.Log = ui.NewLogger(io.Discard, false, false)
	}
	if a.Out == nil {
		a.Out = io.Discard
	}
	if a.Ask == nil {
		a.Ask = ui.Defaults{}
	}
	res := &Result{}
	steps := p.Steps
	a.repos(ctx, steps[PhaseRepos])
	if len(steps[PhaseRepos])+len(steps[PhasePackages])+len(steps[PhaseAUR]) > 0 {
		a.packages(ctx, steps[PhasePackages])
	}
	a.aur(ctx, steps[PhaseAUR])
	a.files(ctx, steps[PhaseFiles])
	a.review(steps[PhaseReview])
	restored := a.data(ctx, steps[PhaseData])
	a.account(ctx, steps[PhaseAccount])
	a.services(ctx, steps[PhaseServices])
	a.dirs(steps[PhaseDirs])
	a.userUnits(ctx, steps[PhaseUserServices])
	a.boot(steps[PhaseBoot])
	a.regenerate(ctx)
	a.tools(ctx, steps[PhaseTools])
	res.NotBackedUp = a.saveNotBackedUp()
	if a.Catalog != nil && len(restored) > 0 {
		res.Notes = a.Catalog.NotesFor(restored)
	}
	res.Problems = a.problems
	return res
}

// problem records a step that failed, and says so right away.
func (a *Applier) problem(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	a.Log.Warn("%s", msg)
	a.problems = append(a.problems, msg)
}

func (a *Applier) path(abs string) string { return a.Here.path(abs) }

func (a *Applier) chown(p string, uid, gid int) error {
	if a.Chown != nil {
		return a.Chown(p, uid, gid)
	}
	return os.Lchown(p, uid, gid)
}

func (a *Applier) run(ctx context.Context, name string, args ...string) error {
	return a.Here.Run.Run(ctx, name, args...)
}

// noConfirm is pacman's and the AUR helper's --noconfirm, with --yes.
func (a *Applier) noConfirm() []string {
	if a.Yes {
		return []string{"--noconfirm"}
	}
	return nil
}

// mainUser is who builds AUR packages and gets the list of what to re-download: the user who
// receives the record's main user's files, else the first user mapped.
func (a *Applier) mainUser() *LocalUser {
	for i, m := range a.plan.Users {
		if m.From.Name == a.plan.mainUser {
			return &a.plan.Users[i].To
		}
	}
	if len(a.plan.Users) > 0 {
		return &a.plan.Users[0].To
	}
	return nil
}

func (a *Applier) userRunner(u LocalUser) sysexec.Runner { return a.Here.userRunner(u) }

// repos adds the extra package repositories to pacman's config, keeping the old config as
// pacman.conf.pre-restore, and trusts their signing keys.
func (a *Applier) repos(ctx context.Context, steps []Step) {
	if len(steps) == 0 {
		return
	}
	a.Log.Info("adding %d package repositories", len(steps))
	conf := a.path("/etc/pacman.conf")
	data, err := os.ReadFile(conf)
	if err != nil {
		a.problem("couldn't read /etc/pacman.conf, so repositories %s weren't added: %v", stepIDs(steps), err)
		return
	}
	text := string(data)
	for _, s := range steps {
		it := s.Item
		if it.Section == "" {
			a.problem("repository [%s] has no settings in the backup: add it to /etc/pacman.conf by hand", it.ID)
			continue
		}
		if it.Conf == "" {
			if !strings.Contains(text, "["+it.ID+"]") {
				text = strings.TrimRight(text, "\n") + "\n\n" + it.Section
			}
			continue
		}
		if !cleanAbs(it.Conf) || !within(it.Conf, "/etc") {
			a.problem("repository [%s] is kept in %s, outside /etc: add it to /etc/pacman.conf by hand", it.ID, it.Conf)
			continue
		}
		if err := a.addSection(it); err != nil {
			a.problem("couldn't add repository [%s] to %s: %v", it.ID, it.Conf, err)
			continue
		}
		if !hasInclude(text, it.Conf) {
			text = strings.TrimRight(text, "\n") + "\n\nInclude = " + it.Conf + "\n"
		}
	}
	if text != string(data) {
		if err := copyOld(conf); err != nil {
			a.problem("couldn't keep a copy of /etc/pacman.conf, so it wasn't changed: %v", err)
			return
		}
		if err := replaceFile(conf, []byte(text)); err != nil {
			a.problem("couldn't write /etc/pacman.conf: %v", err)
			return
		}
	}
	var keys []string
	for _, s := range steps {
		for _, k := range s.Item.Keys {
			if !slices.Contains(keys, k) {
				keys = append(keys, k)
			}
		}
	}
	slices.Sort(keys)
	if len(keys) == 0 {
		return
	}
	keyFile := filepath.Join(a.Source.Dir, capture.RepoKeysFile)
	if _, err := os.Stat(keyFile); err != nil {
		a.problem("the backup has no copy of the signing keys %s: import them by hand (sudo pacman-key --recv-keys KEY, then --lsign-key KEY)", strings.Join(keys, ", "))
		return
	}
	failed, err := a.Packages.TrustKeys(ctx, keyFile, keys)
	if err != nil {
		a.problem("%v", err)
		return
	}
	for _, k := range failed {
		a.problem("couldn't trust signing key %s: check it, then run: sudo pacman-key --lsign-key %s", k, k)
	}
}

// hasInclude reports whether pacman's config includes the file conf.
func hasInclude(text, conf string) bool {
	for _, l := range strings.Split(text, "\n") {
		k, v, ok := strings.Cut(l, "=")
		if ok && strings.TrimSpace(k) == "Include" && strings.TrimSpace(v) == conf {
			return true
		}
	}
	return false
}

// addSection appends a repository's section to its own config file, unless it's there already.
func (a *Applier) addSection(it record.Item) error {
	sys, err := a.Here.resolve(it.Conf)
	if err != nil {
		return err
	}
	p := a.path(sys)
	old, err := os.ReadFile(p)
	switch {
	case os.IsNotExist(err):
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return err
		}
		return writeNew(p, []byte(it.Section), 0o644)
	case err != nil:
		return err
	case strings.Contains(string(old), "["+it.ID+"]"):
		return nil
	}
	if err := copyOld(p); err != nil {
		return err
	}
	return replaceFile(p, []byte(strings.TrimRight(string(old), "\n")+"\n\n"+it.Section))
}

func stepIDs(steps []Step) string {
	return record.Brief(stepNames(steps), 8)
}

// packages updates the whole system and installs the packages in one pacman run, because Arch
// only supports installing on an up-to-date system. Packages recorded as dependencies follow in a
// second run. When a run fails, the user can install its packages one at a time instead.
func (a *Applier) packages(ctx context.Context, steps []Step) {
	var pkgs, deps []string
	for _, s := range steps {
		if s.Item.AsDeps {
			deps = append(deps, s.Item.ID)
		} else {
			pkgs = append(pkgs, s.Item.ID)
		}
	}
	a.Log.Info("updating the system and installing %d packages", len(pkgs)+len(deps))
	if err := a.Packages.Install(ctx, pkgs, pkgmgr.InstallOptions{Upgrade: true, NoConfirm: a.Yes}); err != nil {
		a.Log.Warn("%v", err)
		a.oneByOne(ctx, pkgs, false)
	}
	if len(deps) > 0 {
		if err := a.Packages.Install(ctx, deps, pkgmgr.InstallOptions{AsDeps: true, NoConfirm: a.Yes}); err != nil {
			a.Log.Warn("%v", err)
			a.oneByOne(ctx, deps, true)
		}
	}
}

// oneByOne installs packages one at a time, skipping any that fail, if the user agrees. Ported
// from install_one_by_one.
func (a *Applier) oneByOne(ctx context.Context, pkgs []string, asDeps bool) {
	if len(pkgs) == 0 {
		return
	}
	ok, err := a.Ask.Confirm(ui.Confirm{
		Title:   fmt.Sprintf("pacman failed. Install the %d packages one at a time instead, skipping any that fail?", len(pkgs)),
		Default: true,
	})
	if err != nil || !ok {
		a.problem("packages not installed: %s; install them with: sudo pacman -S --needed %s", record.Brief(pkgs, 30), strings.Join(pkgs, " "))
		return
	}
	for _, p := range a.Packages.InstallEach(ctx, pkgs, asDeps) {
		a.problem("package %s couldn't be installed: see pacman's message above, then try: sudo pacman -S %s", p, p)
	}
}

// AUR helpers Carryon builds AUR packages with, in order of preference.
var aurHelpers = []string{"paru", "yay"}

// aurHelperBootstrap is the AUR package built by hand when no helper can be installed otherwise.
const aurHelperBootstrap = "paru-bin"

// aur builds the AUR packages with paru or yay, as the user: makepkg refuses to run as root, and
// a build script must never get root's rights.
func (a *Applier) aur(ctx context.Context, steps []Step) {
	if len(steps) == 0 {
		return
	}
	u := a.mainUser()
	if u == nil || u.UID == 0 {
		a.problem("AUR packages %s weren't built: they're built as a user, never as root, and no user here receives the backup's files", stepIDs(steps))
		return
	}
	helper := a.aurHelper(ctx, *u)
	if helper == "" {
		a.problem("no AUR helper could be installed, so AUR packages %s weren't built: install paru or yay, then run: %s",
			stepIDs(steps), "paru -S "+strings.Join(stepNames(steps), " "))
		return
	}
	a.Log.Info("building %d AUR packages with %s (as %s)", len(steps), helper, u.Name)
	ur := a.userRunner(*u)
	for _, s := range steps {
		id := s.Item.ID
		if isHelper(id, helper) {
			continue // the helper is installed
		}
		args := append([]string{"-S", "--needed"}, a.noConfirm()...)
		if s.Item.AsDeps {
			args = append(args, "--asdeps")
		}
		if err := ur.Run(ctx, helper, append(args, id)...); err != nil {
			a.problem("AUR package %s failed to build: see the messages above, then try as %s: %s -S %s", id, u.Name, helper, id)
		}
	}
}

func stepNames(steps []Step) []string {
	out := make([]string, len(steps))
	for i, s := range steps {
		out[i] = s.Item.ID
	}
	return out
}

// isHelper reports whether an AUR package is the helper itself, in any of its builds.
func isHelper(pkg, helper string) bool {
	return pkg == helper || pkg == helper+"-bin" || pkg == helper+"-git"
}

// aurHelper returns an AUR helper, installing one when none is: from the configured repositories
// (several Arch-based distributions ship paru or yay), else by building paru-bin from the AUR as
// the user. It returns "" when none could be had.
func (a *Applier) aurHelper(ctx context.Context, u LocalUser) string {
	for _, h := range aurHelpers {
		if a.Here.Run.Exists(h) {
			if err := a.Packages.Install(ctx, []string{"base-devel"}, pkgmgr.InstallOptions{NoConfirm: a.Yes}); err != nil {
				a.Log.Warn("%v", err)
			}
			return h
		}
	}
	for _, h := range aurHelpers {
		if a.Packages.Install(ctx, []string{"base-devel", h}, pkgmgr.InstallOptions{NoConfirm: a.Yes}) == nil {
			return h
		}
	}
	a.Log.Info("building %s from the AUR (as %s)", aurHelperBootstrap, u.Name)
	if err := a.Packages.Install(ctx, []string{"base-devel", "git"}, pkgmgr.InstallOptions{NoConfirm: a.Yes}); err != nil {
		a.Log.Warn("%v", err)
		return ""
	}
	base := a.TempDir
	if base == "" {
		base = defaultTempDir()
	}
	work, err := os.MkdirTemp(base, "carryon-aur-")
	if err != nil {
		a.Log.Warn("making a folder to build %s in: %v", aurHelperBootstrap, err)
		return ""
	}
	defer os.RemoveAll(work)
	if err := a.chown(work, u.UID, u.GID); err != nil {
		a.Log.Warn("handing %s to %s: %v", work, u.Name, err)
		return ""
	}
	dir := filepath.Join(work, aurHelperBootstrap)
	ur := a.userRunner(u)
	if err := ur.Run(ctx, "git", "clone", "--depth=1", "https://aur.archlinux.org/"+aurHelperBootstrap+".git", dir); err != nil {
		return ""
	}
	if err := ur.Run(ctx, "sh", "-c", `cd "$1" && makepkg -si --needed --noconfirm`, "sh", dir); err != nil {
		return ""
	}
	return "paru"
}

// files copies the system files from the record. What is there is kept as *.pre-restore.
// Review-only files and initramfs configs that could stop this machine booting are checked again
// here, on the path the file would really be written to, so a changed plan can't copy them.
func (a *Applier) files(ctx context.Context, steps []Step) {
	if len(steps) == 0 {
		return
	}
	a.Log.Info("copying %d system files", len(steps))
	var touched []string
	for _, s := range steps {
		it := s.Item
		sys, why := a.fileTarget(it)
		if why != "" {
			a.problem("%s was not copied: %s", it.ID, why)
			continue
		}
		if err := a.writeFile(it, sys); err != nil {
			a.problem("couldn't write %s: %v", it.ID, err)
			continue
		}
		touched = append(touched, sys)
	}
	run := func(cmd ...string) {
		if err := a.run(ctx, cmd[0], cmd[1:]...); err != nil {
			a.problem("`%s` failed: run it by hand to see why", sysexec.Join(cmd))
		}
	}
	if slices.Contains(touched, "/etc/locale.gen") {
		run("locale-gen")
	}
	if slices.ContainsFunc(touched, func(t string) bool {
		return within(t, "/etc/udev") || within(t, "/usr/lib/udev")
	}) {
		run("udevadm", "control", "--reload")
	}
	if slices.ContainsFunc(touched, func(t string) bool {
		return strings.HasPrefix(t, "/etc/mkinitcpio") || within(t, "/etc/modprobe.d") || within(t, "/usr/lib/firmware")
	}) {
		a.regen = true
	}
}

// fileTarget returns the system path a recorded file would be written to, or why it never is.
func (a *Applier) fileTarget(it record.Item) (string, string) {
	if it.Type != record.TypeFile || !cleanAbs(it.ID) || it.ID == "/" {
		return "", "it isn't a system file the record can hold"
	}
	if it.TooBig {
		return "", "it was too big to keep in the record; copy it from the backup by hand"
	}
	if ReviewOnly(it.ID) {
		return "", reviewOnlyWhy
	}
	sys, err := a.Here.resolve(it.ID)
	if err != nil {
		return "", err.Error()
	}
	if sys != it.ID {
		if _, hit := coversProtected(sys); hit || ReviewOnly(sys) {
			return "", "a link here points it at " + sys + ", which is never copied"
		}
	}
	if why := a.pl.bootRisk(it); why != "" {
		return "", why
	}
	return sys, ""
}

// writeFile writes the backup's copy of a file to the system path sys, adapted for this
// machine's users, with its recorded mode and owner.
func (a *Applier) writeFile(it record.Item, sys string) error {
	dst := a.path(sys)
	var data []byte
	if it.Link == "" {
		var err error
		if data, _, err = a.pl.saved(it); err != nil {
			return fmt.Errorf("the backup's copy can't be read: %w", err)
		}
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	if err := keepOld(dst); err != nil {
		return fmt.Errorf("keeping the old one as %s%s: %w", path.Base(sys), PreRestore, err)
	}
	if it.Link != "" {
		return os.Symlink(a.pl.adapt.String(it.Link), dst)
	}
	if err := writeNew(dst, data, parseMode(it.Mode)); err != nil {
		return err
	}
	owner := it.Owner
	if owner == "" || owner == "root:root" {
		return nil
	}
	uid, gid, ok := a.ids.owner(owner)
	if !ok {
		a.Log.Warn("%s: its owner %s doesn't exist here, so root owns it", it.ID, owner)
		return nil
	}
	return a.chown(dst, uid, gid)
}

// review shows the files that are never copied, with what differs.
func (a *Applier) review(steps []Step) {
	for _, s := range steps {
		fmt.Fprintf(a.Out, "\nReview by hand: %s\n  NOT COPIED: %s\n", s.Item.ID, s.Why)
		switch {
		case s.Diff != "":
			fmt.Fprint(a.Out, s.Diff)
		case s.State == StateNew:
			fmt.Fprintln(a.Out, "  (missing here)")
		}
	}
}
