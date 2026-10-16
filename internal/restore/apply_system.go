package restore

import (
	"context"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"github.com/Evimore/carryon/internal/capture"
	"github.com/Evimore/carryon/internal/pkgmgr"
	"github.com/Evimore/carryon/internal/record"
	"github.com/Evimore/carryon/internal/sysexec"
)

// This file holds the phases after the data: accounts, services, folders, user units, kernel
// parameters and the initramfs, and language tools.

// account adds the users to their groups (creating missing groups), sets login shells and turns
// on lingering.
func (a *Applier) account(ctx context.Context, steps []Step) {
	if len(steps) == 0 {
		return
	}
	a.Log.Info("account")
	groups := map[string][]string{}
	var order []string
	for _, s := range steps {
		if s.Item.Type != record.TypeGroup || s.User == nil {
			continue
		}
		if _, ok := groups[s.User.Name]; !ok {
			order = append(order, s.User.Name)
		}
		if !slices.Contains(groups[s.User.Name], s.Item.ID) {
			groups[s.User.Name] = append(groups[s.User.Name], s.Item.ID)
		}
	}
	created := map[string]bool{}
	for _, user := range order {
		for _, g := range groups[user] {
			if _, ok := a.ids.groups[g]; ok || created[g] {
				continue
			}
			if err := a.run(ctx, "groupadd", "-r", g); err != nil {
				a.problem("couldn't create group %s: try: sudo groupadd -r %s", g, g)
			}
			created[g] = true
		}
		gs := strings.Join(groups[user], ",")
		if err := a.run(ctx, "usermod", "-aG", gs, user); err != nil {
			a.problem("couldn't add %s to groups %s: try: sudo usermod -aG %s %s", user, strings.ReplaceAll(gs, ",", ", "), gs, user)
		}
	}
	for _, s := range steps {
		if s.User == nil {
			continue
		}
		user := s.User.Name
		switch s.Item.Type {
		case record.TypeShell:
			if _, err := os.Stat(a.path(s.Item.ID)); err != nil {
				a.problem("login shell %s isn't installed, so %s keeps their current one: install it, then run: sudo usermod -s %s %s", s.Item.ID, user, s.Item.ID, user)
				continue
			}
			if err := a.run(ctx, "usermod", "-s", s.Item.ID, user); err != nil {
				a.problem("couldn't set %s's login shell: try: sudo usermod -s %s %s", user, s.Item.ID, user)
			}
		case record.TypeLinger:
			if err := a.run(ctx, "loginctl", "enable-linger", user); err != nil {
				a.problem("couldn't let %s's services run while they're signed out: try: sudo loginctl enable-linger %s", user, user)
			}
		}
	}
}

// services enables and masks system units.
func (a *Applier) services(ctx context.Context, steps []Step) {
	if len(steps) == 0 {
		return
	}
	a.Log.Info("system services")
	if err := a.run(ctx, "systemctl", "daemon-reload"); err != nil {
		a.Log.Warn("%v", err)
	}
	for _, v := range []struct{ verb, typ string }{{"enable", record.TypeSvc}, {"mask", record.TypeMask}} {
		for _, s := range steps {
			if s.Item.Type != v.typ {
				continue
			}
			if err := a.run(ctx, "systemctl", v.verb, "--", s.Item.ID); err != nil {
				a.problem("couldn't %s %s (its package may not be installed): try: sudo systemctl %s %s", v.verb, s.Item.ID, v.verb, s.Item.ID)
			}
		}
	}
}

// dirs makes the folders units need. Folders in a home are handed to their user.
func (a *Applier) dirs(steps []Step) {
	for _, s := range steps {
		dir := s.Target
		if dir == "" {
			dir = s.Item.ID
		}
		if !cleanAbs(dir) {
			a.problem("folder %s wasn't made: it isn't a full path", dir)
			continue
		}
		var err error
		if s.Item.Rel != "" && s.User != nil {
			err = a.mkdirHome(*s.User, dir)
		} else {
			err = os.MkdirAll(a.path(dir), 0o755)
		}
		if err != nil {
			a.problem("couldn't make folder %s: %v", dir, err)
		}
	}
}

// mkdirHome makes a folder in the user's home, handing each folder it makes to the user. It never
// follows a symlink out of the home.
func (a *Applier) mkdirHome(u LocalUser, dir string) error {
	if !within(dir, u.Home) {
		return fmt.Errorf("%s isn't in %s's home", dir, u.Name)
	}
	sys, err := a.Here.resolveDir(dir)
	if err != nil {
		return err
	}
	home, err := a.Here.resolveDir(u.Home)
	if err != nil {
		return err
	}
	if !within(sys, home) {
		return fmt.Errorf("a link in %s points outside %s's home", dir, u.Name)
	}
	rel, _ := filepath.Rel(home, sys)
	cur := home
	if _, err := os.Lstat(a.path(home)); err != nil {
		if err := os.MkdirAll(a.path(home), 0o700); err != nil {
			return err
		}
		if err := a.chown(a.path(home), u.UID, u.GID); err != nil {
			return err
		}
	}
	for _, name := range splitPath(rel) {
		cur = path.Join(cur, name)
		p := a.path(cur)
		if _, err := os.Lstat(p); err == nil {
			continue
		}
		if err := os.Mkdir(p, 0o755); err != nil {
			return err
		}
		if err := a.chown(p, u.UID, u.GID); err != nil {
			return err
		}
	}
	return nil
}

// homeTarget returns where a path in the user's home is really written, refusing one that a
// symlink takes out of the home.
func (a *Applier) homeTarget(u LocalUser, abs string) (string, error) {
	sys, err := a.Here.resolve(abs)
	if err != nil {
		return "", err
	}
	home, err := a.Here.resolveDir(u.Home)
	if err != nil {
		return "", err
	}
	if !within(sys, home) || sys == home {
		return "", fmt.Errorf("a link in %s points outside %s's home", abs, u.Name)
	}
	return sys, nil
}

// userUnits writes the users' unit files, masks and enables their units. Units are enabled as the
// user, through their session; for a user who isn't signed in, the command to run is listed.
func (a *Applier) userUnits(ctx context.Context, steps []Step) {
	if len(steps) == 0 {
		return
	}
	a.Log.Info("user units")
	byUser := map[string][]Step{}
	var order []LocalUser
	for _, s := range steps {
		if s.User == nil {
			continue
		}
		if _, ok := byUser[s.User.Name]; !ok {
			order = append(order, *s.User)
		}
		byUser[s.User.Name] = append(byUser[s.User.Name], s)
	}
	for _, u := range order {
		a.userUnitsOf(ctx, u, byUser[u.Name])
	}
}

func (a *Applier) userUnitsOf(ctx context.Context, u LocalUser, steps []Step) {
	udir := path.Join(u.Home, ".config/systemd/user")
	var units []string
	for _, s := range steps {
		it := s.Item
		if it.Type == record.TypeUsvc {
			units = append(units, it.ID)
			continue
		}
		if !cleanAbs("/"+it.ID) || strings.Contains(it.ID, "..") {
			a.problem("user unit %s wasn't written: its name isn't a unit name", it.ID)
			continue
		}
		target := path.Join(udir, it.ID)
		if err := a.writeUserUnit(u, it, target); err != nil {
			a.problem("couldn't write %s for %s: %v", it.ID, u.Name, err)
		}
	}
	if len(units) == 0 {
		return
	}
	cmd := "systemctl --user enable " + strings.Join(units, " ")
	bus := fmt.Sprintf("/run/user/%d/bus", u.UID)
	if _, err := os.Stat(a.path(bus)); err != nil {
		a.problem("%s wasn't signed in, so their units weren't enabled: after they sign in, they run: %s", u.Name, cmd)
		return
	}
	ur := a.userRunner(u)
	if _, err := ur.Output(ctx, "systemctl", "--user", "daemon-reload"); err != nil {
		a.Log.Warn("%v", err)
	}
	if err := ur.Run(ctx, "systemctl", append([]string{"--user", "enable"}, units...)...); err != nil {
		a.problem("couldn't enable %s's user units %s: as %s, run: %s", u.Name, strings.Join(units, ", "), u.Name, cmd)
	}
}

// writeUserUnit writes a unit file (adapted for this machine's users) or a mask, owned by the user.
func (a *Applier) writeUserUnit(u LocalUser, it record.Item, target string) error {
	if err := a.mkdirHome(u, path.Dir(target)); err != nil {
		return err
	}
	sys, err := a.homeTarget(u, target)
	if err != nil {
		return err
	}
	dst := a.path(sys)
	if it.Type == record.TypeUmask {
		if err := removeNonDir(dst); err != nil {
			return err
		}
		if err := os.Symlink("/dev/null", dst); err != nil {
			return err
		}
		return a.chown(dst, u.UID, u.GID)
	}
	src := filepath.Join(a.Source.Dir, UserUnitsDir, it.ID)
	if it.User != "" {
		src = filepath.Join(a.Source.Dir, UserUnitsDir, it.User, it.ID)
	}
	raw, err := os.ReadFile(src)
	if err != nil {
		return fmt.Errorf("the backup's copy can't be read: %w", err)
	}
	if err := removeNonDir(dst); err != nil {
		return err
	}
	if err := writeNew(dst, a.pl.adapt.Bytes(raw), 0o644); err != nil {
		return err
	}
	return a.chown(dst, u.UID, u.GID)
}

// removeNonDir removes a file or symlink at the root path p, if one is there.
func removeNonDir(p string) error {
	fi, err := os.Lstat(p)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if fi.IsDir() {
		return fmt.Errorf("%s is a folder", p)
	}
	return os.Remove(p)
}

// cmdlineSource returns the bootloader config holding the kernel command line here, or nil.
func (a *Applier) cmdlineSource() *capture.CmdlineSource {
	for i, s := range capture.CmdlineSources {
		if fi, err := os.Stat(a.path(s.Path)); err == nil && fi.Mode().IsRegular() {
			return &capture.CmdlineSources[i]
		}
	}
	return nil
}

// boot adds kernel parameters to the bootloader config, keeping the old one as *.pre-restore.
func (a *Applier) boot(steps []Step) {
	if len(steps) == 0 {
		return
	}
	add := stepNames(steps)
	params := strings.Join(add, " ")
	src := a.cmdlineSource()
	if src == nil {
		a.problem("no supported bootloader config was found: add kernel parameters %s by hand", params)
		return
	}
	a.Log.Info("adding kernel parameters to %s: %s", src.Path, params)
	p := a.path(src.Path)
	data, err := os.ReadFile(p)
	if err != nil {
		a.problem("couldn't read %s: add kernel parameters %s by hand", src.Path, params)
		return
	}
	loc := src.Pattern.FindSubmatchIndex(data)
	if loc == nil {
		a.problem("couldn't find the kernel command line in %s: add %s by hand", src.Path, params)
		return
	}
	line := strings.TrimSpace(string(data[loc[4]:loc[5]]) + " " + params)
	out := append(append(append([]byte{}, data[:loc[0]]...), data[loc[2]:loc[3]]...), line...)
	out = append(out, data[loc[1]:]...)
	if err := copyOld(p); err != nil {
		a.problem("couldn't keep a copy of %s, so it wasn't changed: add %s by hand", src.Path, params)
		return
	}
	if err := replaceFile(p, out); err != nil {
		a.problem("couldn't write %s: add %s by hand", src.Path, params)
		return
	}
	a.regen = true
}

// regenerate makes the initramfs and boot entries again after initramfs settings, modules,
// firmware or kernel parameters changed.
func (a *Applier) regenerate(ctx context.Context) {
	if !a.regen {
		return
	}
	a.Log.Info("making the initramfs and boot entries again")
	var regen []string
	if src := a.cmdlineSource(); src != nil {
		regen = src.Apply
	}
	steps := [][]string{{"mkinitcpio", "-P"}}
	switch {
	case len(regen) > 0 && regen[0] == "limine-update":
		steps = [][]string{regen}
	case len(regen) > 0 && regen[0] != "mkinitcpio":
		steps = append(steps, regen)
	}
	ran := false
	for _, st := range steps {
		if !a.Here.Run.Exists(st[0]) {
			continue
		}
		ran = true
		if err := a.run(ctx, st[0], st[1:]...); err != nil {
			a.problem("`%s` failed: fix this before restarting the computer", sysexec.Join(st))
		}
	}
	if !ran {
		a.problem("the initramfs needs making again, but %s isn't installed: make it again with your system's tool before restarting the computer", steps[0][0])
	}
}

// flathubURL is the remote Flatpak apps come from by default.
const flathubURL = "https://dl.flathub.org/repo/flathub.flatpakrepo"

// tools installs cargo, pipx, npm and Flatpak installs. Per-user ones run as their user.
func (a *Applier) tools(ctx context.Context, steps []Step) {
	if len(steps) == 0 {
		return
	}
	a.Log.Info("language tools")
	has := func(typ string, user bool) bool {
		return slices.ContainsFunc(steps, func(s Step) bool {
			return s.Item.Type == typ && (!user || s.Item.Scope == "user")
		})
	}
	if has(record.TypeCargo, false) && a.Here.Run.Exists("rustup") {
		seen := map[string]bool{}
		for _, s := range steps {
			if s.Item.Type != record.TypeCargo || s.User == nil || seen[s.User.Name] {
				continue
			}
			seen[s.User.Name] = true
			ur := a.userRunner(*s.User)
			if out, _ := ur.Output(ctx, "rustup", "default"); strings.TrimSpace(string(out)) == "" {
				if err := ur.Run(ctx, "rustup", "default", "stable"); err != nil {
					a.Log.Warn("%v", err)
				}
			}
		}
	}
	if has(record.TypeFlatpak, false) {
		if err := a.Packages.Install(ctx, []string{"flatpak"}, pkgmgr.InstallOptions{NoConfirm: a.Yes}); err != nil {
			a.Log.Warn("%v", err)
		}
		if err := a.run(ctx, "flatpak", "remote-add", "--if-not-exists", "flathub", flathubURL); err != nil {
			a.Log.Warn("%v", err)
		}
		seen := map[string]bool{}
		for _, s := range steps {
			if s.Item.Type != record.TypeFlatpak || s.Item.Scope != "user" || s.User == nil || seen[s.User.Name] {
				continue
			}
			seen[s.User.Name] = true
			if err := a.userRunner(*s.User).Run(ctx, "flatpak", "remote-add", "--user", "--if-not-exists", "flathub", flathubURL); err != nil {
				a.Log.Warn("%v", err)
			}
		}
	}
	for _, s := range steps {
		it := s.Item
		var err error
		var cmd []string
		r := a.Here.Run
		switch it.Type {
		case record.TypeCargo:
			cmd = []string{"cargo", "install", "--locked", it.ID}
		case record.TypePipx:
			cmd = []string{"pipx", "install", it.ID}
		case record.TypeNpm:
			cmd = []string{"npm", "install", "-g", it.ID}
		case record.TypeFlatpak:
			origin := it.Origin
			if origin == "" {
				origin = "flathub"
			}
			cmd = []string{"flatpak", "install", "-y"}
			if it.Scope == "user" {
				cmd = append(cmd, "--user")
			}
			cmd = append(cmd, origin, it.ID)
		default:
			continue
		}
		perUser := it.Type == record.TypeCargo || it.Type == record.TypePipx || (it.Type == record.TypeFlatpak && it.Scope == "user")
		who := ""
		if perUser {
			if s.User == nil {
				continue
			}
			r, who = a.userRunner(*s.User), " as "+s.User.Name
		}
		if err = r.Run(ctx, cmd[0], cmd[1:]...); err != nil {
			a.problem("%s %s couldn't be installed: see the messages above, then try%s: %s", it.Type, it.ID, who, sysexec.Join(cmd))
		}
	}
}

// saveNotBackedUp copies the backup's list of what to re-download into the main user's home.
func (a *Applier) saveNotBackedUp() string {
	data, err := os.ReadFile(filepath.Join(a.Source.Dir, NotBackedUpMD))
	if err != nil {
		return ""
	}
	u := a.mainUser()
	if u == nil || u.Home == "" {
		return ""
	}
	target := path.Join(u.Home, NotBackedUpMD)
	sys, err := a.homeTarget(*u, target)
	if err == nil {
		if err = removeNonDir(a.path(sys)); err == nil {
			if err = writeNew(a.path(sys), data, 0o644); err == nil {
				err = a.chown(a.path(sys), u.UID, u.GID)
			}
		}
	}
	if err != nil {
		a.Log.Warn("couldn't save the list of what to re-download to %s: %v", target, err)
		return ""
	}
	return target
}
