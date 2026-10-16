package restore

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/Evimore/carryon/internal/engine"
	"github.com/Evimore/carryon/internal/ui"
)

// RcloneConf is where a user's rclone config is, in their home. The one on this machine holds the
// login this restore may be using right now, so it's kept and the backup's other remotes are
// added to it.
const RcloneConf = ".config/rclone/rclone.conf"

// fprintDir is where fprintd keeps enrolled fingerprints, in a folder per user.
const fprintDir = "/var/lib/fprint"

// liveRepo is a repository that can show restic's progress as it restores. *engine.Engine is one.
type liveRepo interface {
	Live(ctx context.Context, stdout io.Writer, args ...string) (engine.Result, error)
}

// restic runs a restic command on the backup, with live output when the repository can show it.
func (a *Applier) restic(ctx context.Context, args ...string) error {
	var res engine.Result
	var err error
	if l, ok := a.Repo.(liveRepo); ok {
		res, err = l.Live(ctx, nil, args...)
	} else {
		res, err = a.Repo.Run(ctx, args...)
	}
	if err != nil {
		return err
	}
	return res.Err()
}

// snapshotID is the snapshot data comes from: the plan's, or for a record on disk the newest
// backup of the machine it describes.
func (a *Applier) snapshotID(ctx context.Context) (string, error) {
	if a.plan.Snapshot != nil {
		return a.plan.Snapshot.ID, nil
	}
	snaps, err := a.Repo.Snapshots(ctx, a.plan.From)
	if err != nil {
		return "", err
	}
	if len(snaps) == 0 {
		return "", fmt.Errorf("the backup has no snapshots of %s", a.plan.From)
	}
	return snaps[len(snaps)-1].ID, nil
}

// data restores paths from the backup: system paths in place, then each user's home paths into a
// temp folder, from where they're copied into the home they go to, adapted and handed to that
// user. It returns the home paths restored (relative to the home), for the catalog's notes.
// Ported from restore_data.
func (a *Applier) data(ctx context.Context, steps []Step) []string {
	if len(steps) == 0 {
		return nil
	}
	var sys []Step
	homes := map[string][]Step{}
	var users []LocalUser
	for _, s := range steps {
		switch {
		case s.Item.Home == "":
			sys = append(sys, s)
		case s.User == nil:
			a.problem("~/%s wasn't restored: nobody here receives its user's files", s.Item.Home)
		default:
			if _, ok := homes[s.User.Name]; !ok {
				users = append(users, *s.User)
			}
			homes[s.User.Name] = append(homes[s.User.Name], s)
		}
	}
	if a.Repo == nil {
		a.problem("the backup isn't open, so %s weren't restored from it: run the restore without --record", stepIDs(steps))
		return nil
	}
	snap, err := a.snapshotID(ctx)
	if err != nil {
		a.problem("nothing was restored from the backup (%s): %v", stepIDs(steps), err)
		return nil
	}
	base := a.TempDir
	if base == "" {
		base = defaultTempDir()
	}
	work, err := os.MkdirTemp(base, "carryon-restore-")
	if err != nil {
		a.problem("nothing was restored from the backup: making a folder to restore into: %v", err)
		return nil
	}
	defer os.RemoveAll(work)
	a.systemData(ctx, snap, work, sys)
	var restored []string
	for _, u := range users {
		restored = append(restored, a.homeData(ctx, snap, work, u, homes[u.Name])...)
	}
	return restored
}

// writeIncludes writes restic's --include-file.
func writeIncludes(file string, paths []string) error {
	return os.WriteFile(file, []byte(strings.Join(paths, "\n")+"\n"), 0o600)
}

// systemData restores system paths in place, then hands folders owned by service users back to
// them by name.
func (a *Applier) systemData(ctx context.Context, snap, work string, steps []Step) {
	var ok []Step
	for _, s := range steps {
		id := s.Item.ID
		if !path.IsAbs(id) || path.Clean(id) != id {
			a.problem("%s wasn't restored: it isn't a full path", id)
			continue
		}
		if q, hit := coversProtected(id); hit {
			a.problem("%s wasn't restored: it would write %s, which is never restored", id, q)
			continue
		}
		ok = append(ok, s)
	}
	if len(ok) == 0 {
		return
	}
	a.Log.Info("restoring %d system paths from the backup", len(ok))
	list := filepath.Join(work, "system.txt")
	if err := writeIncludes(list, stepNames(ok)); err != nil {
		a.problem("system paths weren't restored: %v", err)
		return
	}
	if err := a.restic(ctx, "restore", snap, "--target", a.Here.root(), "--include-file", list); err != nil {
		a.problem("restoring system paths from the backup reported errors: %v", err)
	}
	for _, s := range ok {
		a.remapOwner(s)
	}
	for _, s := range ok {
		if s.Item.ID == fprintDir {
			a.moveFingerprints()
		}
	}
}

// remapOwner hands a restored folder back to its service user by name. restic restores owners by
// number, and a service user (such as one an app's package creates) can have another number
// here. Ported from remap_owner.
func (a *Applier) remapOwner(s Step) {
	it := s.Item
	if it.Owner == "" || it.UID == nil || it.GID == nil || it.Owner == "root:root" {
		return
	}
	root := a.path(it.ID)
	if fi, err := os.Stat(root); err != nil || !fi.IsDir() {
		return
	}
	nu, ng, ok := a.ids.owner(it.Owner)
	if !ok {
		a.problem("%s: its owner %s doesn't exist here; install the app that creates it, then restore again", it.ID, it.Owner)
		return
	}
	if nu == *it.UID && ng == *it.GID {
		return
	}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		uid, gid, ok := ownerOf(p)
		if !ok {
			return nil
		}
		nuid, ngid := -1, -1
		if uid == *it.UID {
			nuid = nu
		}
		if gid == *it.GID {
			ngid = ng
		}
		if nuid == -1 && ngid == -1 {
			return nil
		}
		return a.chown(p, nuid, ngid)
	})
	if err != nil {
		a.problem("%s: couldn't hand it back to %s: %v", it.ID, it.Owner, err)
		return
	}
	a.Log.Info("%s: handed back to %s (user number %d there, %d here)", it.ID, it.Owner, *it.UID, nu)
}

// moveFingerprints gives each renamed user's enrolled fingerprints to the user who receives their
// files, since fprintd keeps them in a folder per user name.
func (a *Applier) moveFingerprints() {
	for _, m := range a.plan.Users {
		if m.From.Name == m.To.Name {
			continue
		}
		old, nw := a.path(path.Join(fprintDir, m.From.Name)), a.path(path.Join(fprintDir, m.To.Name))
		if fi, err := os.Lstat(old); err != nil || !fi.IsDir() {
			continue
		}
		if _, err := os.Lstat(nw); err == nil {
			continue
		}
		if err := os.Rename(old, nw); err != nil {
			a.problem("couldn't move %s's enrolled fingerprints to %s: %v", m.From.Name, m.To.Name, err)
			continue
		}
		a.Log.Info("moved %s's enrolled fingerprints to %s", m.From.Name, m.To.Name)
	}
}

// homeData restores one user's home paths. Nothing is restored while the user has a graphical
// session running, unless they say so: desktops write their settings back when the user signs
// out, undoing part of the restore.
func (a *Applier) homeData(ctx context.Context, snap, work string, u LocalUser, steps []Step) []string {
	if a.graphicalSession(ctx, u.Name) {
		a.Log.Warn("%s is signed in to a desktop: desktops save their settings when you sign out, which would undo part of this restore", u.Name)
		ok, err := a.Ask.Confirm(ui.Confirm{
			Title: fmt.Sprintf("Restore %s's home files anyway?", u.Name),
			Text:  fmt.Sprintf("It's best done from a text console (Ctrl+Alt+F3) while %s is signed out of the desktop.", u.Name),
		})
		if err != nil || !ok {
			a.problem("%s's home files weren't restored, because %s was signed in to a desktop: sign out, switch to a text console (Ctrl+Alt+F3), sign in there and run the restore again", u.Name, u.Name)
			return nil
		}
	}
	a.Log.Info("restoring %d home paths for %s from the backup", len(steps), u.Name)
	out := filepath.Join(work, "home-"+u.Name)
	list := filepath.Join(work, "home-"+u.Name+".txt")
	if err := writeIncludes(list, stepNames(steps)); err != nil {
		a.problem("%s's home files weren't restored: %v", u.Name, err)
		return nil
	}
	if err := a.restic(ctx, "restore", snap, "--target", out, "--include-file", list); err != nil {
		a.problem("restoring %s's home files from the backup reported errors: %v", u.Name, err)
	}
	a.mergeRcloneInto(u, out)
	var restored []string
	adapted := 0
	for _, s := range steps {
		src := filepath.Join(out, s.Item.ID)
		if _, err := os.Lstat(src); err != nil {
			a.Log.Warn("~/%s isn't in the backup", s.Item.Home)
			continue
		}
		n, err := a.copyHome(u, src, path.Join(u.Home, s.Item.Home))
		if err != nil {
			a.problem("couldn't restore %s's ~/%s: %v", u.Name, s.Item.Home, err)
			continue
		}
		adapted += n
		restored = append(restored, s.Item.Home)
	}
	if adapted > 0 {
		a.Log.Info("pointed %d restored files at %s's home, %s", adapted, u.Name, u.Home)
	}
	return restored
}

// copyHome copies a restored path into the user's home, adapts the paths in it, and hands all of
// it to the user. It returns how many files were adapted.
func (a *Applier) copyHome(u LocalUser, src, target string) (int, error) {
	sys, err := a.homeTarget(u, target)
	if err != nil {
		return 0, err
	}
	if err := a.mkdirHome(u, path.Dir(sys)); err != nil {
		return 0, err
	}
	dst := a.path(sys)
	if _, err := copyTree(src, dst); err != nil {
		return 0, err
	}
	n := adaptTree(a.pl.adapt, dst) // before handing over: it remakes symlinks as root
	err = filepath.WalkDir(dst, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		return a.chown(p, u.UID, u.GID)
	})
	return n, err
}

// mergeRcloneInto keeps the user's rclone config on this machine and adds the backup's other
// remotes to it, instead of restoring the backup's over it.
func (a *Applier) mergeRcloneInto(u LocalUser, out string) {
	live, err := a.homeTarget(u, path.Join(u.Home, RcloneConf))
	if err != nil {
		return
	}
	keep, err := os.ReadFile(a.path(live))
	if err != nil {
		return
	}
	for _, m := range a.plan.Users {
		if m.To.Name != u.Name || m.From.Home == "" {
			continue
		}
		old := filepath.Join(out, m.From.Home, RcloneConf)
		if fi, err := os.Lstat(old); err != nil || !fi.Mode().IsRegular() {
			continue
		}
		data, err := os.ReadFile(old)
		if err != nil {
			continue
		}
		merged, added := mergeRclone(keep, data)
		os.Remove(old)
		if added > 0 {
			if err := replaceFile(a.path(live), merged); err != nil {
				a.problem("couldn't add the backup's rclone remotes to %s's config: %v", u.Name, err)
				continue
			}
			if err := a.chown(a.path(live), u.UID, u.GID); err != nil {
				a.Log.Warn("%v", err)
			}
			keep = merged
			a.Log.Info("kept %s's working rclone config and added %d other remotes from the backup", u.Name, added)
		} else {
			a.Log.Info("kept %s's working rclone config", u.Name)
		}
	}
}

var rcloneSection = regexp.MustCompile(`(?m)^\[(.+)\]\s*$`)

// mergeRclone adds the remotes in old that keep doesn't have to keep, and returns the result and
// how many it added. Encrypted configs are left alone. Ported from merge_rclone.
func mergeRclone(keep, old []byte) ([]byte, int) {
	k, o := string(keep), string(old)
	if strings.Contains(k+o, "RCLONE_ENCRYPT") {
		return keep, 0
	}
	have := map[string]bool{}
	for _, m := range rcloneSection.FindAllStringSubmatch(k, -1) {
		have[m[1]] = true
	}
	var blocks []string
	var cur strings.Builder
	for _, l := range strings.SplitAfter(o, "\n") {
		if strings.HasPrefix(l, "[") && cur.Len() > 0 {
			blocks = append(blocks, cur.String())
			cur.Reset()
		}
		cur.WriteString(l)
	}
	if cur.Len() > 0 {
		blocks = append(blocks, cur.String())
	}
	var add []string
	for _, b := range blocks {
		if !strings.HasPrefix(b, "[") {
			continue
		}
		first, _, _ := strings.Cut(b, "\n")
		end := strings.LastIndex(first, "]")
		if end < 2 {
			continue
		}
		if name := first[1:end]; !have[name] {
			add = append(add, b)
		}
	}
	if len(add) == 0 {
		return keep, 0
	}
	return []byte(strings.TrimRight(k, "\n") + "\n\n" + strings.Join(add, "")), len(add)
}

// graphicalSessions are the session types of a desktop.
var graphicalSessions = map[string]bool{"x11": true, "wayland": true, "mir": true}

// graphicalSession reports whether the user has a desktop session (X11 or Wayland) open, as
// logind sees it. Generalized from the prototype's check for Plasma.
func (a *Applier) graphicalSession(ctx context.Context, user string) bool {
	r := a.Here.Run
	if !r.Exists("loginctl") {
		return false
	}
	out, err := r.Output(ctx, "loginctl", "list-sessions", "--no-legend")
	if err != nil {
		return false
	}
	for _, l := range nonEmptyLines(out) {
		f := strings.Fields(l)
		if len(f) < 3 || f[2] != user {
			continue
		}
		props, err := r.Output(ctx, "loginctl", "show-session", f[0], "-p", "Type", "-p", "Class", "-p", "State")
		if err != nil {
			continue
		}
		p := map[string]string{}
		for _, kv := range nonEmptyLines(props) {
			k, v, _ := strings.Cut(kv, "=")
			p[k] = v
		}
		if graphicalSessions[p["Type"]] && p["Class"] != "greeter" && p["State"] != "closing" {
			return true
		}
	}
	return false
}
