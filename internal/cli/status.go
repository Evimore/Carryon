package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/Evimore/carryon/internal/config"
	"github.com/Evimore/carryon/internal/dest"
	"github.com/Evimore/carryon/internal/engine"
	"github.com/Evimore/carryon/internal/paths"
	"github.com/Evimore/carryon/internal/record"
	"github.com/Evimore/carryon/internal/state"
	"github.com/Evimore/carryon/internal/status"
	"github.com/Evimore/carryon/internal/sysexec"
)

// exitAttention is carryon status --json's exit code when something needs attention.
const exitAttention = 2

// statusChanges is how many of the record's latest changes carryon status shows.
const statusChanges = 5

// What carryon status reads from the machine; tests replace them.
var (
	statusNow    = time.Now
	statusLoc    = time.Local
	statusColor  = colorFor
	statusRoot   = "/"          // the system root the destination check reads mounts from
	statusRunner engine.Runner  // starts restic; nil runs it
	statusRclone sysexec.Runner // runs rclone for the destination check; nil uses the system's
)

func newStatusCmd(dirs paths.Dirs) *cobra.Command {
	var asJSON, snapshots bool
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show recent and upcoming runs, and anything that needs attention",
		Long: `Show how the last backups and checks went, when the next ones run, a backup that is still
owed, and anything that needs attention, with what to do about it. Anyone can run it: it reads
the summary every backup leaves in /run/carryon/status.json, or the copy in /var/lib/carryon
after a reboot.

Run as root it also shows the latest changes to the system record, and with --snapshots the
snapshots in the repository for each machine. --snapshots first checks that the destination is
the one set up with carryon init, and only reads: it never locks or writes the repository.

--json prints the status for scripts and status bars. Its fields are stable:
  format        1, the layout's version (fields are only ever added)
  state         "ok", "attention" or "not_set_up"
  summary       one line, like "Backed up 3 hours ago" or "Backup failed"
  problems      what needs attention: {text, fix, details}
  machine       the name backups are stored under
  updated       when the status was written (null when there is none)
  stale         true when that is longer ago than verify.max_age_hours
  last_backup   the latest backup, however it ended: {kind, trigger, start, end,
                result, error, fix, snapshot, warnings, ...}; null if none
  last_success  the latest backup that saved a snapshot
  last_check    the latest carryon verify
  last_capture  when the system record was last refreshed
  next          [{unit, at}], the timers still to fire, soonest first
  pending       {reason: "change" or "waiting", since} for a backup still owed
  restoring     true while a restore pauses automatic backups
  inbox         suggestions waiting in carryon inbox
  changes       root only: [{commit, date, subject}], the record's latest changes
  snapshots     with --snapshots: {machines: [{name, count, newest, oldest}],
                error, fix}
Results are "ok", "warning", "waiting", "paused", "skipped" or "failed"; times are RFC 3339.

Exit status with --json: 0 when everything is fine, 2 when something needs attention
(including backups not set up), 1 when the status can't be read.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if snapshots && geteuid() != 0 {
				return errors.New("--snapshots opens the repository, which needs its password, so it needs root: run sudo carryon status --snapshots")
			}
			in, err := statusInput(cmd.Context(), dirs, snapshots)
			if err != nil {
				return err
			}
			r := status.Build(in)
			out := cmd.OutOrStdout()
			if asJSON {
				enc := json.NewEncoder(out)
				enc.SetIndent("", "  ")
				enc.SetEscapeHTML(false)
				if err := enc.Encode(r); err != nil {
					return err
				}
				if r.NeedsAttention() {
					return exitError{code: exitAttention}
				}
				return nil
			}
			return status.WriteText(out, r, status.TextOptions{Color: statusColor(out), Loc: statusLoc})
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print JSON")
	cmd.Flags().BoolVar(&snapshots, "snapshots", false, "also list the snapshots in the repository (root)")
	return cmd
}

// statusInput gathers what carryon status shows. Without root it reads only world-readable files.
func statusInput(ctx context.Context, dirs paths.Dirs, snapshots bool) (status.Input, error) {
	in := status.Input{Now: statusNow(), RestoringFlag: dirs.RestoringFlag(), MaxAge: status.DefaultMaxAge}
	s, err := state.ReadStatus(dirs)
	switch {
	case err == nil:
		in.Status, in.Found = s, true
	case errors.Is(err, fs.ErrNotExist):
	default:
		return in, fmt.Errorf("reading the status (%v): the next backup writes it again", err)
	}
	// The flags and the inbox may be newer than the status, or there may be no status yet: the pacman hook marks
	// a backup owed without rewriting status.json, and init --connect sets the restoring flag on a
	// machine that has never backed up.
	if p, ok, err := state.ReadPending(dirs); err == nil {
		in.Status.Pending = nil
		if ok {
			p.Since = p.Since.UTC()
			in.Status.Pending = &p
		}
	}
	in.Status.Restoring = state.Restoring(dirs)
	in.Status.Inbox = state.InboxCount(dirs) // settling a suggestion doesn't rewrite status.json
	if runs, err := state.ReadRuns(dirs); err == nil {
		in.Runs = runs
	}
	_, err = state.ReadDestination(dirs)
	in.NotSetUp = errors.Is(err, state.ErrNotSetUp)

	cfg, _, cfgErr := config.Load(dirs.ConfigFile())
	if cfgErr == nil && cfg.Verify.MaxAgeHours > 0 {
		in.MaxAge = time.Duration(cfg.Verify.MaxAgeHours) * time.Hour
	}

	if geteuid() == 0 {
		commits, err := record.History(ctx, dirs.RecordDir(), historyGit(), statusChanges)
		switch {
		case err == nil:
			in.Changes = commits
		case errors.Is(err, record.ErrNoHistory):
			in.Changes = []record.Commit{}
		}
	}
	if snapshots {
		in.Snapshots = statusSnapshots(ctx, dirs)
	}
	return in, nil
}

// statusSnapshots lists the repository's snapshots by machine, after checking the destination is
// the one recorded at init, as a backup does. restic runs with --no-lock, so nothing is written.
func statusSnapshots(ctx context.Context, dirs paths.Dirs) *status.Snapshots {
	cfg, err := loadConfig(dirs)
	if err != nil {
		return &status.Snapshots{Machines: []status.Machine{}, Error: err.Error()}
	}
	id, err := state.ReadDestination(dirs)
	if err != nil {
		return &status.Snapshots{Machines: []status.Machine{}, Error: err.Error()}
	}
	e := engine.New(cfg.Repository, dirs, machineName(cfg))
	e.NoLock = true
	e.Runner = statusRunner
	e.Stderr = io.Discard
	rclone := statusRclone
	if rclone == nil {
		rclone = sysexec.New()
	}
	checker := dest.Checker{Root: statusRoot, Repo: e, Runner: rcloneRunner(cfg, rclone)}
	if r := checker.Check(ctx, cfg.Repository.URL, id); !r.OK() {
		return &status.Snapshots{Machines: []status.Machine{}, Error: r.Message, Fix: r.Fix}
	}
	snaps, err := e.Snapshots(ctx, "")
	if err != nil {
		var re *engine.Error
		if errors.As(err, &re) {
			return &status.Snapshots{Machines: []status.Machine{}, Error: re.Problem.Cause, Fix: re.Problem.Fix}
		}
		return &status.Snapshots{Machines: []status.Machine{}, Error: err.Error()}
	}
	return &status.Snapshots{Machines: status.ByMachine(snaps)}
}

// colorFor reports whether output to w may be coloured: only on a terminal, and not with NO_COLOR.
func colorFor(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok || os.Getenv("NO_COLOR") != "" {
		return false
	}
	st, err := f.Stat()
	return err == nil && st.Mode()&os.ModeCharDevice != 0
}
