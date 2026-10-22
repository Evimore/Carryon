// Package backup runs a backup: it checks the destination, captures the record, backs up the
// system and the homes with restic, applies retention and keeps the run history and status.json
// up to date. It also holds the settle step that backs up once package changes have settled. See
// docs/design.md, "Storage", "Destinations and mount guards" and "Running by itself".
//
// It ports the prototype's cmd_backup (legacy/sysbackup.py).
package backup

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/Evimore/carryon/internal/catalog"
	"github.com/Evimore/carryon/internal/config"
	"github.com/Evimore/carryon/internal/dest"
	"github.com/Evimore/carryon/internal/engine"
	"github.com/Evimore/carryon/internal/paths"
	"github.com/Evimore/carryon/internal/record"
	"github.com/Evimore/carryon/internal/restore"
	"github.com/Evimore/carryon/internal/state"
	report "github.com/Evimore/carryon/internal/status"
	"github.com/Evimore/carryon/internal/sysexec"
	"github.com/Evimore/carryon/internal/ui"
)

// Tag is the tag on every snapshot Carryon makes.
const Tag = "carryon"

// RecordPath is where the record is in a snapshot: its place on the machine. Restore reads it
// from there.
const RecordPath = "/var/lib/carryon/record"

// Checker checks a destination before anything is written to it. dest.Checker provides it.
type Checker interface {
	Check(ctx context.Context, url string, want dest.Identity) dest.Result
}

// Options configure a backup.
type Options struct {
	Dirs   paths.Dirs
	Config config.Config
	System System
	// Engine runs restic on the repository.
	Engine *engine.Engine
	// Checker checks the destination against the identity recorded at init.
	Checker Checker
	// Machine is the name the snapshots are stored under.
	Machine string
	// Trigger says what started the run (state.TriggerManual, TriggerTimer, TriggerSettle).
	Trigger string
	// Force backs up even while a restore is in progress.
	Force bool
	// Inhibitor is the systemd-inhibit program restic runs under, so the machine doesn't sleep
	// in the middle of a backup. Empty means none.
	Inhibitor string
	// Unit and Invocation name the systemd unit run this backup is, for the run history.
	Unit, Invocation string
	// Progress receives a progress line now and then while restic runs. Nil means none.
	Progress io.Writer
	// ProgressEvery is how often a progress line is written. Zero means every 30 seconds.
	ProgressEvery time.Duration
	// Redact hides secrets in text that goes into the run history. Nil leaves text as is.
	Redact func(string) string
	// Timers tells status.json when the timers fire next. Nil leaves that out.
	Timers state.Timers
	// Git runs git for the record's history. Nil means System.Runner.
	Git sysexec.Runner
	Log Logger
	Now func() time.Time

	devOf func(string) (uint64, error) // tests replace it
}

// Result is how a backup went.
type Result struct {
	Run      state.Run       // as written to the run history
	Captured *Captured       // the capture, when there was one
	Targets  []string        // the folders backed up (system paths)
	LeftOut  []LeftOut       // filesystems inside them that weren't backed up
	Summary  *engine.Summary // restic's summary, when it finished
}

// Problem is a failed run's error: what went wrong and what to do.
type Problem struct {
	Cause, Fix string
}

func (p *Problem) Error() string {
	if p.Fix == "" {
		return p.Cause
	}
	return p.Cause + "\n" + p.Fix
}

// ErrWaiting means the destination wasn't there, so the backup wrote nothing and waits for it.
// The error wrapping it says why.
var ErrWaiting = errors.New("the backup is waiting for its destination")

// Run runs one backup and records it in the run history and status.json.
//
// It returns state.ErrBusy, without recording a run, when another backup is running; an error
// wrapping ErrWaiting when the destination isn't there (nothing was written, the pending marker
// is set and the settle timer tries again); nil with Result.Run.Result == state.ResultPaused when
// a restore is in progress; and a *Problem when the backup failed.
func Run(ctx context.Context, opt Options) (Result, error) {
	if opt.Log == nil {
		opt.Log = nopLogger{}
	}
	if opt.Now == nil {
		opt.Now = time.Now
	}
	if opt.Redact == nil {
		opt.Redact = func(s string) string { return s }
	}
	if opt.devOf == nil {
		opt.devOf = deviceOf
	}
	unlock, err := state.Lock(opt.Dirs)
	if err != nil {
		return Result{}, err
	}
	defer unlock()

	b := &run{opt: opt, log: opt.Log}
	b.res.Run = state.Run{
		Kind: state.KindBackup, Trigger: opt.Trigger, Unit: opt.Unit, Invocation: opt.Invocation,
		Machine: opt.Machine, Start: opt.Now(),
	}
	err = b.backup(ctx)
	b.finish(ctx, err)
	return b.res, err
}

// run is one backup in progress.
type run struct {
	opt      Options
	log      Logger
	res      Result
	warnings []string
}

func (b *run) warn(format string, a ...any) {
	msg := fmt.Sprintf(format, a...)
	b.warnings = append(b.warnings, msg)
	b.log.Warn("%s", msg)
}

func (b *run) backup(ctx context.Context) error {
	opt := b.opt
	if state.Restoring(opt.Dirs) && !opt.Force {
		b.res.Run.Result = state.ResultPaused
		b.log.Info("Backups are paused while a restore is in progress, so a half-restored system never " +
			"becomes the newest backup. They start again when `carryon restore` finishes; --force backs up anyway.")
		return nil
	}
	if opt.Engine == nil || opt.Checker == nil {
		return &Problem{Cause: "backup: a restic engine and a destination checker are needed"}
	}
	id, err := state.ReadDestination(opt.Dirs)
	if err != nil {
		return &Problem{Cause: err.Error()}
	}
	if id.RepoID == "" {
		return &Problem{
			Cause: "Carryon has no record of which repository belongs at " + opt.Config.Repository.URL + ".",
			Fix:   "Run `sudo carryon init` to connect to the repository again.",
		}
	}
	if err := b.checkDestination(ctx, id); err != nil {
		return err
	}

	tmp, err := os.MkdirTemp("", "carryon-backup-")
	if err != nil {
		return &Problem{Cause: fmt.Sprintf("making a temporary folder: %v", err)}
	}
	defer os.RemoveAll(tmp)
	excludeFile := filepath.Join(tmp, "excludes")

	copt := CaptureOptions{
		Dirs: opt.Dirs, Config: opt.Config, System: opt.System, Nightly: true,
		ExcludeFile: excludeFile, Git: opt.Git, Log: opt.Log, Now: opt.Now,
	}.withDefaults()
	b.log.Info("Refreshing the system record")
	capd := &Captured{}
	b.res.Captured = capd
	in, err := prepare(copt, excludeFile, capd)
	if err != nil {
		return &Problem{Cause: "Carryon couldn't build the list of what to leave out: " + err.Error()}
	}
	items := b.capture(ctx, copt, in, capd)

	tg := targets(opt.System, opt.Config, items, in.users)
	matcher, err := catalog.NewMatcher(in.excludes)
	if err != nil {
		return &Problem{Cause: "Carryon couldn't read its exclude list: " + err.Error()}
	}
	scan := volumeScan{sys: opt.System, mounts: capd.Mounts, excluded: matcher.Excluded, devOf: opt.devOf}
	nested, left := scan.scan(tg)
	b.res.LeftOut = left
	for _, l := range left {
		b.log.Info("%s", l)
	}
	b.res.Targets = append(tg, nested...)
	b.detect(ctx, items, in.users, capd)
	return b.restic(ctx, excludeFile)
}

// checkDestination fails closed: unless the destination is the one init recorded, nothing is
// written. A destination that is only away (unplugged, offline) makes the run wait for it.
func (b *run) checkDestination(ctx context.Context, id dest.Identity) error {
	url := b.opt.Config.Repository.URL
	r := b.opt.Checker.Check(ctx, url, id)
	if r.OK() {
		return nil
	}
	// Only a destination that is away waits: one that answers but can't be used fails, so the
	// user hears what to do instead of the backups quietly stopping.
	switch r.Status {
	case dest.NotMounted, dest.Unreachable:
		b.res.Run.Result = state.ResultWaiting
		b.res.Run.Error, b.res.Run.Fix = r.Message, r.Fix
		if err := state.MarkPending(b.opt.Dirs, state.ReasonWaiting, b.opt.Now()); err != nil {
			b.warn("Carryon couldn't note that a backup is owed, so it won't retry by itself: %v", err)
		}
		return fmt.Errorf("%w: %s", ErrWaiting, r.String())
	}
	return &Problem{Cause: r.Message, Fix: r.Fix}
}

// capture refreshes the record and returns its items. When the capture fails the backup goes on
// with the last record, so the files are still saved.
func (b *run) capture(ctx context.Context, copt CaptureOptions, in *inputs, cap *Captured) []record.Item {
	err := captureWith(ctx, copt, in, cap)
	b.warnings = append(b.warnings, cap.Warnings...)
	if err == nil {
		b.res.Run.Changes = len(cap.Publish.Changes())
		switch {
		case cap.Publish.First:
			b.log.Info("The system record was written for the first time")
		case b.res.Run.Changes > 0:
			b.log.Info("The system record has %d change(s)", b.res.Run.Changes)
		default:
			b.log.Info("The system record hasn't changed")
		}
		return cap.Result.Items
	}
	b.warn("The system record couldn't be refreshed, so the backup holds the last one: %v", err)
	rec, lerr := record.Load(b.opt.Dirs.RecordDir())
	if lerr != nil {
		return nil
	}
	return rec.Items
}

// restic backs up the targets and the record, then applies retention.
func (b *run) restic(ctx context.Context, excludeFile string) error {
	opt := b.opt
	e := *opt.Engine
	if opt.Inhibitor != "" {
		e.Runner = inhibited(opt.Engine.Runner, opt.Inhibitor)
	}
	// A backup cut off by a shutdown leaves its lock behind; unlock removes only stale locks.
	if r, err := e.Run(ctx, "unlock"); err == nil && r.Code != 0 {
		b.log.Info("restic couldn't check for stale locks: %s", lastLine(r.Stderr))
	}

	paths := make([]string, 0, len(b.res.Targets)+1)
	for _, t := range b.res.Targets {
		paths = append(paths, opt.System.real(t))
	}
	if _, err := os.Stat(opt.Dirs.RecordDir()); err == nil {
		paths = append(paths, opt.Dirs.RecordDir())
	}
	if len(paths) == 0 {
		return &Problem{Cause: "There is nothing to back up: none of the folders to back up exist."}
	}
	args := []string{
		"backup", "--json", "--one-file-system", "--exclude-caches", "--exclude-file", excludeFile,
		"--host", opt.Machine, "--group-by", "host", "--tag", Tag,
	}
	args = append(args, paths...)
	b.log.Info("Backing up to %s", opt.Config.Repository.URL)
	pw := newProgressWriter(opt.Progress, opt.ProgressEvery, b.log)
	r, err := e.Live(ctx, pw, args...)
	pw.finish()
	if err != nil {
		return asProblem(err)
	}
	out := pw.output()
	b.res.Summary = out.Summary
	if out.Summary != nil {
		b.res.Run.Snapshot = out.Summary.SnapshotID
	}
	switch {
	case r.Code == 3:
		b.warn("Some files couldn't be read and aren't in this backup (restic named them above); the rest is saved")
	case r.Code != 0:
		return asProblem(r.Err())
	}
	if out.Summary != nil {
		b.log.Info("Snapshot %s saved: %d new and %d changed file(s), %s added", shortID(out.Summary.SnapshotID),
			out.Summary.FilesNew, out.Summary.FilesChanged, HumanBytes(out.Summary.DataAddedPacked))
	}
	b.retention(ctx, &e)
	return nil
}

// retention forgets old snapshots of this machine and prunes the repository every
// retention.prune_every_days. Failures are warnings: the backup itself is saved.
func (b *run) retention(ctx context.Context, e *engine.Engine) {
	opt := b.opt
	keep := keepArgs(opt.Config.Retention)
	if len(keep) == 0 {
		return
	}
	b.log.Info("Applying retention: %s", strings.Join(keep, " "))
	// The snapshot restoring as you go works from, when this machine backs up under its source
	// machine's name, is kept while offers are open (see restore.TagSource).
	args := append([]string{"forget", "--host", opt.Machine, "--group-by", "host", "--keep-tag", restore.SourceTag}, keep...)
	r, err := e.Run(ctx, args...)
	if err == nil {
		err = r.Err()
	}
	if err != nil {
		b.warn("Old backups weren't cleaned up (Carryon tries again next time): %v", err)
		return
	}
	days := opt.Config.Retention.PruneEveryDays
	if days <= 0 {
		return
	}
	now := opt.Now()
	last, ok := state.LastPrune(opt.Dirs)
	if !ok {
		// The first backup has nothing to prune: start the clock.
		if err := state.SetLastPrune(opt.Dirs, now); err != nil {
			b.warn("Carryon couldn't note when it last pruned: %v", err)
		}
		return
	}
	if now.Sub(last) < time.Duration(days)*24*time.Hour {
		return
	}
	b.log.Info("Pruning data no backup uses any more (every %d days)", days)
	r, err = e.Run(ctx, "prune", "--max-unused", "5%")
	if err == nil {
		err = r.Err()
	}
	if err != nil {
		b.warn("Pruning failed (Carryon tries again next time): %v", err)
		return
	}
	if err := state.SetLastPrune(opt.Dirs, now); err != nil {
		b.warn("Carryon couldn't note when it last pruned: %v", err)
	}
}

// keepArgs are restic forget's --keep options for the retention settings.
func keepArgs(r config.Retention) []string {
	var out []string
	if r.KeepWithin != "" {
		out = append(out, "--keep-within", r.KeepWithin)
	}
	for _, k := range []struct {
		flag string
		n    int
	}{{"--keep-daily", r.KeepDaily}, {"--keep-weekly", r.KeepWeekly}, {"--keep-monthly", r.KeepMonthly}, {"--keep-yearly", r.KeepYearly}} {
		if k.n > 0 {
			out = append(out, k.flag, strconv.Itoa(k.n))
		}
	}
	return out
}

// finish fills in how the run ended, clears the pending marker after a backup, and writes the run
// history and status.json. Problems writing them are warnings: they don't change how the backup
// went.
func (b *run) finish(ctx context.Context, err error) {
	opt := b.opt
	r := &b.res.Run
	r.End = opt.Now()
	var p *Problem
	switch {
	case errors.As(err, &p):
		r.Result = state.ResultFailed
		r.Error, r.Fix = p.Cause, p.Fix
	case err != nil && r.Result == "":
		r.Result = state.ResultFailed
		r.Error = err.Error()
	case err == nil && r.Result == "" && len(b.warnings) > 0:
		r.Result = state.ResultWarning
	case err == nil && r.Result == "":
		r.Result = state.ResultOK
	}
	if r.Succeeded() {
		// The backup captured every change made before it started.
		if _, err := state.ClearPending(opt.Dirs, r.Start); err != nil {
			b.warn("Carryon couldn't clear the note that a backup is owed: %v", err)
		}
	}
	r.Error, r.Fix = opt.Redact(r.Error), opt.Redact(r.Fix)
	for _, w := range b.warnings {
		r.Warnings = append(r.Warnings, opt.Redact(w))
	}
	for _, l := range b.res.LeftOut {
		r.Notes = append(r.Notes, opt.Redact(l.String()))
	}
	Record(ctx, opt.Dirs, *r, opt.Machine, opt.Timers, opt.Log)
}

// Record adds a run to the history and rewrites status.json, warning about what fails.
func Record(ctx context.Context, d paths.Dirs, r state.Run, machine string, timers state.Timers, log Logger) {
	if log == nil {
		log = nopLogger{}
	}
	if err := state.AppendRun(d, r); err != nil {
		log.Warn("The run history wasn't updated: %v", err)
	}
	s, err := state.BuildStatus(ctx, d, machine, timers, r.End)
	if err == nil {
		err = state.WriteStatus(d, s)
	}
	if err != nil {
		log.Warn("The status file wasn't updated: %v", err)
		return
	}
	if err := report.UpdateLogin(d, s, loginOptions(d), r.End); err != nil {
		log.Warn("The login message wasn't updated: %v", err)
	}
}

// loginOptions reads the login message's settings. Unreadable settings leave it on with the
// default age limit, as carryon status reads them.
func loginOptions(d paths.Dirs) report.LoginOptions {
	opt := report.LoginOptions{Enabled: true, MaxAge: report.DefaultMaxAge}
	if cfg, _, err := config.Load(d.ConfigFile()); err == nil {
		opt.Enabled = cfg.Notify.LoginMessage
		if cfg.Verify.MaxAgeHours > 0 {
			opt.MaxAge = time.Duration(cfg.Verify.MaxAgeHours) * time.Hour
		}
	}
	return opt
}

// asProblem turns a restic error into a Problem.
func asProblem(err error) error {
	var re *engine.Error
	if errors.As(err, &re) {
		return &Problem{Cause: re.Problem.Cause, Fix: re.Problem.Fix}
	}
	return &Problem{Cause: err.Error()}
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}

func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

// HumanBytes writes a size the way people read it: "1.5 GiB".
func HumanBytes(n uint64) string { return ui.Bytes(n) }
