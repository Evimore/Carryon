package state

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/Evimore/carryon/internal/paths"
)

// StatusFormat is the version of status.json's layout. Readers ignore fields they don't know.
const StatusFormat = 1

// recentRuns is how many of the latest runs status.json lists.
const recentRuns = 10

// Status is status.json: a summary of recent and coming runs that anyone on the machine may read,
// so `carryon status` and the user agent work without root and after a reboot. It holds no
// secrets: no password, no repository address, only short error text.
type Status struct {
	Format      int        `json:"format"`
	Updated     time.Time  `json:"updated"`
	Machine     string     `json:"machine,omitempty"`
	LastBackup  *Run       `json:"last_backup,omitempty"`  // the latest backup, however it ended
	LastSuccess *Run       `json:"last_success,omitempty"` // the latest backup that saved a snapshot
	LastCapture *time.Time `json:"last_capture,omitempty"` // when the record was last refreshed
	Recent      []Run      `json:"recent"`                 // the latest runs, newest first
	Next        []NextRun  `json:"next,omitempty"`         // when the timers fire next
	Pending     *Pending   `json:"pending,omitempty"`      // a backup that is still owed
	Restoring   bool       `json:"restoring,omitempty"`    // automatic runs are paused
	Inbox       int        `json:"inbox"`                  // suggestions waiting for the user
}

// NextRun is when a timer fires next.
type NextRun struct {
	Unit string    `json:"unit"`
	At   time.Time `json:"at"`
}

// Timers lists Carryon's timers and when each fires next. systemd provides it; tests fake it.
type Timers func(ctx context.Context) ([]NextRun, error)

// BuildStatus summarizes the history and flags in d. timers may be nil.
func BuildStatus(ctx context.Context, d paths.Dirs, machine string, timers Timers, now time.Time) (Status, error) {
	s := Status{Format: StatusFormat, Updated: now.UTC(), Machine: machine, Recent: []Run{}}
	runs, err := ReadRuns(d)
	if err != nil {
		return s, err
	}
	for i := len(runs) - 1; i >= 0; i-- {
		r := runs[i]
		if len(s.Recent) < recentRuns {
			s.Recent = append(s.Recent, public(r))
		}
		if r.Kind == KindBackup && s.LastBackup == nil && r.Result != ResultPaused && r.Result != ResultSkipped {
			p := public(r)
			s.LastBackup = &p
		}
		if r.Kind == KindBackup && s.LastSuccess == nil && r.Succeeded() {
			p := public(r)
			s.LastSuccess = &p
		}
		if s.LastCapture == nil && r.Succeeded() && r.Kind != KindVerify {
			end := r.End.UTC()
			s.LastCapture = &end
		}
	}
	if p, ok, err := ReadPending(d); err == nil && ok {
		p.Since = p.Since.UTC()
		s.Pending = &p
	}
	s.Restoring = Restoring(d)
	s.Inbox = inboxCount(d.InboxFile())
	if timers != nil {
		if next, err := timers(ctx); err == nil {
			sort.Slice(next, func(i, j int) bool { return next[i].At.Before(next[j].At) })
			s.Next = next
		}
	}
	return s, nil
}

// public is a run as status.json shows it: everything but the systemd invocation ID.
func public(r Run) Run {
	r.Invocation = ""
	r.Start, r.End = r.Start.UTC(), r.End.UTC()
	return r
}

// inboxCount is the number of suggestions in the inbox: the length of its "entries" list, or of
// the file's list if it is one.
func inboxCount(file string) int {
	data, err := os.ReadFile(file)
	if err != nil {
		return 0
	}
	var list []json.RawMessage
	if json.Unmarshal(data, &list) == nil {
		return len(list)
	}
	var obj struct {
		Entries []json.RawMessage `json:"entries"`
	}
	if json.Unmarshal(data, &obj) == nil {
		return len(obj.Entries)
	}
	return 0
}

// RunStatusFile is status.json's place in the runtime folder. The copy in the state folder
// (paths.Dirs.StatusFile) survives reboots.
func RunStatusFile(d paths.Dirs) string { return filepath.Join(d.Run, "status.json") }

// WriteStatus writes status.json to the runtime folder and mirrors it to the state folder, both
// readable by everyone. It tries both and reports the first error.
func WriteStatus(d paths.Dirs, s Status) error {
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	var errs []error
	for _, file := range []string{RunStatusFile(d), d.StatusFile()} {
		if err := writeFileAtomic(file, data, 0o644); err != nil {
			errs = append(errs, fmt.Errorf("writing %s: %w", file, err))
		}
	}
	return errors.Join(errs...)
}

// ReadStatus reads status.json from the runtime folder, or from the state folder after a reboot.
func ReadStatus(d paths.Dirs) (Status, error) {
	var s Status
	data, err := os.ReadFile(RunStatusFile(d))
	if errors.Is(err, fs.ErrNotExist) {
		data, err = os.ReadFile(d.StatusFile())
	}
	if err != nil {
		return s, err
	}
	if err := json.Unmarshal(data, &s); err != nil {
		return s, fmt.Errorf("reading the status: %w", err)
	}
	return s, nil
}

// Refresh rebuilds status.json from the history and flags and writes it.
func Refresh(ctx context.Context, d paths.Dirs, machine string, timers Timers, now time.Time) error {
	s, err := BuildStatus(ctx, d, machine, timers, now)
	if err != nil {
		return err
	}
	return WriteStatus(d, s)
}
