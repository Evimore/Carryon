package status

import (
	"errors"
	"fmt"
	"io/fs"
	"time"

	"github.com/Evimore/carryon/internal/config"
	"github.com/Evimore/carryon/internal/paths"
	"github.com/Evimore/carryon/internal/state"
)

// ReadInput reads what anyone may read for a report: status.json, the pending and restoring flags,
// the inbox count, the run history when readable, whether init has run, and verify.max_age_hours.
// carryon status and the user agent both start from it, so they never disagree. The record's
// changes and the snapshot list, which need root, are left for the caller.
func ReadInput(d paths.Dirs, now time.Time) (Input, error) {
	in := Input{Now: now, RestoringFlag: d.RestoringFlag(), MaxAge: DefaultMaxAge}
	s, err := state.ReadStatus(d)
	switch {
	case err == nil:
		in.Status, in.Found = s, true
	case errors.Is(err, fs.ErrNotExist):
	default:
		return in, fmt.Errorf("reading the status (%v): the next backup writes it again", err)
	}
	// The flags and the inbox may be newer than the status, or there may be no status yet: the
	// pacman hook marks a backup owed without rewriting status.json, and init --connect sets the
	// restoring flag on a machine that has never backed up.
	if p, ok, err := state.ReadPending(d); err == nil {
		in.Status.Pending = nil
		if ok {
			p.Since = p.Since.UTC()
			in.Status.Pending = &p
		}
	}
	in.Status.Restoring = state.Restoring(d)
	in.Status.Inbox = state.InboxCount(d) // settling a suggestion doesn't rewrite status.json
	if runs, err := state.ReadRuns(d); err == nil {
		in.Runs = runs
	}
	_, err = state.ReadDestination(d)
	in.NotSetUp = errors.Is(err, state.ErrNotSetUp)

	cfg, _, cfgErr := config.Load(d.ConfigFile())
	if cfgErr == nil && cfg.Verify.MaxAgeHours > 0 {
		in.MaxAge = time.Duration(cfg.Verify.MaxAgeHours) * time.Hour
	}
	return in, nil
}
