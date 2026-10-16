package restore

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Evimore/carryon/internal/record"
	"github.com/Evimore/carryon/internal/rules"
	"github.com/Evimore/carryon/internal/ui"
)

func testRecord() *record.Record {
	return &record.Record{
		Meta: record.Meta{Format: 1, Host: "desktop", Machine: "0123456789abcdef", User: "alice", Home: "/home/alice"},
		Bundles: record.Bundles{
			{ID: "packages", Title: "Packages", When: "always"},
			{ID: "identity", Title: "Machine identity", When: "samehost"},
			{ID: "laptop", Title: "Laptop tweaks", When: "chassis:laptop"},
			{ID: "odd", Title: "Odd rule", When: "moon:full"},
		},
		Items: []record.Item{
			{Type: "pkg", ID: "base", Bundle: "packages"},
			{Type: "pkg", ID: "htop", Bundle: "packages"},
			{Type: "path", ID: "/etc/ssh/ssh_host_*", Bundle: "identity"},
			{Type: "file", ID: "/etc/tlp.conf", Bundle: "laptop"},
		},
	}
}

func TestDefaultTicks(t *testing.T) {
	rec := testRecord()
	for _, tt := range []struct {
		name string
		here rules.Facts
		want []string
	}{
		{"same machine by fingerprint, renamed", rules.Facts{Host: "renamed", Machine: "0123456789abcdef", Chassis: "desktop"},
			[]string{"packages on", "identity on same machine", "laptop off laptops only", "odd on"}},
		{"same name, other hardware", rules.Facts{Host: "desktop", Machine: "fedcba9876543210", Chassis: "laptop"},
			[]string{"packages on", "identity off specific to desktop (this is a different machine)", "laptop on", "odd on"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ticks, warnings := DefaultTicks(rec, tt.here)
			var got []string
			for _, tk := range ticks {
				s := tk.Bundle.ID + map[bool]string{true: " on", false: " off"}[tk.On]
				if tk.Reason != "" {
					s += " " + tk.Reason
				}
				got = append(got, s)
			}
			if strings.Join(got, "|") != strings.Join(tt.want, "|") {
				t.Errorf("ticks = %q\nwant    %q", got, tt.want)
			}
			if len(warnings) != 1 || !strings.Contains(warnings[0], `unknown when-rule "moon:full"`) {
				t.Errorf("warnings = %q", warnings)
			}
			if len(ticks[0].Items) != 2 {
				t.Errorf("packages has %d items", len(ticks[0].Items))
			}
		})
	}
}

func TestPick(t *testing.T) {
	ticks, _ := DefaultTicks(testRecord(), rules.Facts{Host: "laptop", Chassis: "desktop"})

	// Without earlier choices: one checklist, starting from the default ticks.
	var shown ui.Checklist
	ask := &checklistPrompter{answer: func(c ui.Checklist) ui.Selection {
		shown = c
		return ui.Selection{Groups: []string{"identity", "packages"}, Unticked: []string{"pkg:htop"}}
	}}
	p, err := Pick(ask, ticks, nil, "desktop", "laptop")
	if err != nil {
		t.Fatal(err)
	}
	if shown.Title != "Restore from desktop onto laptop" || shown.Flag != "--defaults" || len(shown.Groups) != 4 {
		t.Errorf("checklist = %+v", shown)
	}
	g := shown.Groups[1]
	if g.On || g.Reason != "specific to desktop (this is a different machine)" || g.Summary != "1 item" || len(g.Items) != 0 {
		t.Errorf("identity = %+v", g)
	}
	if g := shown.Groups[0]; !g.On || g.Reason != "" || len(g.Items) != 2 || g.Items[1].ID != "pkg:htop" || !g.Items[1].On {
		t.Errorf("packages = %+v", g)
	}
	if !slices.Equal(p.Bundles, []string{"packages", "identity"}) || !slices.Equal(p.Deselected, []string{"pkg:htop"}) {
		t.Errorf("picked = %+v", p)
	}

	// With earlier choices the user accepts: they set the ticks.
	prev := &Choices{Date: "2026-10-01T10:00:00+02:00", Bundles: []string{"laptop"}, Deselected: []string{"pkg:htop"}}
	script := &ui.Script{Answers: []any{true, ui.UseDefault}}
	p, err = Pick(script, ticks, prev, "desktop", "laptop")
	if err != nil {
		t.Fatal(err)
	}
	if script.Asked[0].Title != "Start from the choices you made on 2026-10-01?" {
		t.Errorf("asked %+v", script.Asked)
	}
	if !slices.Equal(p.Bundles, []string{"laptop"}) || len(p.Deselected) != 0 {
		t.Errorf("picked = %+v", p) // htop's bundle isn't ticked, so it isn't listed
	}

	// Declined: the default ticks.
	script = &ui.Script{Answers: []any{false, ui.UseDefault}}
	if p, err = Pick(script, ticks, prev, "desktop", "laptop"); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(p.Bundles, DefaultPick(ticks).Bundles) || len(p.Deselected) != 0 {
		t.Errorf("picked = %+v", p)
	}

	// Cancelled.
	script = &ui.Script{Answers: []any{ui.ErrCancelled}}
	if _, err = Pick(script, ticks, nil, "desktop", "laptop"); err == nil || !strings.Contains(err.Error(), "nothing was changed") {
		t.Errorf("err = %v", err)
	}
}

// checklistPrompter answers checklists with a function.
type checklistPrompter struct {
	ui.Defaults
	answer func(ui.Checklist) ui.Selection
}

func (c *checklistPrompter) Checklist(cl ui.Checklist) (ui.Selection, error) {
	return c.answer(cl), nil
}

func TestChoicesRoundTrip(t *testing.T) {
	state := t.TempDir()
	if LoadChoices(state, "desktop") != nil {
		t.Fatal("choices before any were saved")
	}
	p := Picked{Bundles: []string{"packages", "account"}, Deselected: []string{"pkg:htop", "group:wheel@bob"}}
	now := time.Date(2026, 10, 14, 9, 30, 0, 0, time.UTC)
	if err := SaveChoices(state, "desktop", p.Choices("laptop", now)); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(state, "selections", "desktop.json"))
	if err != nil {
		t.Fatal(err)
	}
	want := `{
  "date": "2026-10-14T09:30:00Z",
  "target": "laptop",
  "bundles": [
    "account",
    "packages"
  ],
  "deselected": [
    "group:wheel@bob",
    "pkg:htop"
  ]
}
`
	if string(b) != want {
		t.Errorf("file =\n%s", b)
	}
	c := LoadChoices(state, "desktop")
	if c == nil || c.Target != "laptop" || len(c.Bundles) != 2 {
		t.Errorf("loaded %+v", c)
	}
	if !slices.Equal(p.Bundles, []string{"packages", "account"}) {
		t.Error("saving sorted the caller's pick")
	}
	// A broken file counts as no choices.
	writeFile(t, filepath.Join(state, "selections", "laptop.json"), "{")
	if LoadChoices(state, "laptop") != nil {
		t.Error("a broken file gave choices")
	}
}
