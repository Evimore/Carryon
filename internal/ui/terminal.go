package ui

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/huh"
	"github.com/charmbracelet/lipgloss"
)

// Terminal asks in the terminal. In the normal mode the checklist is a full-screen list and the
// other questions are huh forms. In accessible mode every question is a plain prompt read line by
// line, which works with screen readers and TERM=dumb.
type Terminal struct {
	in         io.Reader
	out        io.Writer
	accessible bool
	r          *lipgloss.Renderer
	lines      *bufio.Reader // the accessible checklist's reader, kept so no typed-ahead input is lost
	ctx        context.Context
}

// NewTerminal asks on in and out. Colours follow out and NO_COLOR.
func NewTerminal(in io.Reader, out io.Writer, accessible bool) *Terminal {
	return &Terminal{in: in, out: out, accessible: accessible, r: lipgloss.NewRenderer(out)}
}

// WithContext makes the questions end with ErrCancelled once ctx is done, also while one is
// waiting for a line. A nil ctx never ends them.
func (t *Terminal) WithContext(ctx context.Context) *Terminal {
	t.ctx = ctx
	return t
}

// stopped reports whether the context has ended the questions.
func (t *Terminal) stopped() bool { return t.ctx != nil && t.ctx.Err() != nil }

// wait runs read, which waits for the user's input, and gives up with ErrCancelled when the
// context ends first. The read it leaves waiting ends with carryon, and no question asked after
// it reads again.
func (t *Terminal) wait(read func() error) error {
	if t.ctx == nil {
		return read()
	}
	if t.stopped() {
		return ErrCancelled
	}
	done := make(chan error, 1)
	go func() { done <- read() }()
	select {
	case err := <-done:
		return err
	case <-t.ctx.Done():
		return ErrCancelled
	}
}

// Checklist shows the groups and lets the user tick them and open them to pick single items.
func (t *Terminal) Checklist(c Checklist) (Selection, error) {
	if len(c.Groups) == 0 {
		return Selection{}, nil
	}
	if t.stopped() {
		return Selection{}, ErrCancelled
	}
	if t.accessible {
		return t.plainChecklist(c)
	}
	m := newChecklistModel(c, newStyles(t.r))
	final, err := tea.NewProgram(m, tea.WithInput(t.in), tea.WithOutput(t.out), tea.WithAltScreen()).Run()
	if err != nil {
		if errors.Is(err, tea.ErrInterrupted) {
			return Selection{}, ErrCancelled
		}
		return Selection{}, fmt.Errorf("showing the checklist: %w", err)
	}
	m = final.(*checklistModel)
	if !m.done {
		return Selection{}, ErrCancelled
	}
	return m.selection(), nil
}

// Menu lets the user pick one option.
func (t *Terminal) Menu(m Menu) (string, error) {
	if len(m.Options) == 0 {
		return "", fmt.Errorf("%q has nothing to choose from", m.Title)
	}
	opts := make([]huh.Option[string], len(m.Options))
	for i, o := range m.Options {
		key := o.label()
		if o.Note != "" {
			key += "  " + o.Note
		}
		opts[i] = huh.NewOption(key, o.Value)
	}
	v := m.Default
	if v == "" {
		v = m.Options[0].Value
	}
	field := huh.NewSelect[string]().Title(m.Title).Description(m.Text).Options(opts...).Value(&v)
	if err := t.run(field, m.Text); err != nil {
		return "", err
	}
	return v, nil
}

// Confirm asks a yes/no question.
func (t *Terminal) Confirm(c Confirm) (bool, error) {
	v := c.Default
	field := huh.NewConfirm().Title(c.Title).Description(c.Text).Affirmative("Yes").Negative("No").Value(&v)
	if err := t.run(field, c.Text); err != nil {
		return false, err
	}
	return v, nil
}

// Input asks for a line of text, again until it passes Validate.
func (t *Terminal) Input(i Input) (string, error) {
	v := i.Default
	field := huh.NewInput().Title(i.Title).Description(i.Text).Placeholder(i.Placeholder).Value(&v).
		Validate(t.validator(i.Validate, i.Default))
	if err := t.run(field, i.Text); err != nil {
		return "", err
	}
	return v, checkFinal(v, i.Validate)
}

// Password asks for a secret without echoing it, twice when Twice is set.
func (t *Terminal) Password(p Password) (string, error) {
	for {
		var first, second string
		field := huh.NewInput().Title(p.Title).Description(p.Text).EchoMode(huh.EchoModePassword).
			Value(&first).Validate(t.validator(p.Validate, ""))
		if err := t.run(field, p.Text); err != nil {
			return "", err
		}
		if err := checkFinal(first, p.Validate); err != nil {
			return "", err
		}
		if !p.Twice {
			return first, nil
		}
		again := huh.NewInput().Title("Type it again").EchoMode(huh.EchoModePassword).Value(&second)
		if err := t.run(again, ""); err != nil {
			return "", err
		}
		if first == second {
			return first, nil
		}
		fmt.Fprintln(t.out, "The two don't match. Try again.")
	}
}

// run asks one huh field. text is the field's description, which huh's accessible mode doesn't
// print, so it's printed here first.
func (t *Terminal) run(f huh.Field, text string) error {
	if t.stopped() {
		return ErrCancelled
	}
	if t.accessible && text != "" {
		fmt.Fprintln(t.out, text)
	}
	theme := huh.ThemeCharm()
	if os.Getenv("NO_COLOR") != "" {
		theme = huh.ThemeBase()
	}
	form := huh.NewForm(huh.NewGroup(f)).WithTheme(theme).WithShowHelp(true).
		WithInput(t.in).WithOutput(t.out).WithAccessible(t.accessible)
	run := form.Run
	if t.accessible { // a full-screen form gets Ctrl+C as a key; a plain one reads lines
		run = func() error { return t.wait(form.Run) }
	}
	if err := run(); err != nil {
		if errors.Is(err, ErrCancelled) {
			return err
		}
		if errors.Is(err, huh.ErrUserAborted) {
			return ErrCancelled
		}
		return fmt.Errorf("asking: %w", err)
	}
	return nil
}

// validator adapts Validate for huh. In accessible mode huh checks the raw line before putting the
// default in for an empty one, so an empty line is accepted there when there's a default.
func (t *Terminal) validator(v func(string) error, def string) func(string) error {
	return func(s string) error {
		if v == nil || (t.accessible && strings.TrimSpace(s) == "" && def != "") {
			return nil
		}
		return v(s)
	}
}

// checkFinal checks the answer once more: huh's accessible mode returns the default, or an empty
// answer, when the input ends, without checking it.
func checkFinal(s string, v func(string) error) error {
	if v != nil && v(s) != nil {
		return ErrCancelled
	}
	return nil
}

// plainChecklist is the accessible checklist: a numbered list, and a prompt that takes numbers to
// tick or untick, "open N" to pick single items, "all", "none", Enter to continue and "q" to stop.
func (t *Terminal) plainChecklist(c Checklist) (Selection, error) {
	if t.lines == nil {
		t.lines = bufio.NewReader(t.in)
	}
	groups := newChecklistModel(c, styles{}).groups
	for {
		fmt.Fprintf(t.out, "\n%s\n", c.Title)
		if c.Text != "" {
			fmt.Fprintln(t.out, c.Text)
		}
		for n, g := range groups {
			mark := plainBox(g.On, g.On && unticked(g) > 0)
			line := fmt.Sprintf("%3d. %s %s", n+1, mark, g.Title)
			if len(g.Items) > 0 {
				line += fmt.Sprintf(" (%d of %d items)", len(g.Items)-unticked(g), len(g.Items))
			}
			fmt.Fprintln(t.out, line)
			if detail := joinNonEmpty(" · ", g.Summary, g.Reason); detail != "" {
				fmt.Fprintf(t.out, "       %s\n", detail)
			}
		}
		ans, err := t.ask(`Numbers to tick or untick (like "3 7"), "open N" to choose single items, "all", "none", Enter to continue, "q" to cancel: `)
		if err != nil {
			return Selection{}, err
		}
		switch f := strings.Fields(ans); {
		case len(f) == 0:
			return selectionOf(groups), nil
		case f[0] == "q":
			return Selection{}, ErrCancelled
		case f[0] == "all" || f[0] == "none":
			for i := range groups {
				groups[i].On = f[0] == "all"
			}
		case f[0] == "open" && len(f) == 2:
			n, err := strconv.Atoi(f[1])
			if err != nil || n < 1 || n > len(groups) || len(groups[n-1].Items) == 0 {
				fmt.Fprintf(t.out, "There's no group %s with items to choose from.\n", f[1])
				continue
			}
			if err := t.plainItems(&groups[n-1]); err != nil {
				return Selection{}, err
			}
		default:
			for _, tok := range f {
				if n, err := strconv.Atoi(tok); err == nil && n >= 1 && n <= len(groups) {
					groups[n-1].On = !groups[n-1].On
				}
			}
		}
	}
}

// plainItems is the accessible item picker for one group. "q" goes back without the changes.
func (t *Terminal) plainItems(g *Group) error {
	items := append([]Item(nil), g.Items...)
	for {
		fmt.Fprintf(t.out, "\n%s: untick what you don't want\n", g.Title)
		for n, it := range items {
			fmt.Fprintf(t.out, "%3d. %s %s\n", n+1, plainBox(it.On, false), it.Label)
		}
		ans, err := t.ask(`Numbers to tick or untick, "all", "none", Enter to go back, "q" to go back without changes: `)
		if err != nil {
			return err
		}
		switch f := strings.Fields(ans); {
		case len(f) == 0:
			g.Items = items
			if unticked(*g) < len(items) {
				g.On = true
			}
			return nil
		case f[0] == "q":
			return nil
		case f[0] == "all" || f[0] == "none":
			for i := range items {
				items[i].On = f[0] == "all"
			}
		default:
			for _, tok := range f {
				if n, err := strconv.Atoi(tok); err == nil && n >= 1 && n <= len(items) {
					items[n-1].On = !items[n-1].On
				}
			}
		}
	}
}

// ask prints a prompt and reads a line. The end of the input cancels, and so does the context.
func (t *Terminal) ask(prompt string) (string, error) {
	if t.stopped() {
		return "", ErrCancelled
	}
	fmt.Fprint(t.out, prompt)
	var line string
	err := t.wait(func() (err error) {
		line, err = t.lines.ReadString('\n')
		return err
	})
	if errors.Is(err, ErrCancelled) { // the read may still fill line: leave it alone
		fmt.Fprintln(t.out)
		return "", err
	}
	if err != nil && (line == "" || !errors.Is(err, io.EOF)) {
		fmt.Fprintln(t.out)
		return "", ErrCancelled
	}
	return strings.TrimSpace(line), nil
}

func plainBox(on, partial bool) string {
	switch {
	case partial:
		return "[-]"
	case on:
		return "[x]"
	}
	return "[ ]"
}

func joinNonEmpty(sep string, parts ...string) string {
	var out []string
	for _, p := range parts {
		if p != "" {
			out = append(out, p)
		}
	}
	return strings.Join(out, sep)
}
