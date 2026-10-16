// Package verify checks that the backup is current, complete and restorable: the record (complete,
// fresh, matching the machine), whether everything in it can be reinstalled (repository and AUR
// packages, extra repositories and their keys), whether what the machine runs is covered (units'
// programs, DKMS sources, kernel parameters, mounts, modules), and the repository (it opens, the
// newest snapshot is recent, the recovery kit is saved). The deep check also looks inside the
// latest snapshot, re-reads part of the data and restores a few files into a temporary folder.
//
// Verify never writes to the repository, except what `restic check` itself needs, and runs no
// restic command before the destination matches the one recorded at init. See docs/design.md,
// "Running by itself" and "Getting back in".
//
// It ports the prototype's cmd_verify, verify_cloud, Report, aur_info, repo_reachable, covered and
// finish (legacy/sysbackup.py).
package verify

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/Evimore/carryon/internal/backup"
	"github.com/Evimore/carryon/internal/catalog"
	"github.com/Evimore/carryon/internal/config"
	"github.com/Evimore/carryon/internal/engine"
	"github.com/Evimore/carryon/internal/paths"
	"github.com/Evimore/carryon/internal/record"
	"github.com/Evimore/carryon/internal/state"
)

// Logger reports progress. *ui.Logger provides it.
type Logger = backup.Logger

// DefaultAUR is the AUR's address; its RPC API is under /rpc.
const DefaultAUR = "https://aur.archlinux.org"

// Options configure a verify run.
type Options struct {
	Dirs   paths.Dirs
	Config config.Config
	// System is the machine: its files (under Root), programs and package manager.
	System backup.System
	// Engine runs restic on the repository.
	Engine *engine.Engine
	// Checker checks the destination against the identity recorded at init, before any restic
	// command runs.
	Checker backup.Checker
	// Machine is the name this machine's snapshots are stored under.
	Machine string
	// Deep also looks inside the latest snapshot, re-reads verify.read_subset of the data and
	// restores a few files. Full re-reads all of the data (it implies Deep).
	Deep, Full bool
	// HTTP makes the AUR and package repository requests. Nil means a client with short timeouts.
	HTTP *http.Client
	// AURBase is the AUR's address. Empty means DefaultAUR.
	AURBase string
	// Arch replaces $arch in repository Server lines. Empty means this machine's.
	Arch string
	// Trigger, Unit and Invocation say how the run started, for the run history.
	Trigger, Unit, Invocation string
	// Redact hides secrets in text that goes into the run history. Nil leaves text as is.
	Redact func(string) string
	// Timers tells status.json when the timers fire next. Nil leaves that out.
	Timers state.Timers
	Log    Logger
	Now    func() time.Time
}

// Run runs every check, records the run in the run history and status.json, and returns the
// report. The error is non-nil only when verify couldn't run at all; problems the checks find are
// in the report.
func Run(ctx context.Context, opt Options) (*Report, error) {
	v, err := newVerifier(opt)
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(v.tmp)
	start := v.opt.Now()
	v.run(ctx)
	v.record(ctx, start)
	return v.rep, nil
}

// newVerifier fills in the defaults and makes the run's temporary folder.
func newVerifier(opt Options) (*verifier, error) {
	if opt.Log == nil {
		opt.Log = nopLogger{}
	}
	if opt.Now == nil {
		opt.Now = time.Now
	}
	if opt.Redact == nil {
		opt.Redact = func(s string) string { return s }
	}
	if opt.HTTP == nil {
		opt.HTTP = &http.Client{Timeout: 20 * time.Second}
	}
	if opt.AURBase == "" {
		opt.AURBase = DefaultAUR
	}
	if opt.Arch == "" {
		opt.Arch = pacmanArch(runtime.GOARCH)
	}
	if opt.Full {
		opt.Deep = true
	}
	if opt.System.Runner == nil || opt.System.Packages == nil {
		return nil, errors.New("verify: a runner and a package manager are needed")
	}
	tmp, err := os.MkdirTemp("", "carryon-verify-")
	if err != nil {
		return nil, fmt.Errorf("making a temporary folder: %w", err)
	}
	return &verifier{opt: opt, rep: &Report{Deep: opt.Deep, Full: opt.Full}, log: opt.Log, tmp: tmp}, nil
}

// verifier is one verify run.
type verifier struct {
	opt Options
	rep *Report
	log Logger
	tmp string

	rec      *record.Record
	excluded func(string) bool // the effective exclude list, when the drift capture read it
	netSeen  bool              // some web server answered, so the machine is online
}

func (v *verifier) run(ctx context.Context) {
	if !v.checkRecord(ctx) {
		return
	}
	v.checkReinstall(ctx)
	v.checkCoverage(ctx)
	v.checkRepository(ctx)
}

// maxAge is how old the newest backup and the record may be.
func (v *verifier) maxAge() time.Duration {
	h := v.opt.Config.Verify.MaxAgeHours
	if h <= 0 {
		h = config.Default().Verify.MaxAgeHours
	}
	return time.Duration(h) * time.Hour
}

// root is where the machine's files are read from.
func (v *verifier) root() string {
	if v.opt.System.Root == "" {
		return "/"
	}
	return v.opt.System.Root
}

// isExcluded reports whether the exclude list leaves a path out.
func (v *verifier) isExcluded(p string) bool {
	return v.excluded != nil && v.excluded(p)
}

func (v *verifier) setExcludes(patterns []string) {
	if m, err := catalog.NewMatcher(patterns); err == nil {
		v.excluded = m.Excluded
	}
}

// record writes the run to the run history and refreshes status.json.
func (v *verifier) record(ctx context.Context, start time.Time) {
	opt := v.opt
	r := state.Run{
		Kind: state.KindVerify, Trigger: opt.Trigger, Unit: opt.Unit, Invocation: opt.Invocation,
		Machine: opt.Machine, Start: start, End: opt.Now(), Result: state.ResultOK,
	}
	if r.Trigger == "" {
		r.Trigger = state.TriggerManual
	}
	problems, warnings := v.rep.Lines(Problem), v.rep.Lines(Warning)
	switch {
	case len(problems) > 0:
		r.Result = state.ResultFailed
		r.Error, r.Fix = problems[0].Text, problems[0].Fix
		if len(problems) > 1 {
			r.Error += fmt.Sprintf(" (and %s)", count(len(problems)-1, "more problem"))
		}
	case len(warnings) > 0:
		r.Result = state.ResultWarning
	}
	r.Error, r.Fix = opt.Redact(r.Error), opt.Redact(r.Fix)
	for _, l := range append(problems, warnings...) {
		r.Warnings = append(r.Warnings, opt.Redact(l.Text))
	}
	backup.Record(ctx, opt.Dirs, r, opt.Machine, opt.Timers, opt.Log)
}

// pacmanArch is pacman's name for a Go architecture.
func pacmanArch(goarch string) string {
	switch goarch {
	case "amd64":
		return "x86_64"
	case "arm64":
		return "aarch64"
	case "386":
		return "i686"
	}
	return goarch
}

// ago says how long ago t was, in whole hours.
func ago(now, t time.Time) string {
	h := int(now.Sub(t).Hours())
	if h == 1 {
		return "1 hour ago"
	}
	return fmt.Sprintf("%d hours ago", h)
}

// stamp is how times are shown.
func stamp(t time.Time) string { return t.Local().Format("2006-01-02 15:04") }

func lastLines(s string, n int) []string {
	var out []string
	for _, l := range strings.Split(strings.TrimSpace(s), "\n") {
		if l = strings.TrimSpace(l); l != "" {
			out = append(out, l)
		}
	}
	if len(out) > n {
		out = out[len(out)-n:]
	}
	return out
}

type nopLogger struct{}

func (nopLogger) Info(string, ...any) {}
func (nopLogger) Warn(string, ...any) {}
