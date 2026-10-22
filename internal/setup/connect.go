package setup

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/Evimore/carryon/internal/config"
	"github.com/Evimore/carryon/internal/engine"
	"github.com/Evimore/carryon/internal/restore"
	"github.com/Evimore/carryon/internal/ui"
)

// noSettings is the menu value for starting from the default settings. It can't be a machine name,
// which starts with a letter or digit.
const noSettings = "-"

// How a machine joining a repository is restored (init --connect --restore).
const (
	// RestoreNow: everything at once with carryon restore. Backups pause until it's done.
	RestoreNow = "now"
	// RestoreAsYouGo: each app's settings are offered when it gets installed. Backups start at once.
	RestoreAsYouGo = "as-you-go"
)

// Connect joins this machine to an existing repository at url, typically on a fresh install that
// is about to be restored. It can reuse one machine's settings from its newest backup. Restoring
// everything now pauses automatic backups until `carryon restore` finishes; restoring as you go
// keeps that machine's record, so installing an app offers its saved settings, and backups start
// at once.
func (e *Env) Connect(ctx context.Context, url string, o Options) error {
	if err := e.checkRestic(ctx); err != nil {
		return err
	}
	cfg, err := e.loadExisting()
	if err != nil {
		return err
	}
	if url, err = normalizeURL(url); err != nil {
		return err
	}
	if url, err = e.guardDestination(url, o.Yes); err != nil {
		return err
	}
	rcloneUser, err := e.chooseRcloneUser(url, o.RcloneUser, cfg.Repository.RcloneUser)
	if err != nil {
		return err
	}
	sec, err := e.connectPassword(ctx, url, rcloneUser, o)
	if err != nil {
		return err
	}
	defer sec.discard()
	eng := e.engine(withRclone(sec.repo(url), rcloneUser), "")

	list, err := listMachines(ctx, eng)
	if err != nil {
		return err
	}
	from, same, err := e.chooseSettings(ctx, eng, list, o.SettingsFrom)
	if err != nil {
		return err
	}
	var files settingsFiles
	if from.Name != "" {
		reused, f, err := e.settingsFrom(ctx, eng, from)
		if err != nil {
			return err
		}
		cfg, files = reused, f
	}

	def := e.defaultMachineName()
	if same == thisMachine {
		def = from.Name
	}
	name, err := e.chooseMachineName(ctx, eng, list, o.MachineName, def)
	if err != nil {
		return err
	}
	mode, err := e.chooseRestoreMode(o.Restore, from)
	if err != nil {
		return err
	}
	if o.RcloneUser == "" && cfg.Repository.RcloneUser != "" && strings.HasPrefix(url, "rclone:") {
		rcloneUser = cfg.Repository.RcloneUser
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
		return fmt.Errorf("the settings would have problems, so nothing more was changed:\n%w", err)
	}
	if err := files.write(e, from.Name); err != nil {
		return err
	}
	var replaced *restore.KeptInfo
	if mode == RestoreAsYouGo {
		if replaced, err = e.keepSource(ctx, eng, from); err != nil {
			e.Log.Warn("Restoring as you go can't start (%v), so backups pause until you restore everything with sudo carryon restore.", err)
			mode = RestoreNow
		}
	}
	if mode == RestoreNow {
		// Pause backups before the timers can start one: a half-restored system must never
		// become the newest backup.
		if err := e.setRestoring(); err != nil {
			return err
		}
	}
	if err := e.finish(ctx, eng, cfg); err != nil {
		return err
	}
	if replaced != nil && replaced.Tagged {
		if err := restore.Untag(ctx, eng, *replaced); err != nil {
			e.Log.Warn("%v", err)
		}
	}
	if mode == RestoreAsYouGo && name == from.Name {
		// This machine's retention would one day forget the snapshot offers restore from.
		if err := restore.TagSource(ctx, eng, e.Dirs.State); err != nil {
			e.Log.Warn("%v", err)
		}
	}
	if !KitConfirmed(e.Dirs) && sec.password != "" {
		// Whoever typed the password in has it saved somewhere.
		if err := e.markKitConfirmed(); err != nil {
			return err
		}
	}
	if mode == RestoreAsYouGo {
		fmt.Fprintf(e.Out, "\nConnected to %s as %s. Automatic backups start now.\n", url, name)
		fmt.Fprintf(e.Out, "As you install apps, Carryon offers their saved settings from %s: pacman's output says how\n", from.Name)
		fmt.Fprintln(e.Out, "to restore them, and carryon inbox lists the offers. To restore everything at once instead:")
		fmt.Fprintln(e.Out, "    sudo carryon restore")
		return nil
	}
	fmt.Fprintf(e.Out, "\nConnected to %s as %s. Automatic backups are paused until the restore is done.\n", url, name)
	fmt.Fprintln(e.Out, "Next, ideally from a text console (Ctrl+Alt+F3) while signed out of the desktop:")
	fmt.Fprintln(e.Out, "    sudo carryon restore")
	return nil
}

// chooseRestoreMode asks whether to restore everything now or as you go, or checks --restore.
// Restoring as you go works from the machine whose settings are reused; without one, or without
// a terminal, everything is restored now, as before.
func (e *Env) chooseRestoreMode(given string, from Machine) (string, error) {
	switch given {
	case "", RestoreNow, RestoreAsYouGo:
	default:
		return "", fmt.Errorf("--restore %s: say %s or %s", given, RestoreNow, RestoreAsYouGo)
	}
	if from.Name == "" {
		if given == RestoreAsYouGo {
			return "", errors.New("--restore as-you-go restores from the machine whose settings you reuse: name it with --settings-from")
		}
		return RestoreNow, nil
	}
	if given != "" {
		return given, nil
	}
	return e.Prompt.Menu(ui.Menu{
		Title: "Restore everything now, or as you go?",
		Text:  fmt.Sprintf("Carryon can restore everything from %s now, or offer each app's saved settings when you install it.", from.Name),
		Options: []ui.Option{
			{Value: RestoreNow, Label: "Everything now", Note: "backups pause until sudo carryon restore is done"},
			{Value: RestoreAsYouGo, Label: "As you go", Note: "backups start now; installing an app offers its settings"},
		},
		Default: RestoreNow,
		Flag:    "--restore",
	})
}

// keepSource fetches the record from the newest backup of the machine whose settings are reused
// and keeps it, for restoring as you go. A source kept before from another snapshot is replaced,
// and its offers closed; replaced says what it was, so its tag can come off.
func (e *Env) keepSource(ctx context.Context, eng *engine.Engine, from Machine) (replaced *restore.KeptInfo, err error) {
	if err := os.MkdirAll(e.Dirs.State, 0o755); err != nil {
		return nil, err
	}
	src, err := restore.Fetch(ctx, eng, ui.Defaults{}, restore.FetchOptions{
		From: from.Name, Snapshot: from.Snapshot, TempDir: e.Dirs.State, RecordPaths: []string{e.Dirs.RecordDir()},
	})
	if err != nil {
		return nil, err
	}
	defer src.Close()
	if replaced, err = restore.Keep(e.Dirs.State, src, nil, e.now()); err != nil || replaced == nil {
		return nil, err
	}
	return replaced, restore.CloseOffers(e.Dirs, nil, true)
}

// connectPassword gets the password of the repository at url, from the flags or by asking, and
// checks it opens the repository.
func (e *Env) connectPassword(ctx context.Context, url, rcloneUser string, o Options) (*secret, error) {
	try := func(s *secret) (repoState, string, error) {
		return probe(ctx, e.engine(withRclone(s.repo(url), rcloneUser), ""))
	}
	noRepo := fmt.Errorf("there's no backup repository at %s: check the address, or set up a new repository with \"sudo carryon init\"", url)
	var sec *secret
	switch {
	case o.PasswordCommand != "":
		sec = &secret{command: o.PasswordCommand}
	case o.PasswordFile != "":
		pw, err := ReadPasswordFile(o.PasswordFile)
		if err != nil {
			return nil, err
		}
		if sec, err = e.newSecret(pw); err != nil {
			return nil, err
		}
	}
	if sec != nil {
		state, _, err := try(sec)
		switch {
		case err != nil:
		case state == repoMissing:
			err = noRepo
		case state == repoWrongKey:
			err = fmt.Errorf("that password doesn't open the repository at %s", url)
		}
		if err != nil {
			sec.discard()
			return nil, err
		}
		return sec, nil
	}

	// Ask only once a repository is known to be there.
	throwaway, err := GeneratePassword(e.rand())
	if err != nil {
		return nil, err
	}
	probeSec, err := e.newSecret(throwaway)
	if err != nil {
		return nil, err
	}
	state, _, err := try(probeSec)
	probeSec.discard()
	if err != nil {
		return nil, err
	}
	if state == repoMissing {
		return nil, noRepo
	}
	pw, err := e.askExistingPassword(url, try)
	if err != nil {
		return nil, err
	}
	sec, _, _, err = e.openWith(pw, try)
	return sec, err
}

// chooseSettings asks which machine's settings to reuse. It returns a zero Machine for none, and
// whether the chosen machine is this one.
func (e *Env) chooseSettings(ctx context.Context, eng *engine.Engine, list []Machine, given string) (Machine, sameness, error) {
	if len(list) == 0 {
		e.Log.Warn("The repository has no backups yet, so there are no settings to reuse.")
		if given != "" && given != "none" {
			return Machine{}, unknownMachine, fmt.Errorf("--settings-from %s: the repository has no backups", given)
		}
		return Machine{}, unknownMachine, nil
	}
	fmt.Fprintln(e.Out, "Machines in this repository:")
	opts := make([]ui.Option, 0, len(list)+1)
	def := noSettings
	host := e.defaultMachineName()
	var sameAs = map[string]sameness{}
	for _, m := range list {
		note := fmt.Sprintf("newest backup %s, %d in all", m.Newest.Local().Format("2006-01-02 15:04"), m.Count)
		fmt.Fprintf(e.Out, "  %-20s %s\n", m.Name, note)
		same, err := e.sameMachine(ctx, eng, m)
		if err != nil {
			e.Log.Warn("Couldn't read the record in %s's newest backup: %v", m.Name, err)
		}
		sameAs[m.Name] = same
		if same == thisMachine {
			note += ", this machine"
			def = m.Name
		} else if m.Name == host && def == noSettings {
			def = m.Name
		}
		opts = append(opts, ui.Option{Value: m.Name, Note: note})
	}
	opts = append(opts, ui.Option{Value: noSettings, Label: "None: start from the default settings"})

	choice := given
	switch {
	case given == "none":
		choice = noSettings
	case given != "":
		if _, ok := findMachine(list, given); !ok {
			return Machine{}, unknownMachine, fmt.Errorf("--settings-from %s: the repository has no backups of a machine with that name", given)
		}
	default:
		var err error
		choice, err = e.Prompt.Menu(ui.Menu{
			Title:   "Reuse the settings of which machine?",
			Text:    "Its settings, excludes and bundle rules come from its newest backup. You can change them later.",
			Options: opts,
			Default: def,
			Flag:    "--settings-from",
		})
		if err != nil {
			return Machine{}, unknownMachine, err
		}
	}
	if choice == noSettings {
		return Machine{}, unknownMachine, nil
	}
	m, _ := findMachine(list, choice)
	return m, sameAs[m.Name], nil
}

// settingsFiles are the settings files other than config.toml taken from a backup.
type settingsFiles struct {
	excludes []byte // excludes.txt
	bundles  []byte // bundles.conf
}

// settingsFrom reads a machine's settings from its newest backup. It never takes the machine's
// password or destination identity.
func (e *Env) settingsFrom(ctx context.Context, eng *engine.Engine, m Machine) (config.Config, settingsFiles, error) {
	var f settingsFiles
	read := func(path string) ([]byte, bool, error) { return dump(ctx, eng, m.Snapshot, path) }
	where := func(name string) string {
		return fmt.Sprintf("%s in %s's backup %s", name, m.Name, shortID(m.Snapshot))
	}

	data, ok, err := read(e.Dirs.ConfigFile())
	if err != nil {
		return config.Config{}, f, err
	}
	if !ok {
		return config.Config{}, f, fmt.Errorf("%s's newest backup holds no Carryon settings: use --settings-from none to start from the defaults", m.Name)
	}
	cfg, warnings, err := config.Parse(where("config.toml"), data)
	if err != nil {
		return config.Config{}, f, err
	}
	for _, w := range warnings {
		e.Log.Warn("%s", w)
	}
	if err := cfg.Validate(); err != nil {
		return config.Config{}, f, fmt.Errorf("%s has problems, so it can't be reused (use --settings-from none to start from the defaults):\n%w", where("config.toml"), err)
	}
	if f.excludes, _, err = read(e.Dirs.ExcludesFile()); err != nil {
		return config.Config{}, f, err
	}
	if f.bundles, _, err = read(e.Dirs.BundlesFile()); err != nil {
		return config.Config{}, f, err
	}
	cfg.Repository.PasswordCommand = "" // it may only work on that machine (systemd-creds)
	e.Log.Info("Reusing the settings of %s.", m.Name)
	return cfg, f, nil
}

// write saves the files, keeping what was there before next to them.
func (f settingsFiles) write(e *Env, from string) error {
	suffix := ".before-" + strings.ToLower(orDefault(from, "connect"))
	for _, w := range []struct {
		path string
		data []byte
	}{{e.Dirs.ExcludesFile(), f.excludes}, {e.Dirs.BundlesFile(), f.bundles}} {
		if w.data == nil {
			continue
		}
		kept, err := replaceFile(w.path, w.data, suffix)
		if err != nil {
			return err
		}
		if kept != "" {
			e.Log.Info("Kept the previous %s as %s", filepath.Base(w.path), kept)
		}
	}
	return nil
}

func shortID(id string) string { return id[:min(8, len(id))] }
