package status

import (
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/Evimore/carryon/internal/state"
)

// TextOptions says how to print the report.
type TextOptions struct {
	Color bool           // colour the result words; only on a terminal, never with NO_COLOR
	Loc   *time.Location // times are shown in it; nil means local time
}

// What a run's result reads as, and its colour.
var results = map[string]struct{ text, color string }{
	state.ResultOK:      {"ok", "32"},
	state.ResultWarning: {"ok, with warnings", "33"},
	state.ResultWaiting: {"waiting for its destination", "33"},
	state.ResultPaused:  {"paused for a restore", "33"},
	state.ResultSkipped: {"skipped, another backup was running", "2"},
	state.ResultFailed:  {"failed", "1;31"},
}

// What started a run, as the user knows it.
var triggers = map[string]string{
	state.TriggerManual: "by hand",
	state.TriggerTimer:  "scheduled",
	state.TriggerSettle: "automatic",
	state.TriggerHook:   "pacman hook",
}

const labelWidth = 16

// WriteText prints the report for people.
func WriteText(w io.Writer, r Report, opt TextOptions) error {
	p := printer{w: w, opt: opt, now: r.now}
	if p.opt.Loc == nil {
		p.opt.Loc = time.Local
	}
	p.headline(r)
	if r.State == StateNotSetUp && r.Updated == nil {
		return p.err
	}
	if r.Updated != nil {
		p.line("")
		same := r.LastBackup != nil && r.LastSuccess != nil && r.LastBackup.End.Equal(r.LastSuccess.End)
		p.run("Last backup", r.LastBackup, true)
		if !same {
			p.run("Last success", r.LastSuccess, false)
		}
		p.run("Last check", r.LastCheck, true)
		if r.LastCapture != nil {
			p.field("Record updated", p.when(*r.LastCapture))
		}
		p.next(r)
		if r.Pending != nil {
			p.field("Owed", pendingText(*r.Pending, p))
		}
		if r.Inbox > 0 {
			p.field("Inbox", fmt.Sprintf("%s waiting: carryon inbox", plural(r.Inbox, "suggestion")))
		}
		p.field("Status from", p.when(*r.Updated))
	}
	if r.Changes != nil {
		p.line("")
		p.line(p.paint("1", "Recent changes to the system"))
		if len(r.Changes) == 0 {
			p.line("  none yet")
		}
		for _, c := range r.Changes {
			p.line(fmt.Sprintf("  %s  %s", c.Date.In(p.opt.Loc).Format("2006-01-02 15:04"), c.Subject))
		}
	}
	if s := r.Snapshots; s != nil {
		p.line("")
		p.line(p.paint("1", "Snapshots in the repository"))
		switch {
		case s.Error != "":
			p.line("  not read (see above)")
		case len(s.Machines) == 0:
			p.line("  none yet")
		}
		for _, m := range s.Machines {
			p.line(fmt.Sprintf("  %-20s %3d kept, newest %s, oldest %s", m.Name, m.Count,
				m.Newest.In(p.opt.Loc).Format("2006-01-02 15:04"), m.Oldest.In(p.opt.Loc).Format("2006-01-02")))
		}
	}
	return p.err
}

type printer struct {
	w   io.Writer
	opt TextOptions
	now time.Time
	err error
}

func (p *printer) line(s string) {
	if p.err == nil {
		_, p.err = fmt.Fprintln(p.w, s)
	}
}

func (p *printer) paint(code, s string) string {
	if !p.opt.Color || s == "" {
		return s
	}
	return "\033[" + code + "m" + s + "\033[0m"
}

// field prints a label and its value; further lines of the value line up under the first.
func (p *printer) field(label, value string) {
	pad := strings.Repeat(" ", labelWidth)
	lines := strings.Split(value, "\n")
	p.line(fmt.Sprintf("%-*s%s", labelWidth, label, lines[0]))
	for _, l := range lines[1:] {
		p.line(pad + l)
	}
}

func (p *printer) headline(r Report) {
	if !r.NeedsAttention() {
		p.line(p.paint("1;32", "All good: ") + strings.ToLower(r.Summary[:1]) + r.Summary[1:] + ".")
		return
	}
	p.line(p.paint("1;31", "Needs attention"))
	for _, pr := range r.Problems {
		p.line("  - " + pr.Text)
		for i, d := range pr.Details {
			if i == 5 && len(pr.Details) > 6 {
				p.line(fmt.Sprintf("      and %d more", len(pr.Details)-i))
				break
			}
			p.line("      " + d)
		}
		if pr.Fix != "" {
			p.line("    " + p.paint("2", pr.Fix))
		}
	}
}

// when shows a time and how long ago it was.
func (p *printer) when(t time.Time) string {
	return fmt.Sprintf("%s (%s)", t.In(p.opt.Loc).Format("2006-01-02 15:04"), Ago(p.now.Sub(t)))
}

// run prints one run: its result, when, what started it and the snapshot it saved; with detail,
// its error and fix too.
func (p *printer) run(label string, r *state.Run, detail bool) {
	if r == nil {
		p.field(label, "never")
		return
	}
	res, ok := results[r.Result]
	if !ok {
		res.text = r.Result
	}
	text := p.paint(res.color, res.text) + ", " + p.when(r.End)
	if t := triggers[r.Trigger]; t != "" {
		text += ", " + t
	}
	if r.Snapshot != "" && (!detail || r.Succeeded()) {
		text += "\nsnapshot " + shortID(r.Snapshot)
	}
	if detail && r.Result != state.ResultOK {
		if r.Error != "" && r.Result != state.ResultFailed && r.Result != state.ResultWarning {
			text += "\n" + r.Error
		}
		if r.Fix != "" && r.Result == state.ResultWaiting {
			text += "\n" + p.paint("2", r.Fix)
		}
	}
	p.field(label, text)
}

// next prints when the timers fire next. The settle timer is left out: it fires every few minutes
// and only does something when a backup is owed, which "Owed" shows.
func (p *printer) next(r Report) {
	var lines []string
	for _, n := range r.Next {
		name := strings.TrimSuffix(strings.TrimPrefix(n.Unit, "carryon-"), ".timer")
		switch name {
		case "settle":
			continue
		case "verify":
			name = "check"
		}
		lines = append(lines, fmt.Sprintf("%-8s%s", name, n.At.In(p.opt.Loc).Format("2006-01-02 15:04")+" ("+Ago(p.now.Sub(n.At))+")"))
	}
	if len(lines) == 0 {
		p.field("Next runs", "not known")
		return
	}
	p.field("Next runs", strings.Join(lines, "\n"))
}

func pendingText(pd state.Pending, p printer) string {
	if pd.Reason == state.ReasonWaiting {
		return "a backup, since " + p.when(pd.Since) + "\nit runs when the destination is back"
	}
	return "a backup for changes made " + p.when(pd.Since) + "\nit runs once the changes settle"
}

func shortID(id string) string { return id[:min(8, len(id))] }
