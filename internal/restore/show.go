package restore

import (
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/Evimore/carryon/internal/record"
)

// Header is the plan's title and, when users' names or homes differ, where their files go.
func (p *Plan) Header(loc *time.Location) string {
	if loc == nil {
		loc = time.Local
	}
	when := ""
	if p.Snapshot != nil {
		when = ", backup of " + p.Snapshot.Time.In(loc).Format(timeFormat)
	}
	h := fmt.Sprintf("Restore plan: %s%s -> %s  (%d/%d bundles)", p.From, when, p.Onto, p.Bundles, p.OfBundles)
	for _, m := range p.Users {
		if m.Changed() {
			h += fmt.Sprintf("\n  %s's files go to %s (%s); paths inside them are adapted", m.From.Name, m.To.Name, m.To.Home)
		}
	}
	return h
}

// label is how a step's item is named in the summary.
func (p *Plan) label(s Step) string {
	name := s.Item.ID
	switch {
	case s.Item.Type == record.TypePath && s.Item.Home != "":
		name = "~/" + s.Item.Home
		if len(p.Users) > 1 && s.User != nil {
			name = "~" + s.User.Name + "/" + s.Item.Home
		}
		return name
	case s.Item.Type == record.TypeMkdir && s.Item.Rel != "":
		name = "~/" + s.Item.Rel
	case s.Item.Type == record.TypeLinger && s.User != nil:
		name = s.User.Name
	}
	if len(p.Users) > 1 && s.User != nil {
		name += " (" + s.User.Name + ")"
	}
	return name
}

// Summary writes the plan in short: one line per phase, what's skipped and what's already done.
// planFile, when set, is where the full plan was written. Ported from show_plan.
func (p *Plan) Summary(w io.Writer, planFile string, loc *time.Location) {
	fmt.Fprintf(w, "\n%s\n", p.Header(loc))
	for _, ph := range Phases {
		steps := p.Steps[ph.Phase]
		if len(steps) == 0 {
			continue
		}
		names := make([]string, len(steps))
		for i, s := range steps {
			names[i] = p.label(s)
		}
		var line string
		switch ph.Phase {
		case PhaseFiles:
			n := 0
			for _, s := range steps {
				if s.State == StateNew {
					n++
				}
			}
			line = fmt.Sprintf("%d new, %d replaced: %s", n, len(steps)-n, record.Brief(names, 5))
		case PhaseData:
			line = record.Brief(names, 6)
		case PhaseReview:
			line = record.Brief(names, 4) + "  (diffs in the plan file, never copied automatically)"
		default:
			line = record.Brief(names, 8)
		}
		fmt.Fprintf(w, "  %-15s %4d  %s\n", ph.Label, len(steps), line)
	}
	if len(p.Skipped) > 0 {
		names := make([]string, len(p.Skipped))
		for i, s := range p.Skipped {
			names[i] = fmt.Sprintf("%s (%s)", s.Item.ID, s.Why)
		}
		fmt.Fprintf(w, "  %-15s      %s\n", "Skipped", record.Brief(names, 4))
	}
	fmt.Fprintf(w, "  %-15s %4d  items need no change\n", "Already done", p.Done())
	if planFile != "" {
		fmt.Fprintf(w, "\n  Full plan with diffs: %s\n", planFile)
	}
	if len(p.NotBackedUp) > 0 {
		fmt.Fprintf(w, "\n  Not in the backup, re-download after restoring: %s\n", record.Brief(p.NotBackedUp, 6))
		fmt.Fprintf(w, "  The full list is saved to ~/%s when you apply the restore.\n", NotBackedUpMD)
	}
}

// Full is the whole plan, step by step, with diffs of the files that differ. It's written to
// restore-plan.txt.
func (p *Plan) Full(loc *time.Location) string {
	var b strings.Builder
	b.WriteString(p.Header(loc) + "\n\n")
	for _, ph := range Phases {
		for _, s := range p.Steps[ph.Phase] {
			fmt.Fprintf(&b, "[%s] %s", ph.Label, describe(s.Item))
			if s.State != "" {
				fmt.Fprintf(&b, "  (%s)", s.State)
			}
			b.WriteString("\n")
			if s.Why != "" {
				fmt.Fprintf(&b, "    NOT COPIED: %s\n", s.Why)
			}
			if s.User != nil && s.User.Name != p.recordedUser(s.Item) {
				fmt.Fprintf(&b, "    for %s\n", s.User.Name)
			}
			if s.User != nil && s.Target != "" && s.Target != s.Item.ID {
				fmt.Fprintf(&b, "    goes to %s\n", s.Target)
			}
			if s.Adapted {
				fmt.Fprintf(&b, "    paths inside are adapted: %s\n", p.adaptedHomes())
			}
			if s.Diff != "" {
				b.WriteString(s.Diff + "\n")
			}
		}
	}
	for _, s := range p.Skipped {
		fmt.Fprintf(&b, "[skipped] %s: %s\n", describe(s.Item), s.Why)
	}
	return b.String()
}

// recordedUser is whose a per-user item was on the source machine.
func (p *Plan) recordedUser(it record.Item) string {
	if it.User != "" {
		return it.User
	}
	return p.mainUser
}

// adaptedHomes lists the home rewrites, such as "/home/alice -> /home/bob".
func (p *Plan) adaptedHomes() string {
	var parts []string
	for _, m := range p.Users {
		switch {
		case m.From.Home != m.To.Home:
			parts = append(parts, m.From.Home+" -> "+m.To.Home)
		case m.From.Name != m.To.Name:
			parts = append(parts, "User="+m.From.Name+" -> User="+m.To.Name)
		}
	}
	return strings.Join(parts, ", ")
}
