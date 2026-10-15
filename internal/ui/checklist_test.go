package ui

import (
	"bytes"
	"reflect"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

func sampleChecklist() Checklist {
	return Checklist{
		Title: "Restore from desktop onto laptop",
		Text:  "Unticked bundles are skipped. Nothing changes until you confirm the plan.",
		Groups: []Group{
			{ID: "base", Title: "Base system", Summary: "12 packages", Reason: "same distribution", On: true},
			{ID: "app-obs-studio", Title: "OBS Studio", Summary: "1 package, 2 settings files", On: true, Items: []Item{
				{ID: "pkg:obs-studio", Label: "obs-studio", On: true},
				{ID: "/home/alice/.config/obs-studio/basic.ini", Label: "~/.config/obs-studio/basic.ini", On: true},
				{ID: "/home/alice/.config/obs-studio/global.ini", Label: "~/.config/obs-studio/global.ini", On: true},
			}},
			{ID: "hw-nvidia", Title: "NVIDIA driver", Summary: "3 packages", Reason: "needs hardware this machine doesn't have"},
			{ID: "identity", Title: "Machine identity", Summary: "hostname, machine-id", Reason: "not the same machine"},
		},
	}
}

// keyMsg turns a key name as Bubble Tea prints it ("down", "enter", "a") into a key message.
func keyMsg(k string) tea.KeyMsg {
	named := map[string]tea.KeyType{
		"up": tea.KeyUp, "down": tea.KeyDown, "left": tea.KeyLeft, "right": tea.KeyRight,
		"enter": tea.KeyEnter, "esc": tea.KeyEsc, "space": tea.KeySpace, "ctrl+c": tea.KeyCtrlC,
		"backspace": tea.KeyBackspace, "tab": tea.KeyTab,
	}
	if t, ok := named[k]; ok {
		return tea.KeyMsg{Type: t}
	}
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(k)}
}

func plainStyles() styles {
	// A renderer for something that isn't a terminal draws without colour.
	return newStyles(lipgloss.NewRenderer(&bytes.Buffer{}))
}

// drive sends keys to a fresh checklist model and returns it.
func drive(t *testing.T, c Checklist, keys ...string) *checklistModel {
	t.Helper()
	m := newChecklistModel(c, plainStyles())
	m.Update(tea.WindowSizeMsg{Width: 80, Height: 30})
	for _, k := range keys {
		if m.done || m.cancelled {
			t.Fatalf("key %q sent after the checklist closed", k)
		}
		var cmd tea.Cmd
		_, cmd = m.Update(keyMsg(k))
		_ = cmd
		_ = m.View() // every state must draw without panicking
	}
	return m
}

func TestChecklistKeys(t *testing.T) {
	def := sampleChecklist().Defaults()
	cases := []struct {
		name      string
		keys      []string
		want      Selection
		cancelled bool
	}{
		{"enter keeps the defaults", []string{"enter"}, def, false},
		{"space unticks the first group", []string{"space", "enter"},
			Selection{Groups: []string{"app-obs-studio"}}, false},
		{"move down and tick", []string{"down", "down", "x", "enter"},
			Selection{Groups: []string{"base", "app-obs-studio", "hw-nvidia"}}, false},
		{"a ticks everything", []string{"a", "enter"},
			Selection{Groups: []string{"base", "app-obs-studio", "hw-nvidia", "identity"}}, false},
		{"n unticks everything", []string{"n", "enter"}, Selection{}, false},
		{"search limits tick-all to matches", []string{"/", "n", "v", "i", "enter", "a", "enter"},
			Selection{Groups: []string{"base", "app-obs-studio", "hw-nvidia"}}, false},
		{"esc clears the search first", []string{"/", "o", "b", "s", "enter", "esc", "a", "enter"},
			Selection{Groups: []string{"base", "app-obs-studio", "hw-nvidia", "identity"}}, false},
		{"open a group and untick one item", []string{"down", "right", "down", "space", "enter", "enter"},
			Selection{Groups: []string{"base", "app-obs-studio"}, Unticked: []string{"/home/alice/.config/obs-studio/basic.ini"}}, false},
		{"esc inside a group undoes its changes", []string{"down", "right", "n", "esc", "enter"}, def, false},
		{"left inside a group keeps its changes", []string{"down", "o", "n", "left", "enter"},
			Selection{Groups: []string{"base", "app-obs-studio"}, Unticked: []string{
				"pkg:obs-studio", "/home/alice/.config/obs-studio/basic.ini", "/home/alice/.config/obs-studio/global.ini"}}, false},
		{"ticking an item ticks its group", []string{"down", "space", "right", "down", "space", "space", "enter", "enter"},
			Selection{Groups: []string{"base", "app-obs-studio"}}, false},
		{"opening a group without items does nothing", []string{"right", "space", "enter"},
			Selection{Groups: []string{"app-obs-studio"}}, false},
		{"esc cancels", []string{"esc"}, Selection{}, true},
		{"q cancels", []string{"q"}, Selection{}, true},
		{"ctrl+c cancels inside a group", []string{"down", "right", "ctrl+c"}, Selection{}, true},
		{"the cursor stops at the ends", []string{"up", "up", "space", "end", "down", "space", "enter"},
			Selection{Groups: []string{"app-obs-studio", "identity"}}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := drive(t, sampleChecklist(), tc.keys...)
			if m.cancelled != tc.cancelled || m.done == tc.cancelled {
				t.Fatalf("done=%v cancelled=%v, want cancelled=%v", m.done, m.cancelled, tc.cancelled)
			}
			if tc.cancelled {
				return
			}
			if got := m.selection(); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %+v\nwant %+v", got, tc.want)
			}
		})
	}
}

func TestChecklistDoesNotChangeTheCallersGroups(t *testing.T) {
	c := sampleChecklist()
	drive(t, c, "n", "down", "right", "n", "left", "enter")
	if !reflect.DeepEqual(c, sampleChecklist()) {
		t.Fatal("the checklist changed the caller's groups")
	}
}

func TestChecklistView(t *testing.T) {
	m := drive(t, sampleChecklist(), "down", "right", "down", "space", "left")
	v := m.View()
	for _, want := range []string{
		"Restore from desktop onto laptop",
		"[x] Base system",
		"same distribution",
		"> [-] OBS Studio (2 of 3)",
		"[ ] NVIDIA driver",
		"needs hardware this machine doesn't have",
		"enter continue",
	} {
		if !strings.Contains(v, want) {
			t.Errorf("view lacks %q:\n%s", want, v)
		}
	}
	if strings.Contains(v, "\x1b[") {
		t.Errorf("view has colour codes with the plain profile:\n%q", v)
	}
}

func TestChecklistItemView(t *testing.T) {
	v := drive(t, sampleChecklist(), "down", "right").View()
	for _, want := range []string{"OBS Studio", "Untick what you don't want.", "> [x] obs-studio", "esc undo"} {
		if !strings.Contains(v, want) {
			t.Errorf("view lacks %q:\n%s", want, v)
		}
	}
}

func TestChecklistFitsSmallTerminals(t *testing.T) {
	c := sampleChecklist()
	for i := range 40 {
		c.Groups = append(c.Groups, Group{ID: strings.Repeat("x", i+1), Title: "A group with a long title that goes on " + strings.Repeat("and on ", 10), Summary: "summary", On: true})
	}
	for _, size := range [][2]int{{80, 24}, {40, 10}, {20, 5}} {
		m := newChecklistModel(c, plainStyles())
		m.Update(tea.WindowSizeMsg{Width: size[0], Height: size[1]})
		for range 30 {
			m.Update(keyMsg("down"))
		}
		v := m.View()
		lines := strings.Split(strings.TrimRight(v, "\n"), "\n")
		if len(lines) > size[1] {
			t.Errorf("%dx%d: %d lines:\n%s", size[0], size[1], len(lines), v)
		}
		for _, l := range lines {
			if w := lipgloss.Width(l); w > size[0] {
				t.Errorf("%dx%d: line is %d wide: %q", size[0], size[1], w, l)
			}
		}
		if !strings.Contains(v, "> [x]") {
			t.Errorf("%dx%d: the cursor isn't on screen:\n%s", size[0], size[1], v)
		}
	}
}

func TestChecklistKeepsTheReasonWhole(t *testing.T) {
	c := sampleChecklist()
	c.Groups[2].Summary = "nvidia-dkms, nvidia-utils, nvidia-settings, modprobe.d/nvidia.conf"
	m := newChecklistModel(c, plainStyles())
	m.Update(tea.WindowSizeMsg{Width: 72, Height: 30})
	if v := m.View(); !strings.Contains(v, "      needs hardware this machine doesn't have\n") {
		t.Errorf("the reason isn't on a line of its own:\n%s", v)
	}
}
