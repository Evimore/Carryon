package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/Evimore/carryon/internal/backup"
	"github.com/Evimore/carryon/internal/config"
	"github.com/Evimore/carryon/internal/dest"
	"github.com/Evimore/carryon/internal/engine"
	"github.com/Evimore/carryon/internal/notify"
	"github.com/Evimore/carryon/internal/paths"
	"github.com/Evimore/carryon/internal/pkgmgr/pacman"
	"github.com/Evimore/carryon/internal/state"
	"github.com/Evimore/carryon/internal/sysexec"
	"github.com/Evimore/carryon/internal/ui"
)

// backupSystem is the machine backups and captures read; tests replace it with a fake tree.
var backupSystem = func(cfg config.Config) (backup.System, error) {
	run := sysexec.New()
	pm := pacman.New(run, pacman.Options{ExtraStandardRepos: cfg.Pacman.ExtraStandardRepos})
	if !pm.Present() {
		return backup.System{}, errors.New("pacman isn't installed here: Carryon captures Arch-based systems for now")
	}
	return backup.System{Root: "/", Runner: run, Packages: pm, IsRoot: geteuid() == 0}, nil
}

// backupHost is what a backup learns about how it runs; tests replace it.
var backupHost = func(ctx context.Context, r sysexec.Runner) hostInfo {
	return hostInfo{
		Inhibitor:  backup.FindInhibitor(ctx),
		Unit:       state.SystemdUnit("/proc/self/cgroup"),
		Invocation: os.Getenv("INVOCATION_ID"),
		Timers:     state.SystemdTimers(r),
	}
}

// hostInfo is how this run is started and what it can use.
type hostInfo struct {
	Inhibitor        string
	Unit, Invocation string
	Timers           state.Timers
}

func newBackupCmd(dirs paths.Dirs) *cobra.Command {
	var quiet, force, timer bool
	cmd := &cobra.Command{
		Use:   "backup",
		Short: "Capture the system and back it up now",
		Long: `Refreshes the system record and backs up the system settings, the record and the home
folders to the repository, then removes old backups as retention says.

Nothing is written unless the destination is the one set up with carryon init: when the backup
drive isn't plugged in, the backup waits for it and runs when it's back.

Backups are paused while a restore is in progress; --force backs up anyway.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if geteuid() != 0 {
				return errors.New("carryon backup reads the whole system and the repository password, so it needs root: run it with sudo")
			}
			log := logger(cmd.ErrOrStderr())
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			opt, err := backupOptions(ctx, cmd, dirs, log, quiet)
			if err != nil {
				return err
			}
			opt.Force = force
			opt.Trigger = state.TriggerManual
			if timer {
				opt.Trigger = state.TriggerTimer
			}
			res, err := backup.Run(ctx, opt)
			return reportBackup(cmd.OutOrStdout(), log, res, err, timer)
		},
	}
	cmd.Flags().BoolVarP(&quiet, "quiet", "q", false, "don't show progress")
	cmd.Flags().BoolVar(&force, "force", false, "back up even while a restore is in progress")
	cmd.Flags().BoolVar(&timer, "timer", false, "run as the scheduled backup: a missing backup drive isn't an error")
	_ = cmd.Flags().MarkHidden("timer")
	return cmd
}

// backupOptions builds a backup from the config and the machine.
func backupOptions(ctx context.Context, cmd *cobra.Command, dirs paths.Dirs, log *ui.Logger, quiet bool) (backup.Options, error) {
	cfg, err := loadConfig(dirs)
	if err != nil {
		return backup.Options{}, err
	}
	sys, err := backupSystem(cfg)
	if err != nil {
		return backup.Options{}, err
	}
	machine := machineName(cfg)
	if machine == "" {
		return backup.Options{}, errors.New("this machine has no name to store its backups under: set machine_name in the config")
	}
	e := engine.New(cfg.Repository, dirs, machine)
	e.Stderr = cmd.ErrOrStderr()
	host := backupHost(ctx, sys.Runner)
	red := notify.Redactor{Secrets: repositorySecrets(cfg.Repository, dirs)}
	opt := backup.Options{
		Dirs:       dirs,
		Config:     cfg,
		System:     sys,
		Engine:     e,
		Checker:    dest.Checker{Root: sys.Root, Repo: e, Runner: rcloneRunner(cfg, sys.Runner)},
		Machine:    machine,
		Inhibitor:  host.Inhibitor,
		Unit:       host.Unit,
		Invocation: host.Invocation,
		Redact:     red.Redact,
		Timers:     host.Timers,
		Log:        log,
		Now:        time.Now,
	}
	if !quiet {
		opt.Progress = cmd.ErrOrStderr()
	}
	return opt, nil
}

// rcloneRunner runs rclone as the user whose rclone login the repository uses.
func rcloneRunner(cfg config.Config, r sysexec.Runner) dest.Runner {
	name := cfg.Repository.RcloneUser
	if name == "" {
		return r
	}
	u, err := notify.LookupUser(name)
	if err != nil {
		return r
	}
	return sysexec.AsUser(r, u, geteuid() == 0, os.Getenv("PATH"))
}

// reportBackup says how a backup went and turns it into the command's result. A scheduled run
// waiting for its destination, paused for a restore or overlapping another backup isn't a
// failure: the user is told when the newest backup gets too old instead.
func reportBackup(out io.Writer, log *ui.Logger, res backup.Result, err error, scheduled bool) error {
	switch {
	case errors.Is(err, state.ErrBusy):
		if scheduled {
			log.Info("Another backup is running; it covers this one.")
			return nil
		}
		return errors.New("another backup is running: wait for it to finish, or see carryon status")
	case errors.Is(err, backup.ErrWaiting):
		log.Warn("%s", res.Run.Error)
		if res.Run.Fix != "" {
			log.Info("%s", res.Run.Fix)
		}
		if scheduled {
			return nil
		}
		return exitError{code: 1}
	case err != nil:
		return err
	}
	switch res.Run.Result {
	case state.ResultPaused:
		return nil
	case state.ResultWarning:
		fmt.Fprintf(out, "Backup done with %s (shown above).\n", plural(len(res.Run.Warnings), "warning"))
	default:
		fmt.Fprintln(out, "Backup done.")
	}
	return nil
}

// sinceText says how long ago t was, roughly.
func sinceText(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "less than a minute"
	case d < time.Hour:
		return plural(int(d/time.Minute), "minute")
	default:
		return plural(int(d/time.Hour), "hour")
	}
}
