package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"

	"github.com/Evimore/carryon/internal/config"
	"github.com/Evimore/carryon/internal/engine"
	"github.com/Evimore/carryon/internal/facts"
	"github.com/Evimore/carryon/internal/paths"
	"github.com/Evimore/carryon/internal/pkgmgr/pacman"
	"github.com/Evimore/carryon/internal/restore"
	"github.com/Evimore/carryon/internal/sysexec"
	"github.com/Evimore/carryon/internal/ui"
)

// restoreSystem is the machine carryon restore plans for. Tests replace restoreEnv.
type restoreSystem struct {
	Root     string // "/" on a real machine
	Run      sysexec.Runner
	Packages restore.Packages
	Ask      func(defaults bool) ui.Prompter
	Repo     func(cfg config.Config) restore.Repo
	Location *time.Location
	Now      func() time.Time
}

var restoreEnv = func(dirs paths.Dirs) restoreSystem {
	run := sysexec.New()
	return restoreSystem{
		Root:     "/",
		Run:      run,
		Packages: pacman.New(run, pacman.Options{}),
		Ask:      func(defaults bool) ui.Prompter { return ui.New(ui.Options{Defaults: defaults}) },
		Repo: func(cfg config.Config) restore.Repo {
			return engine.New(cfg.Repository, dirs, cfg.MachineName)
		},
		Now: time.Now,
	}
}

// planFileName is the full plan's name in the state folder.
const planFileName = "restore-plan.txt"

func newRestoreCmd(dirs paths.Dirs) *cobra.Command {
	var from, snapshot, recordDir string
	var planOnly, defaults bool
	var userFlags []string
	cmd := &cobra.Command{
		Use:   "restore",
		Short: "Pick what to restore from a backup and see the plan",
		Long: `Choose the machine and backup to restore from, tick the bundles you want, and see the plan:
only what differs from this machine, with full diffs in ` + filepath.Join(dirs.State, planFileName) + `.

Bundles start ticked by their when-rule; the reason is shown for those that don't. Files that name
disks by ID (fstab, crypttab, bootloader configs, /etc/kernel/cmdline) are shown, never copied.

Applying the plan comes with a later version of Carryon (roadmap T15): for now carryon restore
never changes the system.

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
				src, err = restore.Fetch(ctx, env.Repo(cfg), ask, restore.FetchOptions{
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
			if noTerminal {
				picked = restore.DefaultPick(ticks)
			} else {
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
			if plan.Todo() == 0 {
				fmt.Fprintln(out, "\nNothing to do: this machine already matches what you picked.")
				return nil
			}
			fmt.Fprintln(out, "\nApplying the plan comes with a later version of Carryon (roadmap T15). Nothing was changed.")
			return nil
		},
	}
	f := cmd.Flags()
	f.StringVar(&from, "from", "", "machine to restore from (default: the only one, or ask)")
	f.StringVar(&snapshot, "snapshot", "latest", "latest, pick (choose from a list), or a snapshot ID")
	f.StringVar(&recordDir, "record", "", "plan from a record on disk instead of the backup")
	f.BoolVar(&planOnly, "plan", false, "only show the plan")
	f.BoolVar(&defaults, "defaults", false, "don't ask: take the bundles that start ticked")
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
