package state

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Evimore/carryon/internal/paths"
)

// What a run was.
const (
	KindBackup  = "backup"  // carryon backup, from the timer, settle or by hand
	KindCapture = "capture" // the record alone: carryon capture or the pacman hook
	KindVerify  = "verify"  // carryon verify, from the timer or by hand
)

// What started a run.
const (
	TriggerManual = "manual" // someone ran the command
	TriggerTimer  = "timer"  // carryon-backup.timer
	TriggerSettle = "settle" // carryon-settle.timer, after a change or while waiting for the destination
	TriggerHook   = "hook"   // the pacman hook
)

// How a run ended.
const (
	ResultOK      = "ok"      // everything worked
	ResultWarning = "warning" // done, but something needs a look (unreadable files, retention failed...)
	ResultWaiting = "waiting" // the destination wasn't there, so nothing was written; it runs again when it is
	ResultPaused  = "paused"  // a restore is in progress, so the automatic run did nothing
	ResultSkipped = "skipped" // another backup was running
	ResultFailed  = "failed"  // it didn't do its job
)

// Run is one line of the run history. It holds no secrets: status.json, which anyone may read,
// is built from it.
type Run struct {
	Kind       string    `json:"kind"`
	Trigger    string    `json:"trigger"`
	Unit       string    `json:"unit,omitempty"`       // the systemd unit that ran it
	Invocation string    `json:"invocation,omitempty"` // that unit run's invocation ID
	Machine    string    `json:"machine,omitempty"`
	Start      time.Time `json:"start"`
	End        time.Time `json:"end"`
	Result     string    `json:"result"`
	Error      string    `json:"error,omitempty"` // what went wrong, short
	Fix        string    `json:"fix,omitempty"`   // what to do about it
	Snapshot   string    `json:"snapshot,omitempty"`
	Changes    int       `json:"changes,omitempty"` // items that changed in the record
	Warnings   []string  `json:"warnings,omitempty"`
	Notes      []string  `json:"notes,omitempty"`    // worth knowing, not a problem (filesystems left out...)
	Packages   []string  `json:"packages,omitempty"` // the packages a pacman transaction changed
}

// Succeeded reports whether the run did its job, perhaps with warnings.
func (r Run) Succeeded() bool { return r.Result == ResultOK || r.Result == ResultWarning }

// Limits that keep the history and the summary small.
const (
	maxErrorText  = 500       // characters of an error kept in a run
	maxWarnings   = 20        // warnings kept per run
	maxRunsSize   = 1 << 20   // runs.jsonl is trimmed when it grows past this
	keepRunsLines = 1000      // and keeps this many runs
	maxLineSize   = 256 << 10 // longest run line read back
)

// AppendRun adds a run to the history. The history keeps the last thousand runs or so.
func AppendRun(d paths.Dirs, r Run) error {
	r = r.trimmed()
	line, err := json.Marshal(r)
	if err != nil {
		return err
	}
	file := d.RunsFile()
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(file, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return fmt.Errorf("writing the run history: %w", err)
	}
	if _, err := f.Write(append(line, '\n')); err != nil {
		f.Close()
		return fmt.Errorf("writing the run history: %w", err)
	}
	if err := f.Close(); err != nil {
		return err
	}
	if fi, err := os.Stat(file); err == nil && fi.Size() > maxRunsSize {
		return trimRuns(file)
	}
	return nil
}

func (r Run) trimmed() Run {
	r.Error = shorten(r.Error, maxErrorText)
	r.Fix = shorten(r.Fix, maxErrorText)
	r.Warnings = shortList(r.Warnings)
	r.Notes = shortList(r.Notes)
	r.Packages = shortList(r.Packages)
	return r
}

// shortList keeps up to maxWarnings entries, each shortened.
func shortList(list []string) []string {
	if len(list) == 0 {
		return nil
	}
	out := make([]string, 0, min(len(list), maxWarnings))
	for i, s := range list {
		if i == maxWarnings-1 && len(list) > maxWarnings {
			out = append(out, fmt.Sprintf("and %d more", len(list)-i))
			break
		}
		out = append(out, shorten(s, maxErrorText))
	}
	return out
}

// shorten cuts s to n runes, marking the cut.
func shorten(s string, n int) string {
	s = strings.TrimSpace(s)
	if r := []rune(s); len(r) > n {
		return string(r[:n-1]) + "…"
	}
	return s
}

// trimRuns keeps the last keepRunsLines lines of the history.
func trimRuns(file string) error {
	data, err := os.ReadFile(file)
	if err != nil {
		return err
	}
	lines := bytes.SplitAfter(data, []byte("\n"))
	if len(lines) <= keepRunsLines {
		return nil
	}
	return writeFileAtomic(file, bytes.Join(lines[len(lines)-keepRunsLines:], nil), 0o644)
}

// ReadRuns reads the run history, oldest first. Lines that can't be read are skipped, so a damaged
// line never hides the rest. No history is no runs.
func ReadRuns(d paths.Dirs) ([]Run, error) {
	f, err := os.Open(d.RunsFile())
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading the run history: %w", err)
	}
	defer f.Close()
	var runs []Run
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), maxLineSize)
	for sc.Scan() {
		var r Run
		if json.Unmarshal(sc.Bytes(), &r) == nil && r.Kind != "" {
			runs = append(runs, r)
		}
	}
	if err := sc.Err(); err != nil {
		return runs, fmt.Errorf("reading the run history: %w", err)
	}
	return runs, nil
}

// SystemdUnit returns the systemd unit this process runs in, read from cgroup (normally
// /proc/self/cgroup), or "" when it isn't a service. carryon-notify@.service names the failed
// unit, and the run history finds that unit's run by it.
func SystemdUnit(cgroup string) string {
	data, err := os.ReadFile(cgroup)
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(data), "\n") {
		// cgroup v2: "0::/system.slice/carryon-backup.service"
		_, path, ok := strings.Cut(line, "::")
		if !ok {
			continue
		}
		base := filepath.Base(strings.TrimSpace(path))
		if strings.HasSuffix(base, ".service") {
			return base
		}
	}
	return ""
}
