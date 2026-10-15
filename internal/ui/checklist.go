package ui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

// checklistModel is the Bubble Tea model behind Terminal.Checklist. It shows the groups; opening
// one shows its items (the item picker). Both views can be searched.
type checklistModel struct {
	spec   Checklist
	groups []Group // the working copy; ticks change here

	open    int    // index of the open group, or -1 for the group list
	saved   []Item // the open group's items as they were when opened, for Esc
	savedOn bool   // the open group's tick when it was opened

	cursor int // index into visible()
	offset int // first visible entry drawn, for scrolling
	filter textinput.Model
	typing bool // keys go to the search box

	width, height int
	st            styles

	done, cancelled bool
}

type styles struct {
	title, text, cursor, on, off, dim, reason, warn, help lipgloss.Style
}

func newStyles(r *lipgloss.Renderer) styles {
	s := r.NewStyle
	return styles{
		title:  s().Bold(true),
		text:   s().Faint(true),
		cursor: s().Bold(true).Foreground(lipgloss.AdaptiveColor{Light: "4", Dark: "12"}),
		on:     s().Foreground(lipgloss.AdaptiveColor{Light: "2", Dark: "10"}),
		off:    s(),
		dim:    s().Faint(true),
		reason: s().Faint(true).Italic(true),
		warn:   s().Foreground(lipgloss.AdaptiveColor{Light: "3", Dark: "11"}),
		help:   s().Faint(true),
	}
}

func newChecklistModel(c Checklist, st styles) *checklistModel {
	groups := make([]Group, len(c.Groups))
	for i, g := range c.Groups {
		g.Items = append([]Item(nil), g.Items...)
		groups[i] = g
	}
	f := textinput.New()
	f.Prompt = "Search: "
	f.Placeholder = "type to filter"
	return &checklistModel{spec: c, groups: groups, open: -1, filter: f, width: 80, height: 24, st: st}
}

func (m *checklistModel) Init() tea.Cmd { return nil }

// selection is what's ticked now.
func (m *checklistModel) selection() Selection { return selectionOf(m.groups) }

// visible returns the indices (into groups, or into the open group's items) that match the search.
func (m *checklistModel) visible() []int {
	q := strings.ToLower(strings.TrimSpace(m.filter.Value()))
	var out []int
	if m.open < 0 {
		for i, g := range m.groups {
			if q == "" || strings.Contains(strings.ToLower(g.Title+"\n"+g.Summary+"\n"+g.ID), q) {
				out = append(out, i)
			}
		}
		return out
	}
	for i, it := range m.groups[m.open].Items {
		if q == "" || strings.Contains(strings.ToLower(it.Label+"\n"+it.ID), q) {
			out = append(out, i)
		}
	}
	return out
}

func (m *checklistModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		return m, nil
	case tea.KeyMsg:
		return m.key(msg)
	}
	return m, nil
}

func (m *checklistModel) key(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	if k.String() == "ctrl+c" {
		m.cancelled = true
		return m, tea.Quit
	}
	if m.typing {
		switch k.String() {
		case "enter", "down", "up":
			m.typing = false
			m.filter.Blur()
			if k.String() != "enter" {
				return m.key(k)
			}
		case "esc":
			m.clearFilter()
		default:
			var cmd tea.Cmd
			m.filter, cmd = m.filter.Update(k)
			m.cursor, m.offset = 0, 0
			return m, cmd
		}
		return m, nil
	}

	vis := m.visible()
	switch k.String() {
	case "up", "k":
		m.move(-1, len(vis))
	case "down", "j":
		m.move(1, len(vis))
	case "pgup":
		m.move(-m.pageSize(), len(vis))
	case "pgdown":
		m.move(m.pageSize(), len(vis))
	case "home", "g":
		m.cursor = 0
	case "end", "G":
		m.cursor = max(0, len(vis)-1)
	case " ", "x":
		if len(vis) > 0 {
			m.toggle(vis[m.cursor])
		}
	case "a":
		m.setAll(vis, true)
	case "n":
		m.setAll(vis, false)
	case "/":
		m.typing = true
		return m, m.filter.Focus()
	case "right", "l", "o", "tab":
		if m.open < 0 && len(vis) > 0 && len(m.groups[vis[m.cursor]].Items) > 0 {
			m.openGroup(vis[m.cursor])
		}
	case "left", "h", "backspace":
		if m.open >= 0 {
			m.closeGroup(false)
		}
	case "enter":
		if m.open >= 0 {
			m.closeGroup(false)
			return m, nil
		}
		m.done = true
		return m, tea.Quit
	case "esc", "q":
		switch {
		case m.filter.Value() != "":
			m.clearFilter()
		case m.open >= 0:
			m.closeGroup(true)
		default:
			m.cancelled = true
			return m, tea.Quit
		}
	}
	return m, nil
}

func (m *checklistModel) move(by, n int) {
	m.cursor = min(max(m.cursor+by, 0), max(n-1, 0))
}

func (m *checklistModel) toggle(i int) {
	if m.open < 0 {
		m.groups[i].On = !m.groups[i].On
		return
	}
	g := &m.groups[m.open]
	g.Items[i].On = !g.Items[i].On
	if g.Items[i].On {
		g.On = true // ticking something inside a group means the group is wanted
	}
}

func (m *checklistModel) setAll(vis []int, on bool) {
	for _, i := range vis {
		if m.open < 0 {
			m.groups[i].On = on
		} else {
			m.groups[m.open].Items[i].On = on
		}
	}
	if m.open >= 0 && on && len(vis) > 0 {
		m.groups[m.open].On = true
	}
}

func (m *checklistModel) openGroup(i int) {
	m.open = i
	m.saved = append([]Item(nil), m.groups[i].Items...)
	m.savedOn = m.groups[i].On
	m.clearFilter()
	m.cursor, m.offset = 0, 0
}

// closeGroup goes back to the group list, undoing changes made inside the group when undo is set.
func (m *checklistModel) closeGroup(undo bool) {
	i := m.open
	if undo {
		m.groups[i].Items = m.saved
		m.groups[i].On = m.savedOn
	}
	m.open, m.saved = -1, nil
	m.clearFilter()
	// Put the cursor back on the group that was open.
	m.cursor, m.offset = 0, 0
	for n, v := range m.visible() {
		if v == i {
			m.cursor = n
		}
	}
}

func (m *checklistModel) clearFilter() {
	m.filter.SetValue("")
	m.filter.Blur()
	m.typing = false
	m.cursor, m.offset = 0, 0
}

// compact is a one-line-per-entry layout for small terminals.
func (m *checklistModel) compact() bool { return m.height < 14 || m.width < 50 }

func (m *checklistModel) pageSize() int { return max(1, m.height/3) }

func (m *checklistModel) View() string {
	if m.done || m.cancelled {
		return ""
	}
	w := max(m.width, 20)
	var head []string
	if m.open < 0 {
		head = append(head, fit(m.st.title.Render(m.spec.Title), w))
		if m.spec.Text != "" && !m.compact() {
			head = append(head, m.st.text.Width(w).Render(m.spec.Text))
		}
	} else {
		g := m.groups[m.open]
		head = append(head, fit(m.st.title.Render(g.Title), w))
		if !m.compact() {
			head = append(head, m.st.text.Width(w).Render("Untick what you don't want."))
		}
	}
	if m.typing || m.filter.Value() != "" {
		head = append(head, fit(m.filter.View(), w))
	}

	help := m.helpLine()
	vis := m.visible()
	blocks := make([][]string, len(vis))
	for n, i := range vis {
		blocks[n] = m.entry(i, n == m.cursor, w)
	}

	avail := max(1, m.height-lineCount(head)-lineCount([]string{help})-2)
	m.scroll(blocks, avail)
	var body []string
	used, shown := 0, 0
	for n := m.offset; n < len(blocks); n++ {
		if used+len(blocks[n]) > avail && n > m.offset {
			break
		}
		body = append(body, blocks[n]...)
		used += len(blocks[n])
		shown++
	}
	if len(vis) == 0 {
		body = append(body, m.st.dim.Render("  Nothing matches."))
	}
	more := ""
	if shown < len(vis) {
		more = m.st.dim.Render(fmt.Sprintf("  %d–%d of %d", m.offset+1, m.offset+shown, len(vis)))
	}

	out := strings.Join(head, "\n") + "\n\n" + strings.Join(body, "\n") + "\n"
	if more != "" {
		out += more + "\n"
	}
	return out + help
}

// scroll moves the offset so the block under the cursor is fully on screen.
func (m *checklistModel) scroll(blocks [][]string, avail int) {
	if m.cursor >= len(blocks) {
		m.cursor = max(0, len(blocks)-1)
	}
	if m.cursor < m.offset {
		m.offset = m.cursor
	}
	for m.offset < m.cursor {
		used := 0
		for n := m.offset; n <= m.cursor; n++ {
			used += len(blocks[n])
		}
		if used <= avail {
			break
		}
		m.offset++
	}
}

func lineCount(parts []string) int {
	n := 0
	for _, p := range parts {
		n += strings.Count(p, "\n") + 1
	}
	return n
}

// entry draws one group or item: a line with the tick box and title, and in the full layout a
// second line with what's inside and why it's ticked or not.
func (m *checklistModel) entry(i int, here bool, w int) []string {
	pointer := "  "
	if here {
		pointer = m.st.cursor.Render("> ")
	}
	if m.open >= 0 {
		it := m.groups[m.open].Items[i]
		return []string{fit(pointer+m.box(it.On, false)+" "+m.name(it.Label, here), w)}
	}
	g := m.groups[i]
	partial := g.On && unticked(g) > 0
	title := m.name(g.Title, here)
	if n := len(g.Items); n > 0 {
		count := fmt.Sprintf("(%d)", n)
		if partial {
			count = fmt.Sprintf("(%d of %d)", n-unticked(g), n)
		}
		title += " " + m.st.dim.Render(count)
	}
	lines := []string{fit(pointer+m.box(g.On, partial)+" "+title, w)}
	if m.compact() {
		return lines
	}
	var detail []string
	if g.Summary != "" {
		detail = append(detail, m.st.dim.Render(g.Summary))
	}
	if g.Reason != "" {
		// A reason for leaving something unticked is worth noticing; one for ticking it isn't.
		style := m.st.reason
		if !g.On {
			style = m.st.warn
		}
		detail = append(detail, style.Render(g.Reason))
	}
	const indent = "      "
	if joined := indent + strings.Join(detail, m.st.dim.Render(" · ")); len(detail) > 0 && lipgloss.Width(joined) <= w {
		lines = append(lines, joined)
	} else {
		// Too long for one line: the reason gets its own, so it isn't the part cut off.
		for _, d := range detail {
			lines = append(lines, fit(indent+d, w))
		}
	}
	return lines
}

func (m *checklistModel) name(s string, here bool) string {
	if here {
		return m.st.cursor.Render(s)
	}
	return s
}

// box draws the tick box. It's readable without colour: [x] ticked, [-] partly, [ ] not.
func (m *checklistModel) box(on, partial bool) string {
	switch {
	case partial:
		return m.st.on.Render("[-]")
	case on:
		return m.st.on.Render("[x]")
	}
	return m.st.off.Render("[ ]")
}

func (m *checklistModel) helpLine() string {
	var keys []string
	switch {
	case m.typing:
		keys = []string{"type to search", "enter keep", "esc clear"}
	case m.open >= 0:
		keys = []string{"space tick", "a all", "n none", "/ search", "enter done", "esc undo"}
	default:
		keys = []string{"space tick", "a all", "n none", "/ search", "→ open", "enter continue", "esc cancel"}
	}
	return fit(m.st.help.Render(strings.Join(keys, " · ")), max(m.width, 20))
}

func unticked(g Group) int {
	n := 0
	for _, it := range g.Items {
		if !it.On {
			n++
		}
	}
	return n
}

// fit cuts a line to the terminal width, keeping colour codes intact.
func fit(s string, w int) string { return ansi.Truncate(s, w, "…") }
