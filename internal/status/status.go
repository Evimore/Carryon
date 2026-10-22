// Package status builds what `carryon status` shows: the summary in status.json, what in it needs
// attention, and, for root, the latest record changes and the snapshot list. It reads nothing
// itself; the command gathers the inputs. See docs/design.md, "Running by itself".
package status

import (
	"fmt"
	"sort"
	"time"

	"github.com/Evimore/carryon/internal/engine"
	"github.com/Evimore/carryon/internal/record"
	"github.com/Evimore/carryon/internal/state"
)

// Format is the version of the JSON report's layout. Fields are only ever added.
const Format = 1

// KindVerify is the run kind of carryon verify, the same as state.KindVerify.
const KindVerify = "verify"

// The report's overall state.
const (
	StateOK        = "ok"         // nothing needs attention
	StateAttention = "attention"  // something does: see Problems
	StateNotSetUp  = "not_set_up" // backups aren't set up on this machine
)

// DefaultMaxAge is how old the newest backup may be when the config doesn't say (verify.max_age_hours).
const DefaultMaxAge = 48 * time.Hour

// Input is what the command found.
type Input struct {
	Status    state.Status // status.json, when Found
	Found     bool         // status.json was there
	NotSetUp  bool         // init hasn't recorded a destination
	MaxAge    time.Duration
	Now       time.Time
	Runs      []state.Run     // the run history, oldest first, when readable: finds a check older than Recent
	Changes   []record.Commit // the record's latest changes; nil when not read (not root)
	Snapshots *Snapshots      // nil unless --snapshots
	// RestoringFlag is the restoring flag's path, named in the fix for a restore left behind.
	RestoringFlag string
}

// Snapshots is the repository's snapshot list, or why it wasn't read.
type Snapshots struct {
	Machines []Machine `json:"machines"`
	Error    string    `json:"error,omitempty"` // why the list couldn't be read
	Fix      string    `json:"fix,omitempty"`
}

// Machine is one machine's snapshots.
type Machine struct {
	Name   string    `json:"name"`
	Count  int       `json:"count"`
	Newest time.Time `json:"newest"`
	Oldest time.Time `json:"oldest"`
}

// ByMachine groups a snapshot list (oldest first) by machine, in name order.
func ByMachine(snaps []engine.Snapshot) []Machine {
	byName := map[string]*Machine{}
	var names []string
	for _, s := range snaps {
		m, ok := byName[s.Hostname]
		if !ok {
			m = &Machine{Name: s.Hostname, Oldest: s.Time, Newest: s.Time}
			byName[s.Hostname] = m
			names = append(names, s.Hostname)
		}
		m.Count++
		if s.Time.Before(m.Oldest) {
			m.Oldest = s.Time
		}
		if s.Time.After(m.Newest) {
			m.Newest = s.Time
		}
	}
	sort.Strings(names)
	out := make([]Machine, 0, len(names))
	for _, n := range names {
		out = append(out, *byName[n])
	}
	return out
}

// What a problem is about, for Problem.Kind, so the user agent can tell them apart without reading
// their text.
const (
	ProblemNotSetUp       = "not_set_up"      // init hasn't run
	ProblemRestoring      = "restoring"       // automatic backups are paused for a restore
	ProblemNoBackup       = "no_backup"       // no backup has run or finished yet
	ProblemBackupFailed   = "backup_failed"   // the last backup failed: LastBackup
	ProblemBackupWarnings = "backup_warnings" // the last backup had warnings: LastBackup
	ProblemBackupOld      = "backup_old"      // the newest backup is too old: LastSuccess
	ProblemCheckFailed    = "check_failed"    // the last check found problems: LastCheck
	ProblemSnapshots      = "snapshots"       // the snapshot list couldn't be read
	ProblemStale          = "stale"           // status.json is old, so the timer may not run
)

// Problem is something that needs the user's attention.
type Problem struct {
	Text    string   `json:"text"`
	Fix     string   `json:"fix,omitempty"`     // what to do about it
	Details []string `json:"details,omitempty"` // the warnings, for a backup that had some
	Kind    string   `json:"kind"`              // what it is about: ProblemBackupFailed...
}

// Report is the status as `carryon status --json` prints it. The field names are stable.
type Report struct {
	Format      int             `json:"format"`
	State       string          `json:"state"`   // StateOK, StateAttention or StateNotSetUp
	Summary     string          `json:"summary"` // one line, for status bars
	Problems    []Problem       `json:"problems"`
	Machine     string          `json:"machine,omitempty"`
	Updated     *time.Time      `json:"updated"` // when status.json was written; null when there is none
	Stale       bool            `json:"stale"`   // it is older than max_age, so it may be out of date
	LastBackup  *state.Run      `json:"last_backup"`
	LastSuccess *state.Run      `json:"last_success"`
	LastCheck   *state.Run      `json:"last_check"`
	LastCapture *time.Time      `json:"last_capture"`
	Next        []state.NextRun `json:"next"` // timers still to fire, soonest first
	Pending     *state.Pending  `json:"pending"`
	Restoring   bool            `json:"restoring"`
	Inbox       int             `json:"inbox"`
	Changes     []record.Commit `json:"changes,omitempty"`
	Snapshots   *Snapshots      `json:"snapshots,omitempty"`

	now   time.Time
	login string // the most important problem, for the login message
}

// NeedsAttention reports whether something in the report does.
func (r Report) NeedsAttention() bool { return r.State != StateOK }

// Build turns what the command found into a report.
func Build(in Input) Report {
	if in.MaxAge <= 0 {
		in.MaxAge = DefaultMaxAge
	}
	s := in.Status
	r := Report{
		Format:    Format,
		State:     StateOK,
		Problems:  []Problem{},
		Next:      []state.NextRun{},
		Changes:   in.Changes,
		Snapshots: in.Snapshots,
		now:       in.Now,
	}
	if in.Found {
		r.Machine = s.Machine
		updated := s.Updated
		r.Updated = &updated
		r.Stale = in.Now.Sub(s.Updated) > in.MaxAge
		r.LastBackup, r.LastSuccess, r.LastCapture = s.LastBackup, s.LastSuccess, s.LastCapture
		r.LastCheck = lastCheck(s.Recent, in.Runs)
		for _, n := range s.Next {
			if n.At.After(in.Now) {
				r.Next = append(r.Next, n)
			}
		}
		r.Inbox = s.Inbox
	}
	r.Pending, r.Restoring = s.Pending, s.Restoring // read from their flags, status.json or not

	if in.NotSetUp {
		r.State = StateNotSetUp
		r.Summary = "Backups aren't set up"
		r.login = "backups aren't set up on this machine"
		r.add(Problem{Kind: ProblemNotSetUp, Text: "Backups aren't set up on this machine yet.", Fix: "Set them up with: sudo carryon init"})
		if !in.Found {
			return r
		}
	}
	if r.Restoring {
		fix := "Backups start again when the restore finishes."
		if in.RestoringFlag != "" {
			fix += " If no restore is running, resume them with: sudo rm " + in.RestoringFlag
		}
		r.add(Problem{Kind: ProblemRestoring, Text: "Automatic backups are paused while a restore is in progress.", Fix: fix})
		r.summary("Backups paused for a restore", "automatic backups are paused for a restore")
	}
	if !in.Found {
		r.add(Problem{
			Kind: ProblemNoBackup,
			Text: "No backup has run yet, so there is no status to show.",
			Fix:  "The first one runs at the next scheduled time; to run it now: sudo carryon backup",
		})
		r.summary("No backup yet", "no backup has run yet")
		return r
	}

	if b := r.LastBackup; b != nil {
		switch b.Result {
		case state.ResultFailed:
			fix := b.Fix
			if fix == "" {
				fix = "Run the backup by hand to see the whole error: sudo carryon backup"
			}
			r.add(Problem{Kind: ProblemBackupFailed, Text: "The last backup failed: " + orDefault(b.Error, "no reason was recorded."), Fix: fix})
			r.summary("Backup failed", "the last backup failed ("+r.when(b.End)+")")
		case state.ResultWarning:
			r.add(Problem{
				Kind:    ProblemBackupWarnings,
				Text:    fmt.Sprintf("The last backup finished with %s:", plural(len(b.Warnings), "warning")),
				Details: b.Warnings,
			})
			r.summary("Backup had warnings", fmt.Sprintf("the last backup had %s (%s)", plural(len(b.Warnings), "warning"), r.when(b.End)))
		}
	}
	switch {
	case r.LastSuccess == nil:
		r.add(Problem{Kind: ProblemNoBackup, Text: "No backup has finished yet.", Fix: "Run one now with: sudo carryon backup"})
		r.summary("No backup yet", "no backup has finished yet")
	case in.Now.Sub(r.LastSuccess.End) > in.MaxAge:
		age := in.Now.Sub(r.LastSuccess.End)
		fix := "Run one now with: sudo carryon backup"
		if b := r.LastBackup; b != nil && b.Result == state.ResultWaiting && b.Fix != "" {
			fix = b.Fix
		}
		r.add(Problem{Kind: ProblemBackupOld, Text: fmt.Sprintf("The newest backup is %s old, more than the %s allowed.", Duration(age), Duration(in.MaxAge)), Fix: fix})
		r.summary("Backup "+Duration(age)+" old", fmt.Sprintf("the newest backup is from %s, more than %s ago", r.when(r.LastSuccess.End), Duration(in.MaxAge)))
	}
	if c := r.LastCheck; c != nil && c.Result == state.ResultFailed {
		r.add(Problem{
			Kind: ProblemCheckFailed,
			Text: "The last check found problems: " + orDefault(c.Error, "no reason was recorded."),
			Fix:  orDefault(c.Fix, "See them with: sudo carryon verify"),
		})
		r.summary("Check failed", "the last check found problems ("+r.when(c.End)+")")
	}
	if sn := r.Snapshots; sn != nil && sn.Error != "" {
		r.add(Problem{Kind: ProblemSnapshots, Text: "Carryon didn't read the snapshots: " + sn.Error, Fix: sn.Fix})
		r.summary("Snapshots not read", "the snapshots couldn't be read")
	}
	if r.Stale {
		r.add(Problem{
			Kind: ProblemStale,
			Text: fmt.Sprintf("The status was last written %s ago, so it may be out of date.", Duration(in.Now.Sub(s.Updated))),
			Fix:  "Check that the backup timer runs: systemctl status carryon-backup.timer",
		})
		r.summary("Status out of date", "the status may be out of date")
	}
	if r.Summary == "" {
		r.Summary = "Backed up " + Ago(in.Now.Sub(r.LastSuccess.End))
	}
	return r
}

// add records a problem.
func (r *Report) add(p Problem) {
	r.Problems = append(r.Problems, p)
	if r.State == StateOK {
		r.State = StateAttention
	}
}

// summary sets the one-line summary, and the login message's wording of the same problem, unless
// an earlier, more important problem set them.
func (r *Report) summary(s, login string) {
	if r.Summary == "" {
		r.Summary = s
	}
	if r.login == "" {
		r.login = login
	}
}

// when says when something happened as a date and time, which stays true after the line is
// written: "Oct 18 14:02".
func (r *Report) when(t time.Time) string {
	return t.In(r.now.Location()).Format("Jan 2 15:04")
}

// lastCheck is the newest verify run: from status.json's recent runs, or from the run history when
// later runs pushed it out of them.
func lastCheck(recent, history []state.Run) *state.Run {
	for _, run := range recent {
		if run.Kind == KindVerify {
			return &run
		}
	}
	for i := len(history) - 1; i >= 0; i-- {
		if run := history[i]; run.Kind == KindVerify {
			run.Invocation = ""
			return &run
		}
	}
	return nil
}

// Duration says how long d is, roughly: "5 minutes", "3 hours", "2 days".
func Duration(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "less than a minute"
	case d < time.Hour:
		return plural(int(d/time.Minute), "minute")
	case d < 48*time.Hour:
		return plural(int(d/time.Hour), "hour")
	default:
		return plural(int(d/(24*time.Hour)), "day")
	}
}

// Ago says how long ago something was that happened d before now.
func Ago(d time.Duration) string {
	switch {
	case d < 0:
		return "in " + Duration(-d)
	case d < time.Minute:
		return "just now"
	}
	return Duration(d) + " ago"
}

func plural(n int, word string) string {
	if n == 1 {
		return "1 " + word
	}
	return fmt.Sprintf("%d %ss", n, word)
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}
