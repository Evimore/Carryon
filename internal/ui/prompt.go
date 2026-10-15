package ui

// This file holds the prompt API that restore, init and the inbox use to ask the user things.
//
// Callers build a question (Checklist, Menu, Confirm, Input, Password) and hand it to a Prompter.
// There are three Prompters:
//
//   - Terminal asks in the terminal: full-screen lists and forms, or plain line-by-line prompts
//     for screen readers and TERM=dumb.
//   - Defaults never asks: it takes each question's default, and fails with an error that names
//     the flag to use when a question has no safe default. It's used when stdin isn't a terminal
//     and for --defaults.
//   - Script answers from a list and records what was asked, for other packages' tests.
//
// New picks Terminal or Defaults for the running program.

import (
	"errors"
	"fmt"
	"os"
	"slices"

	"github.com/charmbracelet/x/term"
)

// Prompter asks the user questions.
type Prompter interface {
	// Checklist lets the user tick groups, and single items inside them.
	Checklist(Checklist) (Selection, error)
	// Menu lets the user pick one option and returns its Value.
	Menu(Menu) (string, error)
	// Confirm asks a yes/no question.
	Confirm(Confirm) (bool, error)
	// Input asks for a line of text.
	Input(Input) (string, error)
	// Password asks for a secret without showing it.
	Password(Password) (string, error)
}

// ErrCancelled is returned when the user backs out of a question (Esc, Ctrl+C, q, or end of input).
var ErrCancelled = errors.New("cancelled")

// NeedsAnswerError is returned by Defaults for a question that has no safe default.
type NeedsAnswerError struct {
	Question string // the question's title
	Flag     string // the flag that answers it instead, such as "--yes"; may be empty
}

func (e *NeedsAnswerError) Error() string {
	if e.Flag == "" {
		return fmt.Sprintf("Carryon can't answer %q on its own: run it in a terminal to answer it", e.Question)
	}
	return fmt.Sprintf("Carryon can't answer %q on its own: run it in a terminal, or use %s", e.Question, e.Flag)
}

// Group is one line of a checklist, such as a bundle, with optional items inside.
type Group struct {
	ID      string
	Title   string // "OBS Studio"
	Summary string // what's inside: "3 packages, 2 settings files"
	Reason  string // why it starts ticked or not: "same machine", "needs an NVIDIA card"
	On      bool   // ticked to start with
	Items   []Item // what the user can untick one by one; may be empty
}

// Item is a single thing inside a group, such as a file or a package.
type Item struct {
	ID    string
	Label string // "/etc/pacman.conf (changed)"
	On    bool   // ticked to start with
}

// Checklist asks the user to tick groups and, optionally, single items inside them.
type Checklist struct {
	Title  string  // "Restore from desktop onto laptop"
	Text   string  // a line or two of explanation under the title
	Groups []Group // in the order shown
	Flag   string  // the flag that chooses without asking, for the help text; may be empty
}

// Selection is what the user ticked.
type Selection struct {
	Groups   []string // IDs of ticked groups, in list order
	Unticked []string // IDs of unticked items, in any group, in list order
}

// HasGroup reports whether the group with this ID is ticked.
func (s Selection) HasGroup(id string) bool { return slices.Contains(s.Groups, id) }

// HasItem reports whether the item with this ID is ticked inside the ticked group.
func (s Selection) HasItem(group, item string) bool {
	return s.HasGroup(group) && !slices.Contains(s.Unticked, item)
}

// Defaults returns the checklist's starting ticks.
func (c Checklist) Defaults() Selection {
	return selectionOf(c.Groups)
}

func selectionOf(groups []Group) Selection {
	var s Selection
	for _, g := range groups {
		if g.On {
			s.Groups = append(s.Groups, g.ID)
		}
		for _, it := range g.Items {
			if !it.On {
				s.Unticked = append(s.Unticked, it.ID)
			}
		}
	}
	return s
}

// Option is one choice in a menu.
type Option struct {
	Value string // what Menu returns
	Label string // what the user sees; Value when empty
	Note  string // shown next to the label: "newest backup 2026-10-14 03:00"
}

func (o Option) label() string {
	if o.Label == "" {
		return o.Value
	}
	return o.Label
}

// Menu asks the user to pick one option.
type Menu struct {
	Title   string
	Text    string
	Options []Option
	Default string // the Value picked to start with; empty means the question has no default
	Flag    string // the flag that answers it instead, such as "--from"
}

// Confirm asks a yes/no question.
type Confirm struct {
	Title   string
	Text    string
	Default bool
	// NoDefault means Default is only where the cursor starts: without a terminal the question
	// fails and names Flag instead of answering Default. Use it for "Apply this plan?".
	NoDefault bool
	Flag      string
}

// Input asks for a line of text.
type Input struct {
	Title       string
	Text        string
	Placeholder string
	Default     string             // the starting value; without a terminal it's the answer
	Validate    func(string) error // optional; the error is shown and the user asked again
	Flag        string
}

// Password asks for a secret. It never has a default.
type Password struct {
	Title    string
	Text     string
	Twice    bool               // ask again to catch typos
	Validate func(string) error // optional
	Flag     string             // such as "--password-file"
}

// Options choose how New asks.
type Options struct {
	// Defaults answers every question with its default, as for --defaults.
	Defaults bool
	// Accessible asks with plain line-by-line prompts. It's also turned on by TERM=dumb and by
	// the ACCESSIBLE environment variable.
	Accessible bool
	// In and Out default to stdin and stdout.
	In  *os.File
	Out *os.File
}

// New returns a Terminal when both In and Out are terminals and Defaults isn't set, and a
// Defaults prompter otherwise.
func New(o Options) Prompter {
	if o.In == nil {
		o.In = os.Stdin
	}
	if o.Out == nil {
		o.Out = os.Stdout
	}
	if o.Defaults || !term.IsTerminal(o.In.Fd()) || !term.IsTerminal(o.Out.Fd()) {
		return Defaults{}
	}
	accessible := o.Accessible || os.Getenv("TERM") == "dumb" || os.Getenv("ACCESSIBLE") != ""
	return NewTerminal(o.In, o.Out, accessible)
}

// Defaults answers every question with its default without asking.
type Defaults struct{}

// Checklist returns the starting ticks.
func (Defaults) Checklist(c Checklist) (Selection, error) { return c.Defaults(), nil }

// Menu returns the default option, or fails when there is none.
func (Defaults) Menu(m Menu) (string, error) {
	for _, o := range m.Options {
		if m.Default != "" && o.Value == m.Default {
			return o.Value, nil
		}
	}
	return "", &NeedsAnswerError{Question: m.Title, Flag: m.Flag}
}

// Confirm returns the default answer, or fails when the question has NoDefault.
func (Defaults) Confirm(c Confirm) (bool, error) {
	if c.NoDefault {
		return false, &NeedsAnswerError{Question: c.Title, Flag: c.Flag}
	}
	return c.Default, nil
}

// Input returns the default value, or fails when it's empty or doesn't pass Validate.
func (Defaults) Input(i Input) (string, error) {
	if i.Default == "" || (i.Validate != nil && i.Validate(i.Default) != nil) {
		return "", &NeedsAnswerError{Question: i.Title, Flag: i.Flag}
	}
	return i.Default, nil
}

// Password always fails: a password has no default.
func (Defaults) Password(p Password) (string, error) {
	return "", &NeedsAnswerError{Question: p.Title, Flag: p.Flag}
}

// Compile-time checks.
var (
	_ Prompter = Defaults{}
	_ Prompter = (*Terminal)(nil)
	_ Prompter = (*Script)(nil)
)
