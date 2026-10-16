package verify

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Evimore/carryon/internal/backup"
	"github.com/Evimore/carryon/internal/record"
	"github.com/Evimore/carryon/internal/state"
)

// checkRecord checks the system record: there, complete, fresh, and matching the machine. It
// returns false when there is no record to check further.
func (v *verifier) checkRecord(ctx context.Context) bool {
	r := v.rep
	r.section("The system record")
	dir := v.opt.Dirs.RecordDir()
	rec, err := record.Load(dir)
	if errors.Is(err, fs.ErrNotExist) {
		r.add(Line{Level: Problem, Text: "There is no system record at " + dir + ".",
			Fix: "Make one with: sudo carryon backup"})
		return false
	}
	if err != nil {
		r.add(Line{Level: Problem, Text: "The system record can't be read: " + err.Error(),
			Fix: "Make a new one with: sudo carryon backup"})
		return false
	}
	v.rec = rec
	if rec.Meta.Complete {
		r.ok("The record is complete.")
	} else {
		r.add(Line{Level: Problem, Text: "The record is incomplete: it was captured without root, so some system files are missing.",
			Fix: "Capture it again as root: sudo carryon backup"})
	}

	if data, err := os.ReadFile(v.opt.Dirs.RestoringFlag()); err == nil {
		since := strings.TrimSpace(string(data))
		r.add(Line{Level: Problem,
			Text: "Automatic backups are paused: a restore started " + since + " never finished.",
			Fix:  "Finish it with: sudo carryon restore\nor start backups again with: sudo rm " + v.opt.Dirs.RestoringFlag()})
	}

	now := v.opt.Now()
	if t, ok := lastCapture(dir); ok {
		text := "Last captured " + stamp(t) + " (" + ago(now, t) + ")."
		if now.Sub(t) < v.maxAge() {
			r.ok("%s", text)
		} else {
			r.add(Line{Level: Problem, Text: text + " That's older than verify.max_age_hours.",
				Fix: "Check that the daily backup runs (carryon status), or run: sudo carryon backup"})
		}
	} else {
		r.add(Line{Level: Problem, Text: "The record doesn't say when it was captured.",
			Fix: "Capture it again: sudo carryon backup"})
	}

	v.checkDrift(ctx)
	return true
}

// lastCapture reads when the record was last published.
func lastCapture(dir string) (time.Time, bool) {
	data, err := os.ReadFile(filepath.Join(dir, record.LastCaptureFile))
	if err != nil {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339, strings.TrimSpace(string(data)))
	return t, err == nil
}

// checkDrift captures the machine into a temporary folder and compares it with the record.
//
// The capture is the quick one the pacman hook makes, so package files edited outside their
// backup= list (found only by the nightly capture) are left out of the comparison.
func (v *verifier) checkDrift(ctx context.Context) {
	out := filepath.Join(v.tmp, "now")
	v.log.Info("Comparing the record with the running system")
	capd, err := backup.Capture(ctx, backup.CaptureOptions{
		Dirs: v.opt.Dirs, Config: v.opt.Config, System: v.opt.System, Out: out,
		ExcludeFile: filepath.Join(v.tmp, "excludes"), Log: nopLogger{}, Now: v.opt.Now,
	})
	if err != nil {
		v.rep.add(Line{Level: Warning, Text: "The running system couldn't be compared with the record: " + err.Error()})
		return
	}
	v.setExcludes(capd.Excludes)
	drift := record.ChangeLines(record.Diff(withoutEdited(v.rec.Items), withoutEdited(capd.Result.Items)))
	if len(drift) == 0 {
		v.rep.ok("The record matches the running system.")
		return
	}
	v.rep.add(Line{Level: Warning, Text: count(len(drift), "change") + " since the last capture; the next backup picks them up.",
		Details: drift, Fix: "To save them now: sudo carryon backup"})
}

// withoutEdited leaves out the files only the nightly capture looks for.
func withoutEdited(items []record.Item) []record.Item {
	out := make([]record.Item, 0, len(items))
	for _, it := range items {
		if it.Type == record.TypeFile && it.Kind == record.KindEdited {
			continue
		}
		out = append(out, it)
	}
	return out
}

// lastBackup is when the last successful backup of this machine ended, from the run history.
func lastBackup(runs []state.Run) (time.Time, bool) {
	for i := len(runs) - 1; i >= 0; i-- {
		if runs[i].Kind == state.KindBackup && runs[i].Succeeded() {
			return runs[i].End, true
		}
	}
	return time.Time{}, false
}
