package setup

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"

	"github.com/Evimore/carryon/internal/config"
	"github.com/Evimore/carryon/internal/dest"
	"github.com/Evimore/carryon/internal/state"
	"github.com/Evimore/carryon/internal/ui"
)

// PasswordOptions are the password change's answers given as flags.
type PasswordOptions struct {
	PasswordFile string // --password-file: the new password, from this file; generated when empty
	Yes          bool   // --yes: change the password without asking
	KitSaved     bool   // --kit-saved: the new recovery kit is saved, don't ask
}

// ChangePassword gives the repository of the machine set up in cfg a new password with restic key
// passwd, saves it in the password file (keeping the old one as .old) and shows the new recovery
// kit, which counts as unsaved until the user confirms it. With a password command, it changes
// the key and tells the user to change what the command reads.
//
// The new password is written next to the password file before restic changes the key, and that
// file goes only once the password is saved or known not to be needed. A change restic made but
// Carryon didn't get to save is finished the next time.
func (e *Env) ChangePassword(ctx context.Context, cfg config.Config, o PasswordOptions) error {
	if err := e.checkRestic(ctx); err != nil {
		return err
	}
	id, err := state.ReadDestination(e.Dirs)
	if err != nil {
		return err
	}
	c := &passwordChange{e: e, cfg: cfg, id: id, file: orDefault(cfg.Repository.PasswordFile, e.Dirs.PasswordFile())}
	c.staged = c.file + ".new"

	e.Log.Info("Checking the backup destination at %s", cfg.Repository.URL)
	if r := c.check(ctx, cfg.Repository); !r.OK() {
		if r.Status == dest.CantOpen {
			if pw, ok := c.stagedOpens(ctx); ok {
				e.Log.Warn("The repository opens with the new password in %s, from a change that didn't finish, and no longer with %s.", c.staged, c.current())
				if o.PasswordFile != "" {
					e.Log.Info("Finishing that change first: run carryon password again to change it to the one in %s.", o.PasswordFile)
				}
				if list, err := listMachines(ctx, e.engine(c.stagedRepo(), cfg.MachineName)); err == nil {
					c.others = otherMachines(list, cfg.MachineName)
				}
				return c.finish(pw, o.KitSaved)
			}
		}
		return fmt.Errorf("%s\nThe password wasn't changed", r)
	}
	pw, err := c.newPassword(o.PasswordFile)
	if err != nil {
		return err
	}
	eng := e.engine(cfg.Repository, cfg.MachineName)
	list, err := listMachines(ctx, eng)
	if err != nil {
		return err
	}
	if c.others = otherMachines(list, cfg.MachineName); len(c.others) > 0 {
		e.Log.Warn("The repository also holds the backups of %s. Machines that open it with the same password, "+
			"as \"carryon init --connect\" sets them up, can't back up after the change until you save the new password "+
			"on each: run \"sudo carryon init\" there and enter it when asked.", andList(c.others))
	}
	if !o.Yes {
		ok, err := e.Prompt.Confirm(ui.Confirm{
			Title:     "Change the repository password?",
			Text:      "The current password stops opening the repository. Carryon saves the new one and shows your new recovery kit.",
			NoDefault: true,
			Flag:      "--yes",
		})
		if err != nil {
			return err
		}
		if !ok {
			return errors.New("the password wasn't changed")
		}
	}

	// A backup of this machine reads the password file again for each restic run: none may run
	// between the change and saving the new password.
	unlock, err := state.Lock(e.Dirs)
	if errors.Is(err, state.ErrBusy) {
		return errors.New("a backup is running: wait for it to finish (carryon status shows it), then try again. The password wasn't changed")
	}
	if err != nil {
		return err
	}
	defer unlock()
	if !o.Yes {
		// The answer may have taken a while: the drive must still be the one set up.
		if r := c.check(ctx, cfg.Repository); !r.OK() {
			return fmt.Errorf("%s\nThe password wasn't changed", r)
		}
	}

	// Saved before restic changes the key, so the new password can't be lost in between.
	if err := writePrivate(c.staged, []byte(pw+"\n")); err != nil {
		return err
	}
	e.Log.Info("Changing the repository password")
	r, err := eng.Live(ctx, nil, "key", "passwd", "--new-password-file", c.staged)
	if err == nil {
		err = r.Err()
	}
	if err != nil {
		return c.failed(ctx, pw, err, o.KitSaved)
	}
	return c.finish(pw, o.KitSaved)
}

// passwordChange is one run of ChangePassword.
type passwordChange struct {
	e      *Env
	cfg    config.Config
	id     dest.Identity // the destination recorded at init
	file   string        // the password file; with a password command, where it would be
	staged string        // the new password, from before restic changes the key until it's saved
	others []string      // the other machines with backups in the repository
}

// check checks the destination as a backup does, opening the repository with repo's password.
func (c *passwordChange) check(ctx context.Context, repo config.Repository) dest.Result {
	eng := c.e.engine(repo, c.cfg.MachineName)
	return dest.Checker{Root: c.e.root(), Repo: eng, Runner: c.e.RcloneRunner}.Check(ctx, repo.URL, c.id)
}

// stagedRepo is the repository opened with the staged password.
func (c *passwordChange) stagedRepo() config.Repository {
	r := c.cfg.Repository
	r.PasswordFile, r.PasswordCommand = c.staged, ""
	return r
}

// stagedOpens returns the password an earlier change left staged, when it opens the repository.
func (c *passwordChange) stagedOpens(ctx context.Context) (string, bool) {
	pw, err := ReadPasswordFile(c.staged)
	if err != nil {
		return "", false
	}
	return pw, c.check(ctx, c.stagedRepo()).OK()
}

// current says where the current password comes from.
func (c *passwordChange) current() string {
	if cmd := c.cfg.Repository.PasswordCommand; cmd != "" {
		return "the one " + cmd + " prints"
	}
	return "the one in " + c.file
}

// newPassword reads the new password from --password-file, or generates one.
func (c *passwordChange) newPassword(from string) (string, error) {
	if from == "" {
		return GeneratePassword(c.e.rand())
	}
	pw, err := ReadPasswordFile(from)
	if err != nil {
		return "", err
	}
	if c.cfg.Repository.PasswordCommand == "" {
		if cur, err := ReadPasswordFile(c.file); err == nil && cur == pw {
			return "", fmt.Errorf("%s holds the current password: put the new one in it, or leave out --password-file to get a generated one", from)
		}
	}
	return pw, nil
}

// failed works out what a failed restic key passwd changed, since restic can fail after the new
// key is in place. The staged password goes only when the current one still opens the repository.
func (c *passwordChange) failed(ctx context.Context, pw string, cause error, kitSaved bool) error {
	// Even after Ctrl+C: the answer decides whether the new password may be removed.
	ctx = context.WithoutCancel(ctx)
	if c.check(ctx, c.cfg.Repository).OK() {
		c.removeStaged()
		return fmt.Errorf("restic couldn't change the password: %w\nThe password is still %s", cause, c.current())
	}
	if c.check(ctx, c.stagedRepo()).OK() {
		c.e.Log.Warn("restic reported a problem, but the repository opens with the new password, so the change went through: %v", cause)
		return c.finish(pw, kitSaved)
	}
	return fmt.Errorf("restic couldn't change the password: %w\nCarryon can't tell whether the change went through, so it kept the new password in %s. "+
		"Run \"sudo carryon password\" again once the repository can be reached: it finishes the change if it went through", cause, c.staged)
}

// finish saves the new password, which now opens the repository, and shows the new recovery kit.
// With a password command the staged file stays: it holds the only copy of the new password until
// the user has changed what the command reads.
func (c *passwordChange) finish(pw string, kitSaved bool) error {
	e := c.e
	if err := os.Remove(KitConfirmedFile(e.Dirs)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		e.Log.Warn("Carryon couldn't note that the new recovery kit isn't saved yet: %v", err)
	}
	cmd := c.cfg.Repository.PasswordCommand
	if cmd == "" {
		if err := e.savePassword(c.file, pw); err != nil {
			return fmt.Errorf("the repository's password changed, but Carryon couldn't save it: %w\nThe new password is in %s. Put it in place with: sudo install -m 600 %s %s",
				err, c.staged, quote(c.staged), quote(c.file))
		}
		c.removeStaged()
	}

	repo := c.cfg.Repository
	k := Kit{URL: repo.URL, MachineName: c.cfg.MachineName, Password: pw, PasswordCommand: cmd, RcloneUser: repo.RcloneUser}
	if err := e.showKit(k, kitSaved); errors.Is(err, ui.ErrCancelled) {
		e.Log.Warn("%s", saveKitSoon)
	} else if err != nil {
		return err
	}

	out := e.Out
	if cmd == "" {
		fmt.Fprintf(out, "The repository password is changed, and saved in %s.\n", c.file)
	} else {
		fmt.Fprintf(out, "The repository password is changed, but Carryon reads it with: %s\n", cmd)
		fmt.Fprintln(out, "That still gives the old password, so backups fail until you change what it reads.")
		fmt.Fprintf(out, "The new password is in %s, which only root can read.\n", c.staged)
		fmt.Fprintln(out, "Once \"sudo carryon verify\" opens the repository again, delete that file.")
	}
	if len(c.others) > 0 {
		fmt.Fprintf(out, "Save the new password on %s too: run \"sudo carryon init\" there and enter it when asked.\n", andList(c.others))
	}
	return nil
}

func (c *passwordChange) removeStaged() {
	if err := os.Remove(c.staged); err != nil && !errors.Is(err, fs.ErrNotExist) {
		c.e.Log.Warn("Carryon couldn't remove %s: %v", c.staged, err)
	}
}

// otherMachines are the names in list other than this machine's.
func otherMachines(list []Machine, name string) []string {
	var out []string
	for _, m := range list {
		if m.Name != name {
			out = append(out, m.Name)
		}
	}
	return out
}

// andList joins names for a sentence: "a", "a and b", "a, b and c".
func andList(names []string) string {
	if len(names) < 2 {
		return strings.Join(names, "")
	}
	return strings.Join(names[:len(names)-1], ", ") + " and " + names[len(names)-1]
}
