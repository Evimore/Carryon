package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/Evimore/carryon/internal/catalog"
	"github.com/Evimore/carryon/internal/config"
	"github.com/Evimore/carryon/internal/engine"
	"github.com/Evimore/carryon/internal/facts"
	"github.com/Evimore/carryon/internal/paths"
	"github.com/Evimore/carryon/internal/pkgmgr/pacman"
	"github.com/Evimore/carryon/internal/record"
	"github.com/Evimore/carryon/internal/restore"
	"github.com/Evimore/carryon/internal/sysexec"
	"github.com/Evimore/carryon/internal/ui"
)

// restoreSystem is the machine carryon restore plans for and applies to. Tests replace restoreEnv.
type restoreSystem struct {
	Root      string // "/" on a real machine
	Run       sysexec.Runner
	Packages  restore.Packages
	Installer restore.Installer
	Ask       func(defaults bool) ui.Prompter
	Repo      func(cfg config.Config) restore.Repo
	// Chown changes owners while applying. Nil means os.Lchown.
	Chown    func(path string, uid, gid int) error
	TempDir  string // where data is put on its way into homes; "" means /var/tmp
	Location *time.Location
	Now      func() time.Time
}

var restoreEnv = func(dirs paths.Dirs) restoreSystem {
	run := sysexec.New()
	pm := pacman.New(run, pacman.Options{})
	return restoreSystem{
		Root:      "/",
		Run:       run,
		Packages:  pm,
		Installer: pm,
		Ask:       func(defaults bool) ui.Prompter { return ui.New(ui.Options{Defaults: defaults}) },
		Repo: func(cfg config.Config) restore.Repo {
			return engine.New(cfg.Repository, dirs, cfg.MachineName)
		},
		Now: time.Now,
	}
}

// lazyRepo opens the backup the first time the data phase needs it, for plans made from a record
// on disk.
type lazyRepo struct {
	open func() (restore.Repo, error)
	repo restore.Repo
	err  error
}

func (l *lazyRepo) get() (restore.Repo, error) {
	if l.repo == nil && l.err == nil {
		l.repo, l.err = l.open()
	}
	return l.repo, l.err
}

func (l *lazyRepo) Snapshots(ctx context.Context, host string) ([]engine.Snapshot, error) {
	r, err := l.get()
	if err != nil {
		return nil, err
	}
	return r.Snapshots(ctx, host)
}

func (l *lazyRepo) Run(ctx context.Context, args ...string) (engine.Result, error) {
	r, err := l.get()
	if err != nil {
		return engine.Result{}, err
	}
	return r.Run(ctx, args...)
}

// Live shows restic's progress when the backup can.
func (l *lazyRepo) Live(ctx context.Context, stdout io.Writer, args ...string) (engine.Result, error) {
	r, err := l.get()
	if err != nil {
		return engine.Result{}, err
	}
	if lr, ok := r.(interface {
		Live(context.Context, io.Writer, ...string) (engine.Result, error)
	}); ok {
		return lr.Live(ctx, stdout, args...)
	}
	return r.Run(ctx, args...)
}

// planFileName is the full plan's name in the state folder.
const planFileName = "restore-plan.txt"

func newRestoreCmd(dirs paths.Dirs) *cobra.Command {
	var from, snapshot, recordDir, bundle string
	var planOnly, defaults, yes bool
	var userFlags []string
	cmd := &cobra.Command{
		Use:   "restore",
		Short: "Pick what to restore from a backup, see the plan and apply it",
		Long: `Choose the machine and backup to restore from, tick the bundles you want, and see the plan:
only what differs from this machine, with full diffs in ` + filepath.Join(dirs.State, planFileName) + `.
Nothing changes until you've seen the plan and said yes.

Bundles start ticked by their when-rule; the reason is shown for those that don't. Files that name
disks by ID (fstab, crypttab, bootloader configs, /etc/kernel/cmdline) are shown, never copied.
Files the restore replaces are kept next to them as *.pre-restore. If a step fails, the rest
still applies, and everything that failed is listed at the end with what to do.

Without a terminal, --yes applies the plan; --defaults alone only shows it. --bundle ID restores
one bundle, such as an app's settings after installing it. Automatic backups pause during a full
restore and start again when it finishes.

--record DIR plans from a record on disk instead of the backup, such as one the sysbackup
prototype wrote to /var/lib/sysbackup/manifest.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			log := ui.Stderr()
			env := restoreEnv(dirs)
			out := cmd.OutOrStdout()
			given, err := restore.ParseUserFlags(userFlags)
			if err != nil {
				return err
			}
			ask := env.Ask(defaults)
			_, noTerminal := ask.(ui.Defaults)

			hostname, _ := os.Hostname()
			here, err := facts.Collect(ctx, env.Root, env.Run)
			if err != nil {
				return err
			}
			if here.Host != "" {
				hostname = here.Host
			}

			var src *restore.Source
			var repo restore.Repo
			if recordDir != "" {
				src, err = restore.OpenRecord(recordDir)
			} else {
				if geteuid() != 0 {
					return errors.New("carryon restore needs root to read the repository password: run it with sudo, or plan from a record on disk with --record DIR")
				}
				cfg, cerr := loadConfig(dirs)
				if cerr != nil {
					return cerr
				}
				log.Info("reading the backup's snapshots")
				repo = env.Repo(cfg)
				src, err = restore.Fetch(ctx, repo, ask, restore.FetchOptions{
					From: from, Snapshot: snapshot, Here: cfg.MachineName, Location: env.Location,
				})
			}
			if err != nil {
				return err
			}
			defer src.Close()

			ticks, warnings := restore.DefaultTicks(src.Record, here)
			for _, w := range warnings {
				log.Warn("%s", w)
			}
			var picked restore.Picked
			switch {
			case bundle != "":
				if !slices.ContainsFunc(src.Record.Bundles, func(b record.Bundle) bool { return b.ID == bundle }) {
					return fmt.Errorf("the backup of %s has no bundle %s: list them with: carryon restore --plan", src.Machine(), bundle)
				}
				picked = restore.Picked{Bundles: []string{bundle}}
			case noTerminal:
				picked = restore.DefaultPick(ticks)
			default:
				prev := restore.LoadChoices(dirs.State, src.Machine())
				if picked, err = restore.Pick(ask, ticks, prev, src.Machine(), hostname); err != nil {
					return err
				}
				if err := restore.SaveChoices(dirs.State, src.Machine(), picked.Choices(hostname, env.Now())); err != nil {
					log.Warn("%v", err)
				}
			}

			localUsers, err := restore.ReadUsers(env.Root)
			if err != nil {
				return err
			}
			users, err := restore.MapUsers(src.Record.Meta, localUsers, ask, restore.MapOptions{
				Given: given, Invoking: os.Getenv("SUDO_USER"),
			})
			if err != nil {
				return err
			}

			planner := &restore.Planner{Source: src, Users: users, Here: &restore.Machine{
				Root: env.Root, Hostname: hostname, Run: env.Run, IsRoot: geteuid() == 0,
				Packages: env.Packages, Users: localUsers,
			}}
			plan, err := planner.Make(ctx, picked)
			if err != nil {
				return err
			}
			planFile, err := writePlanFile(dirs, plan.Full(env.Location))
			if err != nil {
				log.Warn("%v", err)
			}
			plan.Summary(out, planFile, env.Location)
			if planOnly {
				return nil
			}
			full := bundle == ""
			if plan.Todo() == 0 {
				fmt.Fprintln(out, "\nNothing to do: this machine already matches what you picked.")
				if full {
					resumeBackups(out, log, dirs)
				}
				return nil
			}
			if !yes && noTerminal {
				fmt.Fprintln(out, "\nNothing was changed. To apply this plan without a terminal, add --yes.")
				return nil
			}
			if geteuid() != 0 {
				return errors.New("applying the plan needs root: run it again with sudo")
			}
			if !yes {
				ok, err := ask.Confirm(ui.Confirm{Title: "Apply this plan?", NoDefault: true, Flag: "--yes"})
				if err != nil {
					return err
				}
				if !ok {
					fmt.Fprintln(out, "Nothing was changed.")
					return nil
				}
			}
			if repo == nil {
				repo = &lazyRepo{open: func() (restore.Repo, error) {
					cfg, err := loadConfig(dirs)
					if err != nil {
						return nil, err
					}
					return env.Repo(cfg), nil
				}}
			}
			if full {
				if err := pauseBackups(dirs, env.Now()); err != nil {
					log.Warn("%v", err)
				}
			}
			cat, err := catalog.Load(filepath.Join(dirs.Share, "catalog"))
			if err != nil {
				log.Warn("the app catalog wasn't read, so its notes after restoring are missing: %v", err)
			}
			applier := &restore.Applier{
				Source: src, Here: planner.Here, Packages: env.Installer, Repo: repo, Ask: ask, Log: log,
				Out: out, Yes: yes, Catalog: cat, Chown: env.Chown, TempDir: env.TempDir,
			}
			res := applier.Apply(ctx, plan)
			return reportRestore(out, log, dirs, res, full)
		},
	}
	f := cmd.Flags()
	f.StringVar(&from, "from", "", "machine to restore from (default: the only one, or ask)")
	f.StringVar(&snapshot, "snapshot", "latest", "latest, pick (choose from a list), or a snapshot ID")
	f.StringVar(&recordDir, "record", "", "plan from a record on disk instead of the backup")
	f.BoolVar(&planOnly, "plan", false, "only show the plan")
	f.BoolVar(&defaults, "defaults", false, "don't ask: take the bundles that start ticked, and only show the plan unless --yes")
	f.BoolVar(&yes, "yes", false, "apply the plan without asking (needed without a terminal)")
	f.StringVar(&bundle, "bundle", "", "restore only this bundle, such as app-obs-studio")
	f.StringArrayVar(&userFlags, "user", nil, "give a backed-up user's files to a user here: OLD=NEW (repeatable)")
	return cmd
}

// writePlanFile writes the full plan into the state folder, or the temp folder when the state
// folder can't be written (without root). It returns where it went.
func writePlanFile(dirs paths.Dirs, text string) (string, error) {
	path := filepath.Join(dirs.State, planFileName)
	err := os.MkdirAll(dirs.State, 0o700)
	if err == nil {
		err = os.WriteFile(path, []byte(text), 0o600)
	}
	if err == nil {
		return path, nil
	}
	f, err2 := os.CreateTemp("", "carryon-restore-plan-*.txt")
	if err2 != nil {
		return "", fmt.Errorf("the full plan couldn't be saved: %w", err)
	}
	defer f.Close()
	if _, err2 := f.WriteString(text); err2 != nil {
		return "", fmt.Errorf("the full plan couldn't be saved: %w", err2)
	}
	return f.Name(), nil
}

// pauseBackups writes the restoring flag, unless it's there already (carryon init --connect writes
// it on a new machine), so automatic backups don't save a half-restored system.
func pauseBackups(dirs paths.Dirs, now time.Time) error {
	flag := dirs.RestoringFlag()
	if _, err := os.Lstat(flag); err == nil {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(flag), 0o755); err != nil {
		return fmt.Errorf("pausing automatic backups: %w", err)
	}
	if err := os.WriteFile(flag, []byte(now.Format(time.RFC3339)+"\n"), 0o644); err != nil {
		return fmt.Errorf("pausing automatic backups: %w", err)
	}
	return nil
}

// resumeBackups removes the restoring flag, so automatic backups start again.
func resumeBackups(out io.Writer, log *ui.Logger, dirs paths.Dirs) {
	err := os.Remove(dirs.RestoringFlag())
	switch {
	case err == nil:
		fmt.Fprintln(out, "Automatic backups are on again.")
	case !errors.Is(err, os.ErrNotExist):
		log.Warn("automatic backups are still paused: remove %s to start them again (%v)", dirs.RestoringFlag(), err)
	}
}

// reportRestore lists what failed and the notes for what was restored, and starts automatic
// backups again after a full restore that went through. Ported from the end of apply_plan.
func reportRestore(out io.Writer, log *ui.Logger, dirs paths.Dirs, res *restore.Result, full bool) error {
	if len(res.Notes) > 0 {
		fmt.Fprintln(out, "\nAbout what was restored:")
		for _, n := range res.Notes {
			fmt.Fprintf(out, "  - %s: %s\n", n.App, strings.TrimSpace(n.Text))
		}
	}
	if res.NotBackedUp != "" {
		fmt.Fprintf(out, "\nWhat to download again (Steam games, app files...) is listed in %s.\n", res.NotBackedUp)
	}
	if len(res.Problems) > 0 {
		fmt.Fprintln(out, "\nFinished, but these need your attention:")
		for _, p := range res.Problems {
			fmt.Fprintf(out, "  - %s\n", p)
		}
		if full {
			fmt.Fprintf(out, "\nAutomatic backups stay paused, so they don't save a half-restored system. When you've\n"+
				"fixed the above, run sudo carryon restore again: it only does what's still missing. To start\n"+
				"backups again without that, remove %s.\n", dirs.RestoringFlag())
		}
		return exitError{code: 1}
	}
	fmt.Fprintln(out, "\nDone. Restart the computer so every service, module and kernel parameter comes up cleanly.")
	if full {
		resumeBackups(out, log, dirs)
	}
	return nil
}
