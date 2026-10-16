package verify

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// Level is how a check came out.
type Level string

// Levels, from fine to bad. Only a problem makes verify fail.
const (
	OK      Level = "ok"
	Info    Level = "info"    // worth knowing, not a check
	Warning Level = "warning" // needs a look, but the backup works
	Problem Level = "problem" // the backup isn't current, complete or restorable
)

// Line is one check's outcome.
type Line struct {
	Level   Level    `json:"level"`
	Text    string   `json:"text"`
	Details []string `json:"details,omitempty"` // what the check found, one entry per line
	Fix     string   `json:"fix,omitempty"`     // what to do about it
}

// Section is a group of checks.
type Section struct {
	Title string `json:"title"`
	Lines []Line `json:"lines"`
}

// Report is the outcome of a verify run.
type Report struct {
	Sections []Section `json:"sections"`
	Problems int       `json:"problems"`
	Warnings int       `json:"warnings"`
	Deep     bool      `json:"deep"`
	Full     bool      `json:"full"`
}

// OK reports whether nothing is wrong (warnings are fine).
func (r *Report) OK() bool { return r.Problems == 0 }

func (r *Report) section(title string) {
	r.Sections = append(r.Sections, Section{Title: title})
}

func (r *Report) add(l Line) {
	if len(r.Sections) == 0 {
		r.section("")
	}
	s := &r.Sections[len(r.Sections)-1]
	s.Lines = append(s.Lines, l)
	switch l.Level {
	case Problem:
		r.Problems++
	case Warning:
		r.Warnings++
	}
}

func (r *Report) ok(text string, a ...any) { r.add(Line{Level: OK, Text: fmt.Sprintf(text, a...)}) }

func (r *Report) info(text string, a ...any) {
	r.add(Line{Level: Info, Text: fmt.Sprintf(text, a...)})
}

// check adds a line that is ok when bad is empty and at level otherwise, with bad as details.
func (r *Report) check(level Level, bad []string, okText, badText, fix string) {
	if len(bad) == 0 {
		r.add(Line{Level: OK, Text: okText})
		return
	}
	r.add(Line{Level: level, Text: badText, Details: bad, Fix: fix})
}

// Lines lists every line at a level, in order.
func (r *Report) Lines(level Level) []Line {
	var out []Line
	for _, s := range r.Sections {
		for _, l := range s.Lines {
			if l.Level == level {
				out = append(out, l)
			}
		}
	}
	return out
}

// maxDetails is how many details a line shows in the text report; --json has them all.
const maxDetails = 15

// Write prints the report for people. color adds terminal colours to the level marks.
func (r *Report) Write(w io.Writer, color bool) {
	mark := func(l Level) string {
		text, code := "ok  ", "1;32"
		switch l {
		case Info:
			text, code = "info", "2"
		case Warning:
			text, code = "warn", "1;33"
		case Problem:
			text, code = "FAIL", "1;31"
		}
		if color {
			return "\033[" + code + "m" + text + "\033[0m"
		}
		return text
	}
	for i, s := range r.Sections {
		if i > 0 {
			fmt.Fprintln(w)
		}
		if color {
			fmt.Fprintf(w, "\033[1m%s\033[0m\n", s.Title)
		} else {
			fmt.Fprintln(w, s.Title)
		}
		for _, l := range s.Lines {
			fmt.Fprintf(w, "  %s  %s\n", mark(l.Level), l.Text)
			for n, d := range l.Details {
				if n == maxDetails {
					fmt.Fprintf(w, "          ... and %d more\n", len(l.Details)-maxDetails)
					break
				}
				fmt.Fprintf(w, "          %s\n", d)
			}
			if l.Fix != "" {
				for _, f := range strings.Split(l.Fix, "\n") {
					fmt.Fprintf(w, "          → %s\n", f)
				}
			}
		}
	}
	fmt.Fprintln(w)
	switch {
	case r.Problems > 0:
		fmt.Fprintf(w, "%s, %s.\n", count(r.Problems, "problem"), count(r.Warnings, "warning"))
	case r.Warnings > 0:
		fmt.Fprintf(w, "All good, with %s.\n", count(r.Warnings, "warning"))
	default:
		fmt.Fprintln(w, "All good.")
	}
}

// WriteJSON prints the report as JSON.
func (r *Report) WriteJSON(w io.Writer) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(r)
}

func count(n int, word string) string {
	if n == 1 {
		return "1 " + word
	}
	return fmt.Sprintf("%d %ss", n, word)
}
