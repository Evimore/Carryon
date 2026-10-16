package setup

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/Evimore/carryon/internal/engine"
	"github.com/Evimore/carryon/internal/ui"
)

// FromSysbackup moves this machine over from the prototype: it carries over the prototype's
// settings, excludes and bundle rules, and keeps backing up to the same repository with the same
// password under the old hostname, so the backup history continues. The prototype keeps running
// until Carryon has made a backup; then FromSysbackup offers to switch it off.
func (e *Env) FromSysbackup(ctx context.Context, o Options) error {
	if err := e.checkRestic(ctx); err != nil {
		return err
	}
	dir := filepath.Join(e.sysbackupRoot(), sysbackupConfDir)
	confFile := filepath.Join(dir, "sysbackup.conf")
	data, err := os.ReadFile(confFile)
	if errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("there are no prototype settings at %s to move over: to set up Carryon from scratch, run \"sudo carryon init\"", confFile)
	}
	if err != nil {
		return fmt.Errorf("reading %s: %w", confFile, err)
	}
	cfg, err := e.loadExisting()
	if err != nil {
		return err
	}
	cfg, notes := convertSysbackup(cfg, data)
	for _, n := range notes {
		e.Log.Info("%s", n)
	}
	conf := parseSysbackupConf(data)
	if conf["RESTIC_REPOSITORY"] == "" {
		return fmt.Errorf("%s sets no RESTIC_REPOSITORY, so there's no backup to continue: run \"sudo carryon init\" instead", confFile)
	}
	url, err := normalizeURL(cfg.Repository.URL)
	if err != nil {
		return fmt.Errorf("RESTIC_REPOSITORY in %s: %w", confFile, err)
	}
	if url, err = e.guardDestination(url, o.Yes); err != nil {
		return err
	}
	rcloneUser, err := e.chooseRcloneUser(url, o.RcloneUser, cfg.Repository.RcloneUser)
	if err != nil {
		return err
	}

	pwFile := o.PasswordFile
	if pwFile == "" {
		pwFile = filepath.Join(e.sysbackupRoot(), orDefault(conf["RESTIC_PASSWORD_FILE"], sysbackupPasswordFile))
	}
	pw, err := ReadPasswordFile(pwFile)
	if err != nil {
		return fmt.Errorf("%w\nGive the prototype's password with --password-file", err)
	}
	sec, err := e.newSecret(pw)
	if err != nil {
		return err
	}
	defer sec.discard()
	eng := e.engine(withRclone(sec.repo(url), rcloneUser), "")
	state, _, err := probe(ctx, eng)
	if err != nil {
		return err
	}
	switch state {
	case repoMissing:
		return fmt.Errorf("there's no repository at %s, where the prototype backed up: check RESTIC_REPOSITORY in %s", url, confFile)
	case repoWrongKey:
		return fmt.Errorf("the password in %s doesn't open the repository at %s: give the right one with --password-file", pwFile, url)
	}

	// The prototype stored backups under the hostname: keep it, so the history continues.
	host, err := e.hostname()
	if err != nil && o.MachineName == "" {
		return fmt.Errorf("reading the hostname: %w (give the prototype's hostname with --machine-name)", err)
	}
	name := orDefault(o.MachineName, strings.TrimSpace(host))
	list, err := listMachines(ctx, eng)
	if err != nil {
		return err
	}
	if _, ok := findMachine(list, name); !ok {
		e.Log.Warn("The repository has no backups named %s yet. If the prototype ran under another hostname, run this again with --machine-name set to it.", name)
	}
	if name, err = e.chooseMachineName(ctx, eng, list, name, name); err != nil {
		return err
	}

	repo, err := e.commit(sec, url)
	if err != nil {
		return err
	}
	repo.RcloneUser = rcloneUser
	cfg.MachineName = name
	cfg.Repository = repo
	eng = e.engine(repo, name)
	if err := cfg.Validate(); err != nil {
		return fmt.Errorf("the prototype's settings can't be carried over as they are, so nothing more was changed:\n%w", err)
	}

	var files settingsFiles
	if ex, err := os.ReadFile(filepath.Join(dir, "excludes.txt")); err == nil {
		files.ownExcludes = ownExcludes(ex, e.defaultExcludes())
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("reading the prototype's excludes: %w", err)
	}
	bundlesFile := filepath.Join(dir, "bundles.local")
	if b, err := os.ReadFile(bundlesFile); err == nil {
		if files.bundles, files.bundleProblem, err = convertBundles(b, bundlesFile); err != nil {
			return err
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("reading the prototype's bundle rules: %w", err)
	}
	if err := files.write(e, "sysbackup"); err != nil {
		return err
	}
	if err := e.finish(ctx, eng, cfg); err != nil {
		return err
	}

	// The prototype asked the user to confirm they saved this same password.
	if _, err := os.Stat(filepath.Join(e.sysbackupRoot(), sysbackupStateDir, "password-confirmed")); err == nil && !KitConfirmed(e.Dirs) {
		if err := e.markKitConfirmed(); err != nil {
			return err
		}
	}
	if err := e.showKit(Kit{URL: url, MachineName: name, Password: sec.password, RcloneUser: rcloneUser}, o.KitSaved); err != nil {
		return err
	}
	return e.retirePrototype(ctx, eng, name)
}

// retireCommand switches the prototype off by hand.
func (e *Env) retireCommand() string {
	hook := filepath.Join("/", sysbackupHookFile)
	return fmt.Sprintf("sudo systemctl disable --now %s && sudo mv %s %s.disabled", strings.Join(sysbackupTimers, " "), hook, hook)
}

// retirePrototype offers to switch the prototype's timers and pacman hook off, once Carryon has made
// a backup of this machine. Until then both run, and it prints how to do it later.
func (e *Env) retirePrototype(ctx context.Context, eng *engine.Engine, name string) error {
	snaps, err := eng.Snapshots(ctx, name)
	if err != nil {
		return err
	}
	var newest *engine.Snapshot
	for i := range snaps {
		if slices.Contains(snaps[i].Tags, "carryon") {
			newest = &snaps[i]
		}
	}
	if newest == nil {
		fmt.Fprintln(e.Out, "\nThe prototype keeps running until Carryon has made its first backup. After that,")
		fmt.Fprintln(e.Out, "run \"sudo carryon init --from-sysbackup\" again to switch it off, or run:")
		fmt.Fprintf(e.Out, "    %s\n", e.retireCommand())
		return nil
	}
	off, err := e.Prompt.Confirm(ui.Confirm{
		Title: "Switch the prototype off?",
		Text: fmt.Sprintf("Carryon backed up this machine on %s, so the prototype's timers and pacman hook aren't needed any more.",
			newest.Time.Local().Format("2006-01-02 15:04")),
		Default: true,
	})
	if err != nil {
		return err
	}
	if !off {
		fmt.Fprintf(e.Out, "The prototype keeps running. To switch it off later, run:\n    %s\n", e.retireCommand())
		return nil
	}
	run := e.runner()
	if _, err := run.Output(ctx, "systemctl", append([]string{"disable", "--now"}, sysbackupTimers...)...); err != nil {
		e.Log.Warn("Couldn't switch off the prototype's timers: %s\nRun: sudo systemctl disable --now %s", lastLine(err), strings.Join(sysbackupTimers, " "))
	} else {
		e.Log.Info("Switched off %s.", strings.Join(sysbackupTimers, " and "))
	}
	hook := filepath.Join(e.sysbackupRoot(), sysbackupHookFile)
	switch err := os.Rename(hook, hook+".disabled"); {
	case err == nil:
		e.Log.Info("Switched off the prototype's pacman hook (renamed it to %s.disabled).", hook)
	case !errors.Is(err, fs.ErrNotExist):
		e.Log.Warn("Couldn't switch off the prototype's pacman hook: %v", err)
	}
	return nil
}
